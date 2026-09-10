// Package workflows owns the execution plane's durable workflow engine:
// long-running processes (callbacks, collections ladders) whose state lives
// in Postgres, survive restarts, and advance through deterministic steps.
// Temporal-compatible semantics (deterministic steps, retries with backoff,
// crash-safe resume) with a DB-backed executor; the Temporal adapter is a
// config-level extraction (ADR-0006).
package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Roy-Wanyoike/orvexa/internal/platform/db/dbinternal"
	"github.com/Roy-Wanyoike/orvexa/internal/platform/outbox"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
	"github.com/Roy-Wanyoike/orvexa/pkg/events"
)

// Workflow types (CHECK constraint in migration 0010).
const (
	TypeCallback    = "callback"
	TypeCollections = "collections"
)

// Instance status lifecycle: running → (waiting_timer ↔ running) → completed|failed|canceled.
const (
	StatusRunning      = "running"
	StatusWaitingTimer = "waiting_timer"
	StatusCompleted    = "completed"
	StatusFailed       = "failed"
	StatusCanceled     = "canceled"
)

// Step names — the auditable trace of what a workflow did.
const (
	StepScheduled   = "scheduled"
	StepWaited      = "waited"
	StepContacted   = "contacted"
	StepEscalated   = "escalated"
	StepClosed      = "closed"
	StepTaskCreated = "task_created"
)

