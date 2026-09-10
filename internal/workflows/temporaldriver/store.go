//go:build temporal

package temporaldriver

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Roy-Wanyoike/orvexa/internal/platform/db/dbinternal"
	"github.com/Roy-Wanyoike/orvexa/internal/platform/outbox"
	"github.com/Roy-Wanyoike/orvexa/internal/workflows"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
	"github.com/Roy-Wanyoike/orvexa/pkg/events"
)

// Store is the driver's audit port — the persistence half of the engine's
// behavior.
//
// Honesty note (ADR-0009): the engine persists through a CONCRETE
// *pgxpool.Pool + *outbox.Writer; there is no storage interface to implement.
// This port is therefore defined by the driver, and NewPostgresStore writes
// the SAME rows (workflow_instances, workflow_steps) and the SAME outbox
// events as the engine, against the schema from migration 0010, so the
// existing read path (workflows.Engine.Get, httpserver) observes
// Temporal-driven instances without changes.
//
// Every mutation is guarded by a compare-and-swap on current_step (or status),
// which makes the activities idempotent under Temporal's at-least-once
// activity retries: a retried write that arrives after the transition already
// committed is a silent no-op.
type Store interface {
	// StartInstance inserts the initial instance row and its first step trace
	// plus the started event — one transaction, mirroring engine.start.
	StartInstance(ctx context.Context, rec StartRecord) error
	// RecordStep records a completed step and advances the instance
	// (status/current_step/next_run_at) in one transaction. With Next set it
	// is the waitFor equivalent (status waiting_timer, step-completed event);
	// without Next it only records the step.
	RecordStep(ctx context.Context, rec StepRecord) error
	// CompleteInstance seals the instance: status completed, step closed,
	// completed event — the complete equivalent.
	CompleteInstance(ctx context.Context, rec DoneRecord) error
	// MarkCanceled sets status canceled (no step row, no event — engine
	// Cancel parity). Canceling an already-terminal instance is a no-op.
	MarkCanceled(ctx context.Context, rec DoneRecord) error
	// MarkFailed sets status failed with the error preserved (engine
	// markFailed parity). Failing an already-terminal instance is a no-op.
	MarkFailed(ctx context.Context, rec DoneRecord) error
	// GetInstance returns one tenant-scoped instance (engine.Get parity:
	// foreign-tenant or unknown ids are indistinguishable not_found).
	GetInstance(ctx context.Context, tenantID, instanceID string) (*workflows.Instance, error)
}

// StartRecord carries the initial instance and the detail for its first step
// trace row (engine parity: {"scheduled_for": nextRun}).
type StartRecord struct {
	Instance   *workflows.Instance
	StepDetail map[string]any
}

// StepRecord advances the instance after a step ran. ExpectedPrev is the
// current_step CAS guard (the step the workflow believes is current).
type StepRecord struct {
	InstanceID   string
	TenantID     string
	Type         string
	ExpectedPrev string
	DoneStep     string
	Detail       map[string]any
	// Next schedules the following step (waitFor equivalent). Nil means no
	// timer is attached to this transition.
	Next *NextStep
}

// NextStep names the next step and when it becomes due (durable timer).
type NextStep struct {
	Step  string
	RunAt time.Time
}

// DoneRecord is the terminal-state record (completed/canceled/failed).
type DoneRecord struct {
	InstanceID   string
	TenantID     string
	Type         string
	ExpectedPrev string // CAS guard; empty means "any non-terminal status"
	Detail       map[string]any
	Error        string // preserved for failed instances (engine parity)
}

// StepTrace is one immutable audit row (test/introspection view).
type StepTrace struct {
	Step       string
	Detail     map[string]any
	ExecutedAt time.Time
}

// PostgresStore implements Store against the engine's schema (migration
// 0010), reusing the outbox writer so audit rows and events stay atomic —
// the same guarantee engine.start relies on.
type PostgresStore struct {
	pool   *pgxpool.Pool
	writer *outbox.Writer
	source string
}

// NewPostgresStore wires the audit port to the engine's tables. source is
// stamped on outbox envelopes (use the worker/service identity, e.g.
// "orvexa-worker" — Config.Source when driven from this package).
func NewPostgresStore(pool *pgxpool.Pool, source string) *PostgresStore {
	if source == "" {
		source = DefaultSource
	}
	return &PostgresStore{pool: pool, writer: outbox.NewWriter(pool), source: source}
}

