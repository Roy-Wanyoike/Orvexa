// Package routing owns the routing engine: deterministic, recorded decisions
// matching interactions to agents/queues from skills, language, priority,
// business hours and availability. The engine is PURE — every rule is
// unit-testable and every decision is an immutable record.
package routing

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Roy-Wanyoike/orvexa/internal/platform/outbox"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
	"github.com/Roy-Wanyoike/orvexa/pkg/events"
	"github.com/Roy-Wanyoike/orvexa/pkg/pagination"
)

// Request carries everything the engine weighs.
type Request struct {
	InteractionID  string   `json:"interaction_id"`
	RequiredSkills []string `json:"required_skills"`
	Language       string   `json:"language"`
	Priority       int      `json:"priority"`           // 1-10, higher = more urgent
	QueueID        string   `json:"queue_id,omitempty"` // pin to a queue
}

// Candidate is a scored agent.
type Candidate struct {
	AgentID       string   `json:"agent_id"`
	DisplayName   string   `json:"display_name"`
	Score         float64  `json:"score"`
	MatchedSkills []string `json:"matched_skills"`
	Language      bool     `json:"language_match"`
	Available     bool     `json:"available"`
}

// Outcome enumerates decision results.
const (
	OutcomeAssigned = "assigned_agent"
	OutcomeQueued   = "queued"
	OutcomeNone     = "no_agent_available"
)

// Decision is the immutable record of one routing decision.
type Decision struct {
	ID              string      `json:"id"`
	TenantID        string      `json:"tenant_id"`
	InteractionID   string      `json:"interaction_id"`
	Outcome         string      `json:"outcome"`
	Candidates      []Candidate `json:"candidates"`
	AssignedAgentID string      `json:"assigned_agent_id,omitempty"`
	AssignedQueueID string      `json:"assigned_queue_id,omitempty"`
	DecidedAt       time.Time   `json:"decided_at"`
	LatencyMicros   int64       `json:"latency_micros"`
}

// PresenceReader is the availability surface the engine consumes.
type PresenceReader interface {
	AvailableAgents(ctx context.Context, tenantID string) (map[string]bool, error)
}

// Engine evaluates routing requests. Pure over (request, agents, presence):
// identical inputs → identical outputs, fully replayable.
type Engine struct{}

// Score ranks one agent against a request.
// Score = 2×skill coverage + 1.5×language match + priority alignment.
// Unavailable agents are still listed (ranked, available=false) so
// supervisors see the near-miss — but can never be assigned.
func (Engine) Score(req Request, a AgentView, available bool) Candidate {
	matched := map[string]bool{}
	for _, have := range a.Skills {
		for _, need := range req.RequiredSkills {
			if have == need {
				matched[need] = true
			}
		}
	}
	coverage := 0.0
	if len(req.RequiredSkills) > 0 {
		coverage = float64(len(matched)) / float64(len(req.RequiredSkills))
	} else {
		coverage = 1.0
	}
	lang := req.Language == "" || a.Language == "" || req.Language == a.Language
	score := 2*coverage + 1.5*b2f(lang) + float64(req.Priority)/10.0
	ms := make([]string, 0, len(matched))
	for s := range matched {
		ms = append(ms, s)
	}
	sort.Strings(ms)
	return Candidate{
		AgentID: a.ID, DisplayName: a.DisplayName, Score: score,
		MatchedSkills: ms, Language: lang, Available: available,
	}
}

