// Package conversations owns the conversation bounded context: the continuous
// customer relationship across channels. Interactions are created inside
// conversations (auto-opened by the interaction service when absent).
package conversations

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Roy-Wanyoike/orvexa/internal/platform/db/dbinternal"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
	"github.com/Roy-Wanyoike/orvexa/pkg/events"
	"github.com/Roy-Wanyoike/orvexa/pkg/pagination"
)

// Conversation is the continuous customer relationship.
type Conversation struct {
	ID                string     `json:"id"`
	TenantID          string     `json:"tenant_id"`
	CustomerID        string     `json:"customer_id"`
	Channel           string     `json:"channel"`
	Status            string     `json:"status"`
	Subject           string     `json:"subject,omitempty"`
	AssignedAgentID   string     `json:"assigned_agent_id,omitempty"`
	AssignedQueueID   string     `json:"assigned_queue_id,omitempty"`
	UnreadCount       int        `json:"unread_count"`
	LastInteractionAt *time.Time `json:"last_interaction_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	ClosedAt          *time.Time `json:"closed_at,omitempty"`
}

// Writer persists events inside caller transactions (transactional outbox).
type Writer struct{ w *outboxCore }

type outboxCore interface {
	Insert(ctx context.Context, tx pgx.Tx, env *events.Envelope) error
}

// Service implements conversation use-cases.
type Service struct {
	pool   *pgxpool.Pool
	writer outboxCore
	source string
}

// NewService wires the conversation service.
func NewService(pool *pgxpool.Pool, writer outboxCore, source string) *Service {
	return &Service{pool: pool, writer: writer, source: source}
}

// Close ends a conversation (terminal, idempotent-safe: closing twice 409s).
func (s *Service) Close(ctx context.Context, tenantID, id, reason string) (*Conversation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, apperrors.Internal("db.tx_failed", "write failed").WithCause(err)
	}
	defer tx.Rollback(context.Background())

	row := tx.QueryRow(ctx, `SELECT status FROM conversations WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, id, tenantID)
	var status string
	if err := row.Scan(&status); err != nil {
		if err == pgx.ErrNoRows {
			return nil, apperrors.NotFound("conversation.not_found", "conversation not found")
		}
		return nil, apperrors.Internal("db.read_failed", "read failed").WithCause(err)
	}
	if status == "closed" {
		return nil, apperrors.Conflict("conversation.already_closed", "conversation is already closed")
	}

	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, `
		UPDATE conversations SET status='closed', closed_at=$3, updated_at=now()
		WHERE id=$1 AND tenant_id=$2`, id, tenantID, now); err != nil {
		return nil, apperrors.Internal("db.write_failed", "write failed").WithCause(err)
	}
	env, _ := events.New(events.TopicConversationClosed, s.source, id, tenantID, "", map[string]any{
		"conversation_id": id, "reason": reason,
	})
	if err := s.writer.Insert(ctx, tx, env); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, apperrors.Internal("db.commit_failed", "write failed").WithCause(err)
	}
	return s.Get(ctx, tenantID, id)
}

// Assign routes a conversation to an agent/queue.
func (s *Service) Assign(ctx context.Context, tenantID, id, agentID, queueID string) (*Conversation, error) {
	if agentID == "" && queueID == "" {
		return nil, apperrors.Invalid("conversation.assign_target_required", "agent_id or queue_id required")
	}
	res, err := s.pool.Exec(ctx, `
		UPDATE conversations SET
			assigned_agent_id = coalesce($3::uuid, assigned_agent_id),
			assigned_queue_id = coalesce($4::uuid, assigned_queue_id),
			updated_at = now()
		WHERE id = $1 AND tenant_id = $2 AND status = 'open'`,
		id, tenantID, nullUUID(agentID), nullUUID(queueID))
	if err != nil {
		return nil, apperrors.Internal("db.write_failed", "write failed").WithCause(err)
	}
	if res.RowsAffected() == 0 {
		if _, gerr := s.Get(ctx, tenantID, id); gerr != nil {
			return nil, gerr
		}
		return nil, apperrors.Conflict("conversation.not_assignable", "conversation is closed")
	}
	return s.Get(ctx, tenantID, id)
}

// Get returns one tenant-scoped conversation (foreign ids → 404).
func (s *Service) Get(ctx context.Context, tenantID, id string) (*Conversation, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, tenant_id, customer_id, channel, status, coalesce(subject,''),
			coalesce(assigned_agent_id::text,''), coalesce(assigned_queue_id::text,''),
			unread_count, last_interaction_at, created_at, updated_at, closed_at
		FROM conversations WHERE id = $1 AND tenant_id = $2`, id, tenantID)
	c := &Conversation{}
	if err := row.Scan(&c.ID, &c.TenantID, &c.CustomerID, &c.Channel, &c.Status,
		&c.Subject, &c.AssignedAgentID, &c.AssignedQueueID, &c.UnreadCount,
		&c.LastInteractionAt, &c.CreatedAt, &c.UpdatedAt, &c.ClosedAt); err != nil {
		if err == pgx.ErrNoRows {
			return nil, apperrors.NotFound("conversation.not_found", "conversation not found")
		}
		return nil, apperrors.Internal("db.read_failed", "read failed").WithCause(err)
	}
	return c, nil
}

// List returns bounded pages, most recently updated first (supervisor views).
func (s *Service) List(ctx context.Context, tenantID, status string, page pagination.Page) ([]Conversation, string, error) {
	where := "tenant_id = $1"
	args := []any{tenantID}
	if status != "" {
		switch status {
		case "open", "closed":
			args = append(args, status)
			where += " AND status = $2"
		default:
			return nil, "", apperrors.Invalid("conversation.invalid_status", "status must be open|closed")
		}
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, customer_id, channel, status, coalesce(subject,''),
			coalesce(assigned_agent_id::text,''), coalesce(assigned_queue_id::text,''),
			unread_count, last_interaction_at, created_at, updated_at, closed_at
		FROM conversations WHERE `+where+`
		ORDER BY updated_at DESC, id
		LIMIT $`+itoa(len(args)+1)+` OFFSET $`+itoa(len(args)+2),
		append(args, page.Limit+1, page.Offset)...)
	if err != nil {
		return nil, "", apperrors.Internal("db.read_failed", "read failed").WithCause(err)
	}
	defer rows.Close()
	out := []Conversation{}
	for rows.Next() {
		var c Conversation
		if err := rows.Scan(&c.ID, &c.CustomerID, &c.Channel, &c.Status, &c.Subject,
			&c.AssignedAgentID, &c.AssignedQueueID, &c.UnreadCount, &c.LastInteractionAt,
			&c.CreatedAt, &c.UpdatedAt, &c.ClosedAt); err != nil {
			return nil, "", apperrors.Internal("db.scan_failed", "read failed").WithCause(err)
		}
		out = append(out, c)
	}
	hasMore := len(out) > page.Limit
	if hasMore {
		out = out[:page.Limit]
	}
	var cursor string
	if hasMore {
		cursor = page.NextCursor(page.Limit)
	}
	return out, cursor, nil
}

func nullUUID(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func itoa(n int) string {
	return fmtInt(n)
}

func fmtInt(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

var _ = uuid.New // keep uuid import if service grows (create flows live in interactions)
var _ = dbinternal.IsUniqueViolation
