// Package cases owns the case bounded context. Cases are independent from
// interactions: one case can span WhatsApp, calls, emails and notes. The
// lifecycle is guarded; ref numbering is serialized per tenant.
package cases

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Roy-Wanyoike/orvexa/internal/platform/db/dbinternal"
	"github.com/Roy-Wanyoike/orvexa/internal/platform/outbox"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
	"github.com/Roy-Wanyoike/orvexa/pkg/events"
	"github.com/Roy-Wanyoike/orvexa/pkg/pagination"
)

// Status is the case lifecycle.
type Status string

const (
	StatusOpen             Status = "open"
	StatusInProgress       Status = "in_progress"
	StatusPendingCustomer  Status = "pending_customer"
	StatusResolved         Status = "resolved"
	StatusClosed           Status = "closed"
	StatusCanceled         Status = "canceled"
)

// transitions encodes the guarded lifecycle.
var transitions = map[Status][]Status{
	StatusOpen:            {StatusInProgress, StatusResolved, StatusCanceled},
	StatusInProgress:      {StatusPendingCustomer, StatusResolved, StatusCanceled},
	StatusPendingCustomer: {StatusInProgress, StatusResolved, StatusCanceled},
	StatusResolved:        {StatusClosed, StatusInProgress}, // reopen path
	StatusClosed:          {},
	StatusCanceled:        {},
}