// Decide ranks candidates and picks the best available one above threshold.
// Missing skill coverage never blocks assignment for low-priority requests
// (best-effort fallback); for priority ≥ 8, full coverage is required.
func (e Engine) Decide(req Request, agents []AgentView, available map[string]bool) Decision {
	start := time.Now()
	d := Decision{
		ID: uuid.NewString(), InteractionID: req.InteractionID,
		DecidedAt: time.Now().UTC(),
	}
	for _, a := range agents {
		if req.QueueID != "" && a.QueueID != "" && a.QueueID != req.QueueID {
			continue
		}
		d.Candidates = append(d.Candidates, e.Score(req, a, available[a.ID]))
	}
	sort.SliceStable(d.Candidates, func(i, j int) bool {
		if d.Candidates[i].Score != d.Candidates[j].Score {
			return d.Candidates[i].Score > d.Candidates[j].Score
		}
		return d.Candidates[i].AgentID < d.Candidates[j].AgentID
	})

	for _, c := range d.Candidates {
		if !c.Available {
			continue
		}
		if req.Priority >= 8 && len(req.RequiredSkills) > 0 && len(c.MatchedSkills) < len(req.RequiredSkills) {
			continue // urgent requests demand full skill coverage
		}
		if c.Score < 2.0 {
			continue // below 1× baseline coverage — worse than queueing
		}
		d.Outcome = OutcomeAssigned
		d.AssignedAgentID = c.AgentID
		break
	}
	if d.Outcome == "" {
		if len(available) > 0 || len(d.Candidates) > 0 {
			d.Outcome = OutcomeQueued
		} else {
			d.Outcome = OutcomeNone
		}
	}
	d.LatencyMicros = time.Since(start).Microseconds()
	return d
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// AgentView is the read model the engine scores.
type AgentView struct {
	ID          string
	DisplayName string
	Language    string
	Skills      []string
	QueueID     string
}

// Service wires the engine to storage: persists decisions, applies
// assignments, and exposes presence.
type Service struct {
	pool     *pgxpool.Pool
	engine   Engine
	presence PresenceReader
	writer   *outbox.Writer
	source   string
}

func NewService(pool *pgxpool.Pool, presence PresenceReader, writer *outbox.Writer, source string) *Service {
	return &Service{pool: pool, engine: Engine{}, presence: presence, writer: writer, source: source}
}

// Route evaluates, persists the decision, and (on assignment) assigns the
// interaction + flips agent presence to busy — atomically.
func (s *Service) Route(ctx context.Context, tenantID string, req Request) (*Decision, error) {
	if req.InteractionID == "" {
		return nil, apperrors.Invalid("routing.interaction_required", "interaction_id is required")
	}
	if req.Priority < 1 || req.Priority > 10 {
		req.Priority = 5
	}
	// The routed interaction must belong to the caller's tenant. Without
	// this scope the decision was persisted under the CALLER's tenant while
	// embedding a foreign-tenant interaction UUID (identifier disclosure +
	// referential pollution, echoed by GET /routing/decisions) — and the
	// 200-vs-404 difference acted as an enumeration oracle (#96, MAT-D5).
	// Checked before any evaluation or persistence.
	var interactionOwned bool
	if err := s.pool.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM interactions WHERE id = $1 AND tenant_id = $2)`,
		req.InteractionID, tenantID).Scan(&interactionOwned); err != nil {
		return nil, apperrors.Internal("db.read_failed", "routing failed").WithCause(err)
	}
	if !interactionOwned {
		return nil, apperrors.NotFound("interaction.not_found", "interaction not found")
	}
	agents, err := s.agentViews(ctx, tenantID, req.QueueID)
	if err != nil {
		return nil, err
	}
	available, err := s.presence.AvailableAgents(ctx, tenantID)
	if err != nil {
		return nil, apperrors.Internal("routing.presence_failed", "routing unavailable").WithCause(err)
	}
	d := s.engine.Decide(req, agents, available)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, apperrors.Internal("db.tx_failed", "routing failed").WithCause(err)
	}
	defer tx.Rollback(context.Background())

	inputs := map[string]any{"request": req}
	_, err = tx.Exec(ctx, `
		INSERT INTO routing_decisions (id, tenant_id, interaction_id, inputs_json, candidates_json, outcome,
			assigned_agent_id, assigned_queue_id, latency_micros)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		d.ID, tenantID, d.InteractionID, inputs, candidatesJSON(d.Candidates), d.Outcome,
		nullStr(d.AssignedAgentID), nullStr(d.AssignedQueueID), d.LatencyMicros)
	if err != nil {
		return nil, apperrors.Internal("db.write_failed", "routing failed").WithCause(err)
	}

	if d.Outcome == OutcomeAssigned {
		_, err = tx.Exec(ctx, `
			UPDATE interactions SET assigned_agent_id = $3, updated_at = now()
			WHERE id = $1 AND tenant_id = $2 AND status IN ('pending','active')`,
			d.InteractionID, tenantID, d.AssignedAgentID)
		if err != nil {
			return nil, apperrors.Internal("db.write_failed", "routing failed").WithCause(err)
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO agent_presence (agent_id, tenant_id, status, current_interaction_id)
			VALUES ($1,$2,'busy',$3)
			ON CONFLICT (agent_id) DO UPDATE SET status='busy', current_interaction_id=$3, updated_at=now()`,
			d.AssignedAgentID, tenantID, d.InteractionID)
		if err != nil {
			return nil, apperrors.Internal("db.write_failed", "routing failed").WithCause(err)
		}
	}

	env, err := events.New(events.TopicRoutingDecisionRecorded, s.source, d.ID, tenantID, "", map[string]any{
		"decision_id": d.ID, "interaction_id": d.InteractionID, "outcome": d.Outcome,
		"assigned_agent_id": d.AssignedAgentID,
	})
	if err != nil {
		return nil, apperrors.Internal("event.envelope_failed", "routing failed").WithCause(err)
	}
	if err := s.writer.Insert(ctx, tx, env); err != nil {
		return nil, apperrors.Internal("outbox.insert_failed", "routing failed").WithCause(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, apperrors.Internal("db.commit_failed", "routing failed").WithCause(err)
	}
	return &d, nil
}

// SetPresence updates agent presence (the hot store + DB source of truth).
func (s *Service) SetPresence(ctx context.Context, tenantID, agentID, status string) error {
	switch status {
	case "available", "busy", "wrapup", "offline":
	default:
		return apperrors.Invalid("routing.invalid_presence", "presence must be available|busy|wrapup|offline")
	}
	res, err := s.pool.Exec(ctx, `
		INSERT INTO agent_presence (agent_id, tenant_id, status)
		SELECT id, $2, $3 FROM agents WHERE id = $1 AND tenant_id = $2
		ON CONFLICT (agent_id) DO UPDATE SET status = $3, updated_at = now()`,
		agentID, tenantID, status)
	if err != nil {
		return apperrors.Internal("db.write_failed", "presence failed").WithCause(err)
	}
	if res.RowsAffected() == 0 {
		return apperrors.NotFound("agent.not_found", "agent not found")
	}
	return nil
}

// DecisionRow is the persisted read model of a decision.
type DecisionRow struct {
	ID              string    `json:"id"`
	InteractionID   string    `json:"interaction_id"`
	Outcome         string    `json:"outcome"`
	AssignedAgentID string    `json:"assigned_agent_id,omitempty"`
	AssignedQueueID string    `json:"assigned_queue_id,omitempty"`
	Candidates      []byte    `json:"candidates"`
	DecidedAt       time.Time `json:"decided_at"`
	LatencyMicros   int64     `json:"latency_micros"`
}

// ListDecisions returns the immutable decision records (supervisor visibility).
func (s *Service) ListDecisions(ctx context.Context, tenantID, interactionID string, page pagination.Page) ([]DecisionRow, string, error) {
	where := "tenant_id = $1"
	args := []any{tenantID}
	if interactionID != "" {
		args = append(args, interactionID)
		where += " AND interaction_id = $2"
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, interaction_id::text, outcome, coalesce(assigned_agent_id::text,''),
			coalesce(assigned_queue_id::text,''), candidates_json, decided_at, latency_micros
		FROM routing_decisions WHERE `+where+`
		ORDER BY decided_at DESC LIMIT $`+itoa(len(args)+1)+` OFFSET $`+itoa(len(args)+2),
		append(args, page.Limit+1, page.Offset)...)
	if err != nil {
		return nil, "", apperrors.Internal("db.read_failed", "read failed").WithCause(err)
	}
	defer rows.Close()
	out := []DecisionRow{}
	for rows.Next() {
		var d DecisionRow
		if err := rows.Scan(&d.ID, &d.InteractionID, &d.Outcome, &d.AssignedAgentID,
			&d.AssignedQueueID, &d.Candidates, &d.DecidedAt, &d.LatencyMicros); err != nil {
			return nil, "", apperrors.Internal("db.scan_failed", "read failed").WithCause(err)
		}
		out = append(out, d)
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

// Presence is the agent presence read model.
type Presence struct {
	AgentID              string    `json:"agent_id"`
	Status               string    `json:"status"`
	CurrentInteractionID string    `json:"current_interaction_id,omitempty"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// GetPresence returns one agent's presence.
func (s *Service) GetPresence(ctx context.Context, tenantID, agentID string) (*Presence, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT agent_id, status, coalesce(current_interaction_id::text,''), updated_at
		FROM agent_presence WHERE agent_id = $1 AND tenant_id = $2`, agentID, tenantID)
	p := &Presence{}
	if err := row.Scan(&p.AgentID, &p.Status, &p.CurrentInteractionID, &p.UpdatedAt); err != nil {
		if err == pgx.ErrNoRows {
			return nil, apperrors.NotFound("agent.presence_not_found", "agent has no presence record")
		}
		return nil, apperrors.Internal("db.read_failed", "read failed").WithCause(err)
	}
	return p, nil
}

func itoa(n int) string {
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

func (s *Service) agentViews(ctx context.Context, tenantID, queueID string) ([]AgentView, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.display_name, a.language,
			coalesce(array_agg(DISTINCT sk.skill) FILTER (WHERE sk.skill IS NOT NULL), '{}') AS skills
		FROM agents a
		LEFT JOIN agent_skills sk ON sk.agent_id = a.id
		WHERE a.tenant_id = $1 AND a.status = 'active'
		GROUP BY a.id, a.display_name, a.language
		ORDER BY a.id`, tenantID)
	if err != nil {
		return nil, apperrors.Internal("db.read_failed", "routing failed").WithCause(err)
	}
	defer rows.Close()
	out := []AgentView{}
	for rows.Next() {
		var v AgentView
		if err := rows.Scan(&v.ID, &v.DisplayName, &v.Language, &v.Skills); err != nil {
			return nil, apperrors.Internal("db.scan_failed", "routing failed").WithCause(err)
		}
		out = append(out, v)
	}
	return out, nil
}

func candidatesJSON(c []Candidate) []byte {
	b, _ := json.Marshal(c)
	return b
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