// StartInstance inserts the instance + first step + started event atomically.
func (s *PostgresStore) StartInstance(ctx context.Context, rec StartRecord) error {
	inst := rec.Instance
	detail := rec.StepDetail
	if detail == nil {
		detail = map[string]any{}
	}
	payload, _ := json.Marshal(inst.Payload)
	state, _ := json.Marshal(inst.State)
	b, _ := json.Marshal(detail)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return apperrors.Internal("db.tx_failed", "workflow failed").WithCause(err)
	}
	defer tx.Rollback(context.Background())

	if _, err := tx.Exec(ctx, `
                INSERT INTO workflow_instances (id, tenant_id, type, status, payload_json, state_json, current_step, next_run_at)
                VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		inst.ID, inst.TenantID, inst.Type, inst.Status, payload, state, inst.CurrentStep, inst.NextRunAt); err != nil {
		if dbinternal.IsUniqueViolation(err) {
			return apperrors.Conflict("workflow.conflict", "workflow conflict; retry")
		}
		return apperrors.Internal("db.write_failed", "workflow failed").WithCause(err)
	}
	if _, err := tx.Exec(ctx, `
                INSERT INTO workflow_steps (instance_id, step, status, detail_json)
                VALUES ($1,$2,'completed',$3)`, inst.ID, inst.CurrentStep, b); err != nil {
		return apperrors.Internal("db.write_failed", "workflow step failed").WithCause(err)
	}
	if err := s.emit(ctx, tx, events.TopicWorkflowStarted, inst, map[string]any{
		"workflow_id": inst.ID, "type": inst.Type, "next_run_at": ptrTime(inst.NextRunAt),
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return apperrors.Internal("db.commit_failed", "workflow failed").WithCause(err)
	}
	return nil
}

// RecordStep advances the instance behind a current_step CAS guard; a lost
// race means a prior attempt already advanced the row — silent no-op.
func (s *PostgresStore) RecordStep(ctx context.Context, rec StepRecord) error {
	b, _ := json.Marshal(orEmptyMap(rec.Detail))
	nextStep := rec.DoneStep
	var nextRun *time.Time
	status := workflows.StatusRunning
	if rec.Next != nil {
		nextStep = rec.Next.Step
		nextRun = &rec.Next.RunAt
		status = workflows.StatusWaitingTimer
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return apperrors.Internal("db.tx_failed", "workflow step failed").WithCause(err)
	}
	defer tx.Rollback(context.Background())

	res, err := tx.Exec(ctx, `
                UPDATE workflow_instances SET status=$5, current_step=$3, next_run_at=$4, updated_at=now()
                WHERE id=$1 AND tenant_id=$2 AND current_step=$6`,
		rec.InstanceID, rec.TenantID, nextStep, nextRun, status, rec.ExpectedPrev)
	if err != nil {
		return apperrors.Internal("db.write_failed", "workflow step failed").WithCause(err)
	}
	if res.RowsAffected() == 0 {
		return nil // already advanced by a prior attempt: idempotent no-op
	}
	if _, err := tx.Exec(ctx, `
                INSERT INTO workflow_steps (instance_id, step, status, detail_json)
                VALUES ($1,$2,'completed',$3)`, rec.InstanceID, rec.DoneStep, b); err != nil {
		return apperrors.Internal("db.write_failed", "workflow step failed").WithCause(err)
	}
	if rec.Next != nil { // waitFor equivalent emits the step event (engine parity)
		if err := s.emit(ctx, tx, events.TopicWorkflowStepCompleted,
			&workflows.Instance{ID: rec.InstanceID, TenantID: rec.TenantID, Type: rec.Type},
			map[string]any{"workflow_id": rec.InstanceID, "completed": rec.DoneStep,
				"next": rec.Next.Step, "run_at": rec.Next.RunAt}); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return apperrors.Internal("db.commit_failed", "workflow step failed").WithCause(err)
	}
	return nil
}

// CompleteInstance seals the instance behind the ExpectedPrev CAS guard.
func (s *PostgresStore) CompleteInstance(ctx context.Context, rec DoneRecord) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return apperrors.Internal("db.tx_failed", "workflow complete failed").WithCause(err)
	}
	defer tx.Rollback(context.Background())

	res, err := tx.Exec(ctx, `
                UPDATE workflow_instances SET status='completed', next_run_at=NULL, current_step=$3, updated_at=now()
                WHERE id=$1 AND tenant_id=$2 AND current_step=$4`,
		rec.InstanceID, rec.TenantID, workflows.StepClosed, rec.ExpectedPrev)
	if err != nil {
		return apperrors.Internal("db.write_failed", "workflow complete failed").WithCause(err)
	}
	if res.RowsAffected() == 0 {
		return nil // already sealed: idempotent no-op
	}
	if _, err := tx.Exec(ctx, `
                INSERT INTO workflow_steps (instance_id, step, status, detail_json)
                VALUES ($1,$2,'completed',$3)`,
		rec.InstanceID, workflows.StepClosed, mustJSON(orEmptyMap(rec.Detail))); err != nil {
		return apperrors.Internal("db.write_failed", "workflow step failed").WithCause(err)
	}
	if err := s.emit(ctx, tx, events.TopicWorkflowCompleted,
		&workflows.Instance{ID: rec.InstanceID, TenantID: rec.TenantID, Type: rec.Type},
		map[string]any{"workflow_id": rec.InstanceID}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// MarkCanceled cancels a cancellable instance; terminal rows are left alone.
func (s *PostgresStore) MarkCanceled(ctx context.Context, rec DoneRecord) error {
	_, err := s.pool.Exec(ctx, `
                UPDATE workflow_instances SET status='canceled', next_run_at=NULL, updated_at=now()
                WHERE id=$1 AND tenant_id=$2 AND status IN ('running','waiting_timer')`,
		rec.InstanceID, rec.TenantID)
	if err != nil {
		return apperrors.Internal("db.write_failed", "cancel failed").WithCause(err)
	}
	return nil
}

// MarkFailed preserves the failure reason on a live instance (engine parity).
func (s *PostgresStore) MarkFailed(ctx context.Context, rec DoneRecord) error {
	_, err := s.pool.Exec(ctx, `
                UPDATE workflow_instances SET status='failed', error=$3, updated_at=now()
                WHERE id=$1 AND tenant_id=$2 AND status IN ('running','waiting_timer')`,
		rec.InstanceID, rec.TenantID, rec.Error)
	if err != nil {
		return apperrors.Internal("db.write_failed", "workflow failed").WithCause(err)
	}
	return nil
}

// GetInstance mirrors engine.Get exactly (same columns, same not-found code).
func (s *PostgresStore) GetInstance(ctx context.Context, tenantID, instanceID string) (*workflows.Instance, error) {
	row := s.pool.QueryRow(ctx, `
                SELECT id, tenant_id, type, status, payload_json, state_json, current_step,
                        next_run_at, attempts, coalesce(error,''), created_at, updated_at
                FROM workflow_instances WHERE id=$1 AND tenant_id=$2`, instanceID, tenantID)
	return scanInstanceRow(row)
}

// emit builds the envelope exactly like the engine and hands it to the
// outbox writer inside the caller's transaction (atomicity parity).
func (s *PostgresStore) emit(ctx context.Context, tx pgx.Tx, topic string, i *workflows.Instance, data map[string]any) error {
	env, err := events.New(topic, s.source, i.ID, i.TenantID, "", data)
	if err != nil {
		return apperrors.Internal("event.envelope_failed", "workflow event failed").WithCause(err)
	}
	if err := s.writer.Insert(ctx, tx, env); err != nil {
		return apperrors.Internal("outbox.insert_failed", "workflow event failed").WithCause(err)
	}
	return nil
}

// scanInstanceRow is the engine's scanInstance, verbatim.
func scanInstanceRow(row pgx.Row) (*workflows.Instance, error) {
	i := &workflows.Instance{}
	var payload, state []byte
	if err := row.Scan(&i.ID, &i.TenantID, &i.Type, &i.Status, &payload, &state,
		&i.CurrentStep, &i.NextRunAt, &i.Attempts, &i.Error, &i.CreatedAt, &i.UpdatedAt); err != nil {
		if err == pgx.ErrNoRows {
			return nil, apperrors.NotFound("workflow.not_found", "workflow not found")
		}
		return nil, apperrors.Internal("db.read_failed", "read failed").WithCause(err)
	}
	_ = json.Unmarshal(payload, &i.Payload)
	_ = json.Unmarshal(state, &i.State)
	return i, nil
}

func ptrTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// MemoryStore is an in-process Store: identical CAS semantics to
// PostgresStore, no durability. It serves unit tests, the integration test
// (which checks Temporal-side behavior, not Postgres) and execution-only
// embedders. It is NOT a production audit backend.
type MemoryStore struct {
	mu    sync.Mutex
	inst  map[string]*workflows.Instance
	steps map[string][]StepTrace
}

// NewMemoryStore returns an empty in-memory Store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{inst: map[string]*workflows.Instance{}, steps: map[string][]StepTrace{}}
}

// Steps returns the recorded trace for an instance (introspection helper).
func (m *MemoryStore) Steps(instanceID string) []StepTrace {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]StepTrace, len(m.steps[instanceID]))
	copy(out, m.steps[instanceID])
	return out
}

// StartInstance stores a defensive copy of the instance and the first step.
func (m *MemoryStore) StartInstance(_ context.Context, rec StartRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst := rec.Instance
	if _, exists := m.inst[inst.ID]; exists {
		return apperrors.Conflict("workflow.conflict", "workflow conflict; retry")
	}
	m.inst[inst.ID] = cloneInstance(inst)
	m.steps[inst.ID] = append(m.steps[inst.ID], StepTrace{
		Step: inst.CurrentStep, Detail: copyMap(rec.StepDetail), ExecutedAt: time.Now(),
	})
	return nil
}

// RecordStep applies the same CAS rule as PostgresStore.
func (m *MemoryStore) RecordStep(_ context.Context, rec StepRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst, ok := m.inst[rec.InstanceID]
	if !ok || inst.TenantID != rec.TenantID || inst.CurrentStep != rec.ExpectedPrev {
		return nil // idempotent no-op
	}
	if rec.Next != nil {
		inst.Status = workflows.StatusWaitingTimer
		inst.CurrentStep = rec.Next.Step
		runAt := rec.Next.RunAt
		inst.NextRunAt = &runAt
	} else {
		inst.Status = workflows.StatusRunning
		inst.CurrentStep = rec.DoneStep
	}
	inst.UpdatedAt = time.Now()
	m.steps[rec.InstanceID] = append(m.steps[rec.InstanceID],
		StepTrace{Step: rec.DoneStep, Detail: copyMap(rec.Detail), ExecutedAt: time.Now()})
	return nil
}

// CompleteInstance seals the instance behind the CAS guard.
func (m *MemoryStore) CompleteInstance(_ context.Context, rec DoneRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst, ok := m.inst[rec.InstanceID]
	if !ok || inst.TenantID != rec.TenantID || inst.CurrentStep != rec.ExpectedPrev {
		return nil
	}
	inst.Status = workflows.StatusCompleted
	inst.CurrentStep = workflows.StepClosed
	inst.NextRunAt = nil
	inst.UpdatedAt = time.Now()
	m.steps[rec.InstanceID] = append(m.steps[rec.InstanceID],
		StepTrace{Step: workflows.StepClosed, Detail: copyMap(rec.Detail), ExecutedAt: time.Now()})
	return nil
}

// MarkCanceled cancels only live instances (running/waiting_timer).
func (m *MemoryStore) MarkCanceled(_ context.Context, rec DoneRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst, ok := m.inst[rec.InstanceID]
	if !ok || inst.TenantID != rec.TenantID {
		return nil
	}
	if inst.Status == workflows.StatusRunning || inst.Status == workflows.StatusWaitingTimer {
		inst.Status = workflows.StatusCanceled
		inst.NextRunAt = nil
		inst.UpdatedAt = time.Now()
	}
	return nil
}

// MarkFailed preserves the reason on live instances (engine parity).
func (m *MemoryStore) MarkFailed(_ context.Context, rec DoneRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst, ok := m.inst[rec.InstanceID]
	if !ok || inst.TenantID != rec.TenantID {
		return nil
	}
	if inst.Status == workflows.StatusRunning || inst.Status == workflows.StatusWaitingTimer {
		inst.Status = workflows.StatusFailed
		inst.Error = rec.Error
		inst.UpdatedAt = time.Now()
	}
	return nil
}

// GetInstance returns a defensive copy; unknown or foreign-tenant ids are
// not_found (engine parity).
func (m *MemoryStore) GetInstance(_ context.Context, tenantID, instanceID string) (*workflows.Instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst, ok := m.inst[instanceID]
	if !ok || inst.TenantID != tenantID {
		return nil, apperrors.NotFound("workflow.not_found", "workflow not found")
	}
	return cloneInstance(inst), nil
}

func cloneInstance(i *workflows.Instance) *workflows.Instance {
	b, _ := json.Marshal(i)
	var out workflows.Instance
	_ = json.Unmarshal(b, &out)
	return &out
}

func copyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func orEmptyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