// CanTransition reports whether from→to is legal.
func CanTransition(from, to Status) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Case is the aggregate root.
type Case struct {
	ID              string         `json:"id"`
	TenantID        string         `json:"tenant_id"`
	CustomerID      string         `json:"customer_id"`
	Ref             string         `json:"ref"`
	Subject         string         `json:"subject"`
	Description     string         `json:"description,omitempty"`
	Status          Status         `json:"status"`
	Priority        string         `json:"priority"`
	AssignedAgentID string         `json:"assigned_agent_id,omitempty"`
	ResolvedAt      *time.Time     `json:"resolved_at,omitempty"`
	ClosedAt        *time.Time     `json:"closed_at,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
}

// Note is an internal or external case comment.
type Note struct {
	ID         string    `json:"id"`
	CaseID     string    `json:"case_id"`
	AuthorType string    `json:"author_type"`
	AuthorID   string    `json:"author_id"`
	Body       string    `json:"body"`
	Internal   bool      `json:"internal"`
	CreatedAt  time.Time `json:"created_at"`
}

// outboxAdapter narrows the platform writer to what cases needs.
type outboxAdapter struct{ w *outbox.Writer }

func (a outboxAdapter) insertTx(ctx context.Context, tx pgx.Tx, env *events.Envelope) error {
	return a.w.Insert(ctx, tx, env)
}

// Service implements case use-cases.
type Service struct {
	pool   *pgxpool.Pool
	writer outboxAdapter
	source string
}

// NewService wires the case service with the transactional outbox writer.
func NewService(pool *pgxpool.Pool, w *outbox.Writer, source string) *Service {
	return &Service{pool: pool, writer: outboxAdapter{w: w}, source: source}
}

type CreateInput struct {
	CustomerID  string `json:"customer_id"`
	Subject     string `json:"subject"`
	Description string `json:"description"`
	Priority    string `json:"priority"`
}

// Open creates a case with a per-tenant serial reference. The ref query runs
// inside the same transaction as the insert so concurrent opens serialize.
func (s *Service) Open(ctx context.Context, tenantID string, in CreateInput) (*Case, error) {
	if in.CustomerID == "" {
		return nil, apperrors.Invalid("case.customer_required", "customer_id is required")
	}
	if in.Subject == "" || len(in.Subject) > 512 {
		return nil, apperrors.Invalid("case.subject_invalid", "subject is required (1-512 chars)")
	}
	prio := in.Priority
	if prio == "" {
		prio = "normal"
	}
	switch prio {
	case "low", "normal", "high", "urgent":
	default:
		return nil, apperrors.Invalid("case.priority_invalid", "priority must be low|normal|high|urgent")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, apperrors.Internal("db.tx_failed", "write failed").WithCause(err)
	}
	defer tx.Rollback(context.Background())

	c := &Case{
		ID: uuid.NewString(), TenantID: tenantID, CustomerID: in.CustomerID,
		Subject: in.Subject, Description: in.Description,
		Status: StatusOpen, Priority: prio,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}

	// serialize ref allocation: advisory lock per tenant
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "case-ref:"+tenantID); err != nil {
		return nil, apperrors.Internal("db.lock_failed", "write failed").WithCause(err)
	}
	var yearCount int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM cases
		WHERE tenant_id = $1 AND ref LIKE $2`,
		tenantID, fmt.Sprintf("CASE-%d-%%", time.Now().UTC().Year())).Scan(&yearCount); err != nil {
		return nil, apperrors.Internal("db.read_failed", "read failed").WithCause(err)
	}
	c.Ref = fmt.Sprintf("CASE-%d-%04d", time.Now().UTC().Year(), yearCount+1)

	_, err = tx.Exec(ctx, `
		INSERT INTO cases (id, tenant_id, customer_id, ref, subject, description, priority)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		c.ID, tenantID, c.CustomerID, c.Ref, c.Subject, c.Description, c.Priority)
	if dbinternal.IsUniqueViolation(err) {
		return nil, apperrors.Conflict("case.ref_conflict", "case reference conflict; retry")
	}
	if err != nil {
		return nil, apperrors.Internal("db.write_failed", "write failed").WithCause(err)
	}

	env, _ := events.New(events.TopicCaseOpened, s.source, c.ID, tenantID, "", map[string]any{
		"case_id": c.ID, "ref": c.Ref, "customer_id": c.CustomerID, "priority": c.Priority,
	})
	if err := s.writer.insertTx(ctx, tx, env); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, apperrors.Internal("db.commit_failed", "write failed").WithCause(err)
	}
	return s.Get(ctx, tenantID, c.ID)
}

// Transition moves a case through its guarded lifecycle.
func (s *Service) Transition(ctx context.Context, tenantID, id string, to Status, reason string) (*Case, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, apperrors.Internal("db.tx_failed", "write failed").WithCause(err)
	}
	defer tx.Rollback(context.Background())

	row := tx.QueryRow(ctx, `SELECT ref, customer_id, status FROM cases
		WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, id, tenantID)
	var ref, from string
	var customerID string
	if err := row.Scan(&ref, &customerID, &from); err != nil {
		if err == pgx.ErrNoRows {
			return nil, apperrors.NotFound("case.not_found", "case not found")
		}
		return nil, apperrors.Internal("db.read_failed", "read failed").WithCause(err)
	}
	if Status(from) == to {
		return nil, apperrors.Conflict("case.invalid_transition", "case is already "+string(to))
	}
	if !CanTransition(Status(from), to) {
		return nil, apperrors.Conflict("case.invalid_transition",
			"cannot move case from "+from+" to "+string(to))
	}

	now := time.Now().UTC()
	var resolvedAt, closedAt *time.Time
	if to == StatusResolved {
		resolvedAt = &now
	}
	if to == StatusClosed {
		closedAt = &now
	}
	_, err = tx.Exec(ctx, `
		UPDATE cases SET status=$3, resolved_at=$4, closed_at=$5, updated_at=now()
		WHERE id=$1 AND tenant_id=$2`, id, tenantID, to, resolvedAt, closedAt)
	if err != nil {
		return nil, apperrors.Internal("db.write_failed", "write failed").WithCause(err)
	}

	topic := events.TopicCaseUpdated
	if to == StatusClosed || to == StatusCanceled {
		topic = events.TopicCaseClosed
	}
	env, _ := events.New(topic, s.source, id, tenantID, "", map[string]any{
		"case_id": id, "ref": ref, "from": from, "to": to, "reason": reason,
	})
	if err := s.writer.insertTx(ctx, tx, env); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, apperrors.Internal("db.commit_failed", "write failed").WithCause(err)
	}
	return s.Get(ctx, tenantID, id)
}

