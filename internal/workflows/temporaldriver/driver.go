//go:build temporal

package temporaldriver

import (
	"context"
	"fmt"
	"time"

	sdkclient "go.temporal.io/sdk/client"

	"github.com/google/uuid"

	"github.com/Roy-Wanyoike/orvexa/internal/workflows"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// Driver is the Temporal-backed driver: it exposes the engine's workflow
// lifecycle (StartCallback/StartCollections/Get/Cancel) with Temporal owning
// execution (durable timers, retries) and the Store port owning the audit
// trail the read API serves from.
//
// It is deliberately NOT a drop-in *workflows.Engine replacement: the
// httpserver takes *workflows.Engine concretely, so adopting the driver is a
// wiring decision per deployment (ADR-0009 Migration), not a code swap here.
type Driver struct {
	client  Client
	cfg     Config
	store   Store
	nowFunc func() time.Time
}

// Option configures a Driver at construction.
type Option func(*Driver)

// WithStore wires the audit port. Without it the driver is execution-only:
// starts go to Temporal, but no workflow_instances rows are written, so
// Get/Cancel are unavailable and the read API sees nothing (documented in
// ADR-0009; prefer NewPostgresStore for engine-parity reads).
func WithStore(s Store) Option {
	return func(d *Driver) { d.store = s }
}

// New wraps an existing (possibly fake, for tests) Client.
func New(cl Client, cfg Config, opts ...Option) *Driver {
	d := &Driver{client: cl, cfg: cfg.withDefaults(), nowFunc: time.Now}
	for _, o := range opts {
		o(d)
	}
	return d
}

// Dial connects to the Temporal frontend and returns a ready Driver.
func Dial(ctx context.Context, cfg Config, opts ...Option) (*Driver, error) {
	cl, err := dialClient(ctx, cfg.withDefaults())
	if err != nil {
		return nil, err
	}
	return New(cl, cfg, opts...), nil
}

// Close releases the underlying connection.
func (d *Driver) Close() {
	d.client.Close()
}

// Healthy reports frontend reachability (ops/liveness probe).
func (d *Driver) Healthy(ctx context.Context) error {
	if _, err := d.client.CheckHealth(ctx, &sdkclient.CheckHealthRequest{}); err != nil {
		return apperrors.Internal("temporal.unhealthy", "temporal health check failed").WithCause(err)
	}
	return nil
}

// StartCallbackInput is re-exported input shape (engine parity).
type (
	// StartCallbackInput mirrors workflows.StartCallbackInput.
	StartCallbackInput = workflows.StartCallbackInput
	// StartCollectionsInput mirrors workflows.StartCollectionsInput.
	StartCollectionsInput = workflows.StartCollectionsInput
)

// StartCallback schedules a callback for a customer with engine-identical
// validation, engine-identical audit rows (when a Store is wired), and a
// Temporal workflow owning the durable wait and the contact step.
func (d *Driver) StartCallback(ctx context.Context, tenantID string, in StartCallbackInput) (*workflows.Instance, error) {
	if in.CustomerID == "" {
		return nil, apperrors.Invalid("workflow.customer_required", "customer_id is required")
	}
	if in.Phone == "" {
		return nil, apperrors.Invalid("workflow.phone_required", "phone is required")
	}
	when := in.ScheduleAt
	if when.IsZero() {
		when = d.nowFunc().Add(30 * time.Minute) // engine parity default
	}
	if when.Before(d.nowFunc()) {
		return nil, apperrors.Invalid("workflow.schedule_past", "schedule_at must be in the future")
	}
	wfIn := func(instanceID string) (any, any) {
		return workflowRefs.CallbackWorkflow, CallbackInput{
			InstanceID: instanceID, TenantID: tenantID, CustomerID: in.CustomerID,
			Phone: in.Phone, Notes: in.Notes, ScheduleAt: when,
		}
	}
	return d.start(ctx, tenantID, workflows.TypeCallback, map[string]any{
		"customer_id": in.CustomerID, "phone": in.Phone, "notes": in.Notes,
	}, when, workflows.StepScheduled, wfIn)
}

// StartCollections drives the collections ladder with engine-identical
// validation and payloads; the ladder itself runs as a Temporal workflow.
func (d *Driver) StartCollections(ctx context.Context, tenantID string, in StartCollectionsInput) (*workflows.Instance, error) {
	if in.CustomerID == "" || in.InvoiceRef == "" {
		return nil, apperrors.Invalid("workflow.fields_required", "customer_id and invoice_ref are required")
	}
	if in.AmountDue <= 0 {
		return nil, apperrors.Invalid("workflow.amount_invalid", "amount_due must be positive")
	}
	if in.Phone == "" {
		return nil, apperrors.Invalid("workflow.phone_required", "phone is required")
	}
	wfIn := func(instanceID string) (any, any) {
		return workflowRefs.CollectionsWorkflow, CollectionsInput{
			InstanceID: instanceID, TenantID: tenantID, CustomerID: in.CustomerID,
			InvoiceRef: in.InvoiceRef, AmountDue: in.AmountDue, Phone: in.Phone,
		}
	}
	return d.start(ctx, tenantID, workflows.TypeCollections, map[string]any{
		"customer_id": in.CustomerID, "invoice_ref": in.InvoiceRef,
		"amount_due": in.AmountDue, "phone": in.Phone,
	}, d.nowFunc(), workflows.StepContacted, wfIn)
}

// start persists the engine-parity instance (when a Store is wired) and hands
// execution to Temporal. The mk callback builds the workflow reference and its
// serializable input so the generated instance id travels with the workflow —
// the audit activities key every transition off it. If Temporal rejects the
// start, the audit row is marked failed so nothing is silently dropped — the
// inverse of degrade.
func (d *Driver) start(ctx context.Context, tenantID, typ string, payload map[string]any, when time.Time, firstStep string, mk func(instanceID string) (any, any)) (*workflows.Instance, error) {
	inst := &workflows.Instance{
		ID: uuid.NewString(), TenantID: tenantID, Type: typ,
		// Engine parity: start() hardcodes waiting_timer (the first claimable
		// state); the driver preserves that for read-path consistency.
		Status:  workflows.StatusWaitingTimer,
		Payload: payload, State: map[string]any{},
		CurrentStep: firstStep, NextRunAt: &when,
	}
	if d.store != nil {
		if err := d.store.StartInstance(ctx, StartRecord{
			Instance: inst, StepDetail: map[string]any{"scheduled_for": when},
		}); err != nil {
			return nil, err
		}
	}
	wf, wfInput := mk(inst.ID)
	if _, err := d.client.ExecuteWorkflow(ctx, sdkclient.StartWorkflowOptions{
		ID:        WorkflowID(inst.ID),
		TaskQueue: d.cfg.TaskQueue,
	}, wf, wfInput); err != nil {
		if d.store != nil {
			_ = d.store.MarkFailed(ctx, DoneRecord{
				InstanceID: inst.ID, TenantID: tenantID, Type: typ,
				Error: err.Error(), // engine parity: reason preserved on the row
			})
		}
		return nil, apperrors.Internal("temporal.start_failed", "workflow start failed").WithCause(err)
	}
	return inst, nil
}

// Get returns one tenant-scoped instance from the audit store (engine.Get
// parity). Requires a Store.
func (d *Driver) Get(ctx context.Context, tenantID, id string) (*workflows.Instance, error) {
	if d.store == nil {
		return nil, errStoreRequired()
	}
	return d.store.GetInstance(ctx, tenantID, id)
}

// Cancel stops a live workflow with engine-identical semantics: unknown or
// foreign-tenant ids are not_found; terminal instances are a 409 conflict
// (same codes as workflows.Engine.Cancel); success cancels on Temporal AND
// seals the audit row.
func (d *Driver) Cancel(ctx context.Context, tenantID, id string) (*workflows.Instance, error) {
	if d.store == nil {
		return nil, errStoreRequired()
	}
	cur, err := d.store.GetInstance(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if cur.Status != workflows.StatusRunning && cur.Status != workflows.StatusWaitingTimer {
		return nil, apperrors.Conflict("workflow.not_cancelable", "workflow is not running")
	}
	if err := d.client.CancelWorkflow(ctx, WorkflowID(id), ""); err != nil {
		if isNotFound(err) {
			return nil, apperrors.NotFound("workflow.not_found", "workflow not found")
		}
		return nil, apperrors.Internal("temporal.cancel_failed", "cancel failed").WithCause(err)
	}
	if err := d.store.MarkCanceled(ctx, DoneRecord{
		InstanceID: id, TenantID: tenantID, Type: cur.Type,
	}); err != nil {
		return nil, err
	}
	return d.store.GetInstance(ctx, tenantID, id)
}

// TemporalStatus returns the server-side execution status string for
// operators (e.g. "WORKFLOW_EXECUTION_STATUS_RUNNING"). The public API does
// NOT expose this — audit status (Store) remains the read-path source of
// truth; this is an ops window into Temporal itself.
func (d *Driver) TemporalStatus(ctx context.Context, instanceID string) (string, error) {
	resp, err := d.client.DescribeWorkflowExecution(ctx, WorkflowID(instanceID), "")
	if err != nil {
		if isNotFound(err) {
			return "", apperrors.NotFound("workflow.not_found", "workflow not found")
		}
		return "", apperrors.Internal("temporal.describe_failed", "describe failed").WithCause(err)
	}
	info := resp.GetWorkflowExecutionInfo()
	if info == nil {
		return "unknown", nil
	}
	return info.GetStatus().String(), nil
}

// errStoreRequired is the loud failure for read/cancel without an audit
// backend — never a silent empty result.
func errStoreRequired() error {
	return apperrors.Internal("temporal.store_required",
		fmt.Sprintf("this driver instance has no audit store; construct it with WithStore(NewPostgresStore(pool, %q)) or WithStore(NewMemoryStore())", DefaultSource))
}

// workflowRefs references the workflow function symbols for client-side
// starts. The starting process never executes these bodies; Temporal
// resolves the registered name on the worker hosting real Activities.
var workflowRefs = new(Activities)
