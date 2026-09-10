// Package agents owns agent identity + skills (presence lives in routing).
package agents

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

// Agent is an actor that can own interactions (human today; AI agents get
// their own registry in the intelligence wave).
type Agent struct {
	ID               string    `json:"id"`
	TenantID         string    `json:"tenant_id"`
	ExternalIdentity string    `json:"external_identity"`
	DisplayName      string    `json:"display_name"`
	Email            string    `json:"email,omitempty"`
	Language         string    `json:"language"`
	Status           string    `json:"status"`
	Skills           []Skill   `json:"skills"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// Skill is a routing capability with proficiency 1-5.
type Skill struct {
	Skill string `json:"skill"`
	Level int    `json:"level"`
}

type CreateInput struct {
	ExternalIdentity string  `json:"external_identity"`
	DisplayName      string  `json:"display_name"`
	Email            string  `json:"email"`
	Language         string  `json:"language"`
	Skills           []Skill `json:"skills"`
}

// Service implements agent use-cases.
type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// Create registers an agent. Duplicate external identity per tenant → 409.
func (s *Service) Create(ctx context.Context, tenantID string, in CreateInput) (*Agent, error) {
	if in.ExternalIdentity == "" || in.DisplayName == "" {
		return nil, apperrors.Invalid("agent.identity_required",
			"external_identity and display_name are required")
	}
	lang := in.Language
	if lang == "" {
		lang = "en"
	}
	a := &Agent{
		ID: uuid.NewString(), TenantID: tenantID,
		ExternalIdentity: in.ExternalIdentity, DisplayName: in.DisplayName,
		Email: in.Email, Language: lang, Status: "active",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		Skills: []Skill{},
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, apperrors.Internal("db.tx_failed", "write failed").WithCause(err)
	}
	defer tx.Rollback(context.Background())

	_, err = tx.Exec(ctx, `
		INSERT INTO agents (id, tenant_id, external_identity, display_name, email, language)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		a.ID, tenantID, a.ExternalIdentity, a.DisplayName, a.Email, a.Language)
	if isUnique(err) {
		return nil, apperrors.Conflict("agent.identity_exists", "external identity already registered")
	}
	if err != nil {
		return nil, apperrors.Internal("db.write_failed", "write failed").WithCause(err)
	}
	for _, sk := range in.Skills {
		if sk.Skill == "" {
			return nil, apperrors.Invalid("agent.invalid_skill", "skill must not be empty")
		}
		if sk.Level < 1 || sk.Level > 5 {
			return nil, apperrors.Invalid("agent.invalid_skill_level", "skill level must be 1-5")
		}
		_, err = tx.Exec(ctx, `INSERT INTO agent_skills (agent_id, skill, level) VALUES ($1,$2,$3)`,
			a.ID, sk.Skill, sk.Level)
		if err != nil {
			return nil, apperrors.Internal("db.write_failed", "write failed").WithCause(err)
		}
		a.Skills = append(a.Skills, sk)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, apperrors.Internal("db.commit_failed", "write failed").WithCause(err)
	}
	return a, nil
}

// Get returns one tenant-scoped agent.
func (s *Service) Get(ctx context.Context, tenantID, id string) (*Agent, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, tenant_id, external_identity, display_name, coalesce(email,''), language, status, created_at, updated_at
		FROM agents WHERE id = $1 AND tenant_id = $2`, id, tenantID)
	a := &Agent{Skills: []Skill{}}
	if err := row.Scan(&a.ID, &a.TenantID, &a.ExternalIdentity, &a.DisplayName,
		&a.Email, &a.Language, &a.Status, &a.CreatedAt, &a.UpdatedAt); err != nil {
		if err == pgx.ErrNoRows {
			return nil, apperrors.NotFound("agent.not_found", "agent not found")
		}
		return nil, apperrors.Internal("db.read_failed", "read failed").WithCause(err)
	}
	skills, err := s.pool.Query(ctx, `SELECT skill, level FROM agent_skills WHERE agent_id = $1 ORDER BY skill`, a.ID)
	if err != nil {
		return nil, apperrors.Internal("db.read_failed", "read failed").WithCause(err)
	}
	defer skills.Close()
	for skills.Next() {
		var sk Skill
		if err := skills.Scan(&sk.Skill, &sk.Level); err != nil {
			return nil, apperrors.Internal("db.scan_failed", "read failed").WithCause(err)
		}
		a.Skills = append(a.Skills, sk)
	}
	return a, nil
}

// List returns a bounded page of tenant agents.
func (s *Service) List(ctx context.Context, tenantID string, page pagination.Page) ([]Agent, string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, external_identity, display_name, coalesce(email,''), language, status, created_at, updated_at
		FROM agents WHERE tenant_id = $1
		ORDER BY created_at DESC, id LIMIT $2 OFFSET $3`,
		tenantID, page.Limit+1, page.Offset)
	if err != nil {
		return nil, "", apperrors.Internal("db.read_failed", "read failed").WithCause(err)
	}
	defer rows.Close()
	out := []Agent{}
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.ID, &a.ExternalIdentity, &a.DisplayName, &a.Email,
			&a.Language, &a.Status, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, "", apperrors.Internal("db.scan_failed", "read failed").WithCause(err)
		}
		out = append(out, a)
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

func isUnique(err error) bool {
	return dbinternal.IsUniqueViolation(err)
}