// AddNote appends a note to a case (agent, AI or system authored).
func (s *Service) AddNote(ctx context.Context, tenantID, id, authorType, authorID, body string, internal bool) (*Note, error) {
	switch authorType {
	case "agent", "ai_agent", "system":
	default:
		return nil, apperrors.Invalid("case.author_type_invalid", "author_type must be agent|ai_agent|system")
	}
	if body == "" || len(body) > 8000 {
		return nil, apperrors.Invalid("case.body_invalid", "note body must be 1-8000 chars")
	}
	n := &Note{
		ID: uuid.NewString(), CaseID: id, AuthorType: authorType, AuthorID: authorID,
		Body: body, Internal: internal, CreatedAt: time.Now().UTC(),
	}
	res, err := s.pool.Exec(ctx, `
		INSERT INTO case_notes (id, case_id, tenant_id, author_type, author_id, body, internal)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		n.ID, id, tenantID, n.AuthorType, n.AuthorID, n.Body, n.Internal)
	if err != nil {
		if dbinternal.IsUniqueViolation(err) {
			return nil, apperrors.Conflict("case.note_conflict", "note conflict; retry")
		}
		return nil, apperrors.Internal("db.write_failed", "write failed").WithCause(err)
	}
	if res.RowsAffected() == 0 {
		return nil, apperrors.NotFound("case.not_found", "case not found")
	}
	_, _ = s.pool.Exec(ctx, `UPDATE cases SET updated_at = now() WHERE id = $1 AND tenant_id = $2`, id, tenantID)
	return n, nil
}

// LinkInteraction associates an interaction with a case.
func (s *Service) LinkInteraction(ctx context.Context, tenantID, caseID, interactionID string) error {
	res, err := s.pool.Exec(ctx, `
		INSERT INTO case_interactions (case_id, interaction_id)
		SELECT $1, i.id FROM interactions i
		WHERE i.id = $2 AND i.tenant_id = $3
		ON CONFLICT DO NOTHING`, caseID, interactionID, tenantID)
	if err != nil {
		return apperrors.Internal("db.write_failed", "write failed").WithCause(err)
	}
	if res.RowsAffected() == 0 {
		return apperrors.NotFound("case.link_target_not_found", "case or interaction not found")
	}
	return nil
}

// Get returns one tenant-scoped case.
func (s *Service) Get(ctx context.Context, tenantID, id string) (*Case, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, tenant_id, customer_id, ref, subject, coalesce(description,''), status, priority,
			coalesce(assigned_agent_id::text,''), resolved_at, closed_at, created_at, updated_at
		FROM cases WHERE id = $1 AND tenant_id = $2`, id, tenantID)
	c := &Case{}
	var status string
	if err := row.Scan(&c.ID, &c.TenantID, &c.CustomerID, &c.Ref, &c.Subject,
		&c.Description, &status, &c.Priority, &c.AssignedAgentID,
		&c.ResolvedAt, &c.ClosedAt, &c.CreatedAt, &c.UpdatedAt); err != nil {
		if err == pgx.ErrNoRows {
			return nil, apperrors.NotFound("case.not_found", "case not found")
		}
		return nil, apperrors.Internal("db.read_failed", "read failed").WithCause(err)
	}
	c.Status = Status(status)
	return c, nil
}

// List returns a bounded page of tenant cases.
func (s *Service) List(ctx context.Context, tenantID string, page pagination.Page) ([]Case, string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, customer_id, ref, subject, status, priority, created_at, updated_at
		FROM cases WHERE tenant_id = $1
		ORDER BY updated_at DESC, id LIMIT $2 OFFSET $3`,
		tenantID, page.Limit+1, page.Offset)
	if err != nil {
		return nil, "", apperrors.Internal("db.read_failed", "read failed").WithCause(err)
	}
	defer rows.Close()
	out := []Case{}
	for rows.Next() {
		var c Case
		var status string
		if err := rows.Scan(&c.ID, &c.CustomerID, &c.Ref, &c.Subject, &status,
			&c.Priority, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, "", apperrors.Internal("db.scan_failed", "read failed").WithCause(err)
		}
		c.Status = Status(status)
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

// ListNotes returns the note thread of a case.
func (s *Service) ListNotes(ctx context.Context, tenantID, id string, page pagination.Page) ([]Note, string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, author_type, author_id, body, internal, created_at
		FROM case_notes WHERE case_id = $1 AND tenant_id = $2
		ORDER BY created_at LIMIT $3 OFFSET $4`,
		id, tenantID, page.Limit+1, page.Offset)
	if err != nil {
		return nil, "", apperrors.Internal("db.read_failed", "read failed").WithCause(err)
	}
	defer rows.Close()
	out := []Note{}
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.ID, &n.AuthorType, &n.AuthorID, &n.Body, &n.Internal, &n.CreatedAt); err != nil {
			return nil, "", apperrors.Internal("db.scan_failed", "read failed").WithCause(err)
		}
		out = append(out, n)
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