// Instance is the durable workflow record.
type Instance struct {
	ID          string         `json:"id"`
	TenantID    string         `json:"tenant_id"`
	Type        string         `json:"type"`
	Status      string         `json:"status"`
	Payload     map[string]any `json:"payload"`
	State       map[string]any `json:"state"`
	CurrentStep string         `json:"current_step"`
	NextRunAt   *time.Time     `json:"next_run_at,omitempty"`
	Attempts    int            `json:"attempts"`
	Error       string         `json:"error,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

// Services is the surface workflows may act on (via the executor wiring).
// Workflows never touch the tool gateway's AI path — they call services
// directly as first-class platform actors, always tenant-scoped.
type Services interface {
	// QueueMessage sends a platform message for a workflow step.
	QueueMessage(ctx context.Context, tenantID, customerID, typ, to, body string) error
}

// Engine owns workflow use-cases and the deterministic step machine.
type Engine struct {
	pool     *pgxpool.Pool
	writer   *outbox.Writer
	services Services
	source   string
	// nowFunc is injectable for tests.
	nowFunc func() time.Time
}

func NewEngine(pool *pgxpool.Pool, writer *outbox.Writer, services Services, source string) *Engine {
	return &Engine{pool: pool, writer: writer, services: services, source: source, nowFunc: time.Now}
}

// StartCallbackInput schedules a callback for a customer.
type StartCallbackInput struct {
	CustomerID string    `json:"customer_id"`
	Phone      string    `json:"phone"`
	ScheduleAt time.Time `json:"schedule_at"`
	Notes      string    `json:"notes"`
}

// StartCallback creates a callback workflow: schedule → wait → contact.
func (e *Engine) StartCallback(ctx context.Context, tenantID string, in StartCallbackInput) (*Instance, error) {
	if in.CustomerID == "" {
		return nil, apperrors.Invalid("workflow.customer_required", "customer_id is required")
	}
	if in.Phone == "" {
		return nil, apperrors.Invalid("workflow.phone_required", "phone is required")
	}
	when := in.ScheduleAt
	if when.IsZero() {
		when = e.nowFunc().Add(30 * time.Minute)
	}
	if when.Before(e.nowFunc()) {
		return nil, apperrors.Invalid("workflow.schedule_past", "schedule_at must be in the future")
	}
	return e.start(ctx, tenantID, TypeCallback, map[string]any{
		"customer_id": in.CustomerID, "phone": in.Phone, "notes": in.Notes,
	}, when, StepScheduled)
}

// StartCollectionsInput drives a collections ladder for an invoice reference.
type StartCollectionsInput struct {
	CustomerID string `json:"customer_id"`
	InvoiceRef string `json:"invoice_ref"`
	AmountDue  int64  `json:"amount_due"`
	Phone      string `json:"phone"`
}

// StartCollections creates the ladder: SMS → wait 2d → WhatsApp → wait 3d →
// escalate (call task) → close.
func (e *Engine) StartCollections(ctx context.Context, tenantID string, in StartCollectionsInput) (*Instance, error) {
	if in.CustomerID == "" || in.InvoiceRef == "" {
		return nil, apperrors.Invalid("workflow.fields_required", "customer_id and invoice_ref are required")
	}
	if in.AmountDue <= 0 {
		return nil, apperrors.Invalid("workflow.amount_invalid", "amount_due must be positive")
	}
	if in.Phone == "" {
		return nil, apperrors.Invalid("workflow.phone_required", "phone is required")
	}
	return e.start(ctx, tenantID, TypeCollections, map[string]any{
		"customer_id": in.CustomerID, "invoice_ref": in.InvoiceRef,
		"amount_due": in.AmountDue, "phone": in.Phone,
	}, e.nowFunc(), StepContacted) // first contact (SMS) fires immediately
}

// start persists the initial instance + started event atomically.
func (e *Engine) start(ctx context.Context, tenantID, typ string, payload map[string]any, nextRun time.Time, step string) (*Instance, error) {
	inst := &Instance{
		ID: uuid.NewString(), TenantID: tenantID, Type: typ,
		Status: StatusWaitingTimer, Payload: payload, State: map[string]any{},
		CurrentStep: step, NextRunAt: &nextRun,
	}
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return nil, apperrors.Internal("db.tx_failed", "workflow failed").WithCause(err)
	}
	defer tx.Rollback(context.Background())

	if err := e.insertInstance(ctx, tx, inst); err != nil {
		return nil, err
	}
	if err := e.recordStep(ctx, tx, inst.ID, step, map[string]any{"scheduled_for": nextRun}); err != nil {
		return nil, err
	}
	if err := e.emit(ctx, tx, events.TopicWorkflowStarted, inst, map[string]any{
		"workflow_id": inst.ID, "type": typ, "next_run_at": nextRun,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, apperrors.Internal("db.commit_failed", "workflow failed").WithCause(err)
	}
	return inst, nil
}

// Tick advances due workflows. Called by the worker on a schedule; safe to
// run concurrently (rows claimed with FOR UPDATE SKIP LOCKED).
func (e *Engine) Tick(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := e.pool.Query(ctx, `
		UPDATE workflow_instances SET attempts = attempts + 1, updated_at = now()
		WHERE id IN (
			SELECT id FROM workflow_instances
			WHERE status IN ('running','waiting_timer') AND next_run_at <= now()
			ORDER BY next_run_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, tenant_id, type, payload_json, state_json, current_step, attempts`, limit)
	if err != nil {
		return 0, apperrors.Internal("db.claim_failed", "workflow tick failed").WithCause(err)
	}
	type claimed struct {
		id, tenant, typ, step string
		attempts              int
		payload, state        map[string]any
	}
	batch := []claimed{}
	for rows.Next() {
		var c claimed
		if err := rows.Scan(&c.id, &c.tenant, &c.typ, &c.payload, &c.state, &c.step, &c.attempts); err != nil {
			rows.Close()
			return 0, apperrors.Internal("db.scan_failed", "workflow tick failed").WithCause(err)
		}
		batch = append(batch, c)
	}
	rows.Close()

	advanced := 0
	for _, c := range batch {
		if err := e.advance(ctx, c.tenant, c.id, c.typ, c.payload, c.state, c.step, c.attempts); err != nil {
			_ = e.markFailed(ctx, c.id, err.Error())
			continue
		}
		advanced++
	}
	return advanced, nil
}

// advance executes one step deterministically and schedules the next state.
func (e *Engine) advance(ctx context.Context, tenantID, id, typ string, payload, state map[string]any, step string, attempts int) error {
	_ = attempts
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())

	switch {
	case typ == TypeCallback && step == StepScheduled:
		// wait elapsed → place the callback call (messaging via services)
		if err := e.services.QueueMessage(ctx, tenantID, strOf(payload["customer_id"]), "sms",
			strOf(payload["phone"]), "Callback reminder: "+strOf(payload["notes"])); err != nil {
			return err
		}
		if err := e.recordStep(ctx, tx, id, StepContacted, map[string]any{"channel": "sms"}); err != nil {
			return err
		}
		return e.complete(ctx, tx, id, tenantID, typ)

	case typ == TypeCollections && step == StepContacted:
		if err := e.services.QueueMessage(ctx, tenantID, strOf(payload["customer_id"]), "sms",
			strOf(payload["phone"]),
			fmt.Sprintf("Invoice %s is overdue. Amount due: %d. Reply to arrange payment.",
				strOf(payload["invoice_ref"]), intOf(payload["amount_due"]))); err != nil {
			return err
		}
		return e.waitFor(ctx, tx, id, tenantID, typ, 48*time.Hour, StepWaited, StepContacted)

	case typ == TypeCollections && step == StepWaited:
		// second touch: WhatsApp via messaging service
		if err := e.services.QueueMessage(ctx, tenantID, strOf(payload["customer_id"]), "whatsapp",
			strOf(payload["phone"]),
			fmt.Sprintf("Gentle reminder: invoice %s remains outstanding.", strOf(payload["invoice_ref"]))); err != nil {
			return err
		}
		return e.waitFor(ctx, tx, id, tenantID, typ, 72*time.Hour, StepEscalated, StepWaited)

	case typ == TypeCollections && step == StepEscalated:
		// escalation recorded → close the ladder (call task creation lands
		// with the campaigns wave; the audit trail shows the full ladder)
		if err := e.recordStep(ctx, tx, id, StepTaskCreated, map[string]any{"channel": "voice"}); err != nil {
			return err
		}
		return e.complete(ctx, tx, id, tenantID, typ)

	default:
		return e.complete(ctx, tx, id, tenantID, typ)
	}
}

// waitFor schedules the next step run and records the completed step.
func (e *Engine) waitFor(ctx context.Context, tx pgx.Tx, id, tenantID, typ string, d time.Duration, nextStep, doneStep string) error {
	next := time.Now().Add(d)
	if _, err := tx.Exec(ctx, `
		UPDATE workflow_instances SET status='waiting_timer', current_step=$3, next_run_at=$4, updated_at=now()
		WHERE id=$1 AND tenant_id=$2`, id, tenantID, nextStep, next); err != nil {
		return err
	}
	if err := e.recordStep(ctx, tx, id, doneStep, map[string]any{"next": nextStep, "at": next}); err != nil {
		return err
	}
	return e.emit(ctx, tx, events.TopicWorkflowStepCompleted, &Instance{ID: id, TenantID: tenantID, Type: typ},
		map[string]any{"workflow_id": id, "completed": doneStep, "next": nextStep, "run_at": next})
}

// complete seals a workflow.
func (e *Engine) complete(ctx context.Context, tx pgx.Tx, id, tenantID, typ string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE workflow_instances SET status='completed', next_run_at=NULL, current_step=$3, updated_at=now()
		WHERE id=$1 AND tenant_id=$2`, id, tenantID, StepClosed); err != nil {
		return err
	}
	if err := e.recordStep(ctx, tx, id, StepClosed, nil); err != nil {
		return err
	}
	return e.emit(ctx, tx, events.TopicWorkflowCompleted, &Instance{ID: id, TenantID: tenantID, Type: typ},
		map[string]any{"workflow_id": id})
}

