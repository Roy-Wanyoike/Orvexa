// Package queues owns queue configuration: skills required to serve,
// priority weight, business hours, SLA target. Runtime assignment lives in
// the routing engine; this module is configuration + read model.
package queues

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Roy-Wanyoike/orvexa/internal/platform/db/dbinternal"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
	"github.com/Roy-Wanyoike/orvexa/pkg/pagination"
)

// Queue is a routing destination with its serving policy.
type Queue struct {
	ID            string         `json:"id"`
	TenantID      string         `json:"tenant_id"`
	Name          string         `json:"name"`
	Description   string         `json:"description,omitempty"`
	Priority      int            `json:"priority"`
	BusinessHours map[string]any `json:"business_hours,omitempty"`
	SLASeconds    int            `json:"sla_seconds"`
	Status        string         `json:"status"`
	Skills        []string       `json:"skills"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

type CreateInput struct {
	Name          string         `json:"name"`
	Description   string         `json:"description"`
	Priority      int            `json:"priority"`
	BusinessHours map[string]any `json:"business_hours"`
	SLASeconds    int            `json:"sla_seconds"`
	Skills        []string       `json:"skills"`
}

// Service implements queue use-cases.
type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// Create registers a queue. Name is unique per tenant.
func (s *Service) Create(ctx context.Context, tenantID string, in CreateInput) (*Queue, error) {
	if in.Name == "" {
		return nil, apperrors.Invalid("queue.name_required", "name is required")
	}
	prio := in.Priority
	if prio == 0 {
		prio = 5
	}
	if prio < 1 || prio > 10 {
		return nil, apperrors.Invalid("queue.invalid_priority", "priority must be 1-10")
	}
	sla := in.SLASeconds
	if sla == 0 {
		sla = 60
	}
	if sla < 5 || sla > 3600 {
		return nil, apperrors.Invalid("queue.invalid_sla", "sla_seconds must be 5-3600")
	}
	q := &Queue{
		ID: uuid.NewString(), TenantID: tenantID, Name: in.Name,
		Description: in.Description, Priority: prio,
		BusinessHours: in.BusinessHours, SLASeconds: sla, Status: "active",
		Skills: []string{}, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, apperrors.Internal("db.tx_failed", "write failed").WithCause(err)
	}
	defer tx.Rollback(context.Background())

	_, err = tx.Exec(ctx, `
		INSERT INTO queues (id, tenant_id, name, description, priority, business_hours_json, sla_seconds)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		q.ID, tenantID, q.Name, q.Description, q.Priority, q.BusinessHours, q.SLASeconds)
	if dbinternal.IsUniqueViolation(err) {
		return nil, apperrors.Conflict("queue.name_exists", "queue name already exists")
	}
	if err != nil {
		return nil, apperrors.Internal("db.write_failed", "write failed").WithCause(err)
	}
	for _, sk := range dedupe(in.Skills) {
		if sk == "" {
			return nil, apperrors.Invalid("queue.invalid_skill", "skills must not be empty")
		}
		_, err = tx.Exec(ctx, `INSERT INTO queue_skills (queue_id, skill) VALUES ($1,$2)`, q.ID, sk)
		if err != nil {
			return nil, apperrors.Internal("db.write_failed", "write failed").WithCause(err)
		}
		q.Skills = append(q.Skills, sk)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, apperrors.Internal("db.commit_failed", "write failed").WithCause(err)
	}
	return q, nil
}

// Get returns one tenant-scoped queue.
func (s *Service) Get(ctx context.Context, tenantID, id string) (*Queue, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, tenant_id, name, coalesce(description,''), priority, business_hours_json, sla_seconds, status, created_at, updated_at
		FROM queues WHERE id = $1 AND tenant_id = $2`, id, tenantID)
	q := &Queue{Skills: []string{}}
	var bh map[string]any
	if err := row.Scan(&q.ID, &q.TenantID, &q.Name, &q.Description, &q.Priority,
		&bh, &q.SLASeconds, &q.Status, &q.CreatedAt, &q.UpdatedAt); err != nil {
		if err == pgx.ErrNoRows {
			return nil, apperrors.NotFound("queue.not_found", "queue not found")
		}
		return nil, apperrors.Internal("db.read_failed", "read failed").WithCause(err)
	}
	if bh != nil {
		q.BusinessHours = bh
	}
	sk, err := s.pool.Query(ctx, `SELECT skill FROM queue_skills WHERE queue_id = $1 ORDER BY skill`, q.ID)
	if err != nil {
		return nil, apperrors.Internal("db.read_failed", "read failed").WithCause(err)
	}
	defer sk.Close()
	for sk.Next() {
		var v string
		if err := sk.Scan(&v); err != nil {
			return nil, apperrors.Internal("db.scan_failed", "read failed").WithCause(err)
		}
		q.Skills = append(q.Skills, v)
	}
	return q, nil
}

// List returns a bounded page of tenant queues.
func (s *Service) List(ctx context.Context, tenantID string, page pagination.Page) ([]Queue, string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, coalesce(description,''), priority, sla_seconds, status, created_at, updated_at
		FROM queues WHERE tenant_id = $1
		ORDER BY priority, name LIMIT $2 OFFSET $3`,
		tenantID, page.Limit+1, page.Offset)
	if err != nil {
		return nil, "", apperrors.Internal("db.read_failed", "read failed").WithCause(err)
	}
	defer rows.Close()
	out := []Queue{}
	for rows.Next() {
		var q Queue
		if err := rows.Scan(&q.ID, &q.Name, &q.Description, &q.Priority, &q.SLASeconds,
			&q.Status, &q.CreatedAt, &q.UpdatedAt); err != nil {
			return nil, "", apperrors.Internal("db.scan_failed", "read failed").WithCause(err)
		}
		out = append(out, q)
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

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