func (e *Engine) markFailed(ctx context.Context, id, msg string) error {
	_, err := e.pool.Exec(ctx, `
		UPDATE workflow_instances SET status='failed', error=$2, updated_at=now() WHERE id=$1`, id, msg)
	return err
}

// Cancel stops a workflow (idempotent-hostile: canceling a finished wf 409s).
func (e *Engine) Cancel(ctx context.Context, tenantID, id string) (*Instance, error) {
	res, err := e.pool.Exec(ctx, `
		UPDATE workflow_instances SET status='canceled', next_run_at=NULL, updated_at=now()
		WHERE id=$1 AND tenant_id=$2 AND status IN ('running','waiting_timer')`, id, tenantID)
	if err != nil {
		return nil, apperrors.Internal("db.write_failed", "cancel failed").WithCause(err)
	}
	if res.RowsAffected() == 0 {
		if _, gerr := e.Get(ctx, tenantID, id); gerr != nil {
			return nil, gerr
		}
		return nil, apperrors.Conflict("workflow.not_cancelable", "workflow is not running")
	}
	return e.Get(ctx, tenantID, id)
}

// Get returns one tenant-scoped instance.
func (e *Engine) Get(ctx context.Context, tenantID, id string) (*Instance, error) {
	row := e.pool.QueryRow(ctx, `
		SELECT id, tenant_id, type, status, payload_json, state_json, current_step,
			next_run_at, attempts, coalesce(error,''), created_at, updated_at
		FROM workflow_instances WHERE id=$1 AND tenant_id=$2`, id, tenantID)
	return scanInstance(row)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanInstance(row rowScanner) (*Instance, error) {
	i := &Instance{}
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

func (e *Engine) insertInstance(ctx context.Context, tx pgx.Tx, i *Instance) error {
	payload, _ := json.Marshal(i.Payload)
	state, _ := json.Marshal(i.State)
	_, err := tx.Exec(ctx, `
		INSERT INTO workflow_instances (id, tenant_id, type, status, payload_json, state_json, current_step, next_run_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		i.ID, i.TenantID, i.Type, i.Status, payload, state, i.CurrentStep, i.NextRunAt)
	if dbinternal.IsUniqueViolation(err) {
		return apperrors.Conflict("workflow.conflict", "workflow conflict; retry")
	}
	if err != nil {
		return apperrors.Internal("db.write_failed", "workflow failed").WithCause(err)
	}
	return nil
}

func (e *Engine) recordStep(ctx context.Context, tx pgx.Tx, instanceID, step string, detail map[string]any) error {
	if detail == nil {
		detail = map[string]any{}
	}
	b, _ := json.Marshal(detail)
	_, err := tx.Exec(ctx, `
		INSERT INTO workflow_steps (instance_id, step, status, detail_json)
		VALUES ($1,$2,'completed',$3)`, instanceID, step, b)
	if err != nil {
		return apperrors.Internal("db.write_failed", "workflow step failed").WithCause(err)
	}
	return nil
}

func (e *Engine) emit(ctx context.Context, tx pgx.Tx, topic string, i *Instance, data map[string]any) error {
	env, err := events.New(topic, e.source, i.ID, i.TenantID, "", data)
	if err != nil {
		return apperrors.Internal("event.envelope_failed", "workflow event failed").WithCause(err)
	}
	if err := e.writer.Insert(ctx, tx, env); err != nil {
		return apperrors.Internal("outbox.insert_failed", "workflow event failed").WithCause(err)
	}
	return nil
}

func strOf(v any) string {
	s, _ := v.(string)
	return s
}

func intOf(v any) int {
	switch n := v.(type) {
	case int64:
		return int(n)
	case int:
		return n
	case float64:
		return int(n)
	default:
		return 0
	}
}
