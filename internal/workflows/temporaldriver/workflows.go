//go:build temporal

package temporaldriver

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/Roy-Wanyoike/orvexa/internal/workflows"
)

// The workflow bodies below are a 1:1 transcription of the engine's
// deterministic step machine (workflows.Engine.advance):
//
//	callback:    scheduled --(timer until ScheduleAt)--> contact via SMS --> closed
//	collections: contacted --(SMS now)--> waited --(48h)--> escalated --(72h)-->
//	             task_created --> closed
//
// Determinism rules: no wall-clock reads (workflow.Now only), no goroutines,
// all step data travels in the serializable inputs; side effects (messages,
// audit writes) are activities.

// CallbackInput is the serializable callback payload (engine parity with
// workflows.StartCallbackInput + audit identity).
type CallbackInput struct {
	InstanceID string    `json:"instance_id"`
	TenantID   string    `json:"tenant_id"`
	CustomerID string    `json:"customer_id"`
	Phone      string    `json:"phone"`
	Notes      string    `json:"notes"`
	ScheduleAt time.Time `json:"schedule_at"`
}

// CollectionsInput is the serializable collections payload (engine parity
// with workflows.StartCollectionsInput + audit identity).
type CollectionsInput struct {
	InstanceID string `json:"instance_id"`
	TenantID   string `json:"tenant_id"`
	CustomerID string `json:"customer_id"`
	InvoiceRef string `json:"invoice_ref"`
	AmountDue  int64  `json:"amount_due"`
	Phone      string `json:"phone"`
}

// WorkflowResult reports terminal completion.
type WorkflowResult struct {
	Completed bool `json:"completed"`
}

// Ladder waits, transcribed from the engine (48h between SMS and WhatsApp,
// 72h before escalation). Vars only so the test environment can shrink them
// if a future suite needs it; production values are the engine's.
var (
	collectionsWaitSecond = 48 * time.Hour
	collectionsWaitThird  = 72 * time.Hour
)

// CallbackWorkflow: schedule → wait (durable timer) → contact via SMS → close.
func (a *Activities) CallbackWorkflow(ctx workflow.Context, in CallbackInput) (WorkflowResult, error) {
	ctx = workflow.WithActivityOptions(ctx, stepActivityOptions())

	// Durable wait until ScheduleAt. Past schedules run immediately — engine
	// Tick parity (next_run_at <= now advances).
	if d := in.ScheduleAt.Sub(workflow.Now(ctx)); d > 0 {
		if err := workflow.NewTimer(ctx, d).Get(ctx, nil); err != nil {
			if temporal.IsCanceledError(ctx.Err()) {
				a.recordCanceled(ctx, in.InstanceID, in.TenantID, workflows.TypeCallback)
			}
			return WorkflowResult{}, ctx.Err()
		}
	}

	if err := a.queue(ctx, QueueMessageInput{
		TenantID: in.TenantID, CustomerID: in.CustomerID, Channel: "sms",
		To: in.Phone, Body: "Callback reminder: " + in.Notes,
	}); err != nil {
		return a.fail(ctx, in.InstanceID, in.TenantID, workflows.TypeCallback, err)
	}
	if err := a.step(ctx, RecordStepInput{
		InstanceID: in.InstanceID, TenantID: in.TenantID, Type: workflows.TypeCallback,
		ExpectedPrev: workflows.StepScheduled, DoneStep: workflows.StepContacted,
		Detail: map[string]any{"channel": "sms"},
	}); err != nil {
		return a.fail(ctx, in.InstanceID, in.TenantID, workflows.TypeCallback, err)
	}
	return a.complete(ctx, in.InstanceID, in.TenantID, workflows.TypeCallback, workflows.StepContacted)
}

// CollectionsWorkflow: SMS now → 48h → WhatsApp → 72h → escalate → close.
func (a *Activities) CollectionsWorkflow(ctx workflow.Context, in CollectionsInput) (WorkflowResult, error) {
	ctx = workflow.WithActivityOptions(ctx, stepActivityOptions())

	// First touch fires immediately (engine parity: collections starts at
	// step 'contacted').
	if err := a.queue(ctx, QueueMessageInput{
		TenantID: in.TenantID, CustomerID: in.CustomerID, Channel: "sms", To: in.Phone,
		Body: fmt.Sprintf("Invoice %s is overdue. Amount due: %d. Reply to arrange payment.",
			in.InvoiceRef, in.AmountDue),
	}); err != nil {
		return a.fail(ctx, in.InstanceID, in.TenantID, workflows.TypeCollections, err)
	}
	if err := a.waitStep(ctx, in.InstanceID, in.TenantID, workflows.TypeCollections,
		workflows.StepContacted, workflows.StepWaited, collectionsWaitSecond); err != nil {
		return a.fail(ctx, in.InstanceID, in.TenantID, workflows.TypeCollections, err)
	}

	if err := a.queue(ctx, QueueMessageInput{
		TenantID: in.TenantID, CustomerID: in.CustomerID, Channel: "whatsapp", To: in.Phone,
		Body: fmt.Sprintf("Gentle reminder: invoice %s remains outstanding.", in.InvoiceRef),
	}); err != nil {
		return a.fail(ctx, in.InstanceID, in.TenantID, workflows.TypeCollections, err)
	}
	if err := a.waitStep(ctx, in.InstanceID, in.TenantID, workflows.TypeCollections,
		workflows.StepWaited, workflows.StepEscalated, collectionsWaitThird); err != nil {
		return a.fail(ctx, in.InstanceID, in.TenantID, workflows.TypeCollections, err)
	}

	// Escalation recorded (call-task creation lands with the campaigns wave;
	// the audit trail shows the full ladder — engine comment parity).
	if err := a.step(ctx, RecordStepInput{
		InstanceID: in.InstanceID, TenantID: in.TenantID, Type: workflows.TypeCollections,
		ExpectedPrev: workflows.StepEscalated, DoneStep: workflows.StepTaskCreated,
		Detail: map[string]any{"channel": "voice"},
	}); err != nil {
		return a.fail(ctx, in.InstanceID, in.TenantID, workflows.TypeCollections, err)
	}
	return a.complete(ctx, in.InstanceID, in.TenantID, workflows.TypeCollections, workflows.StepTaskCreated)
}

// waitStep is the engine's waitFor as one recorded step + one durable timer.
func (a *Activities) waitStep(ctx workflow.Context, instanceID, tenantID, typ, doneStep, nextStep string, d time.Duration) error {
	at := workflow.Now(ctx).Add(d)
	if err := a.step(ctx, RecordStepInput{
		InstanceID: instanceID, TenantID: tenantID, Type: typ,
		ExpectedPrev: doneStep, DoneStep: doneStep,
		Detail: map[string]any{"next": nextStep, "at": at},
		Next:   &NextStep{Step: nextStep, RunAt: at},
	}); err != nil {
		return err
	}
	if err := workflow.NewTimer(ctx, d).Get(ctx, nil); err != nil {
		if temporal.IsCanceledError(ctx.Err()) {
			a.recordCanceled(ctx, instanceID, tenantID, typ)
		}
		return ctx.Err()
	}
	return nil
}

func (a *Activities) queue(ctx workflow.Context, in QueueMessageInput) error {
	return workflow.ExecuteActivity(ctx, a.QueueMessageActivity, in).Get(ctx, nil)
}

func (a *Activities) step(ctx workflow.Context, in RecordStepInput) error {
	return workflow.ExecuteActivity(ctx, a.RecordStepActivity, in).Get(ctx, nil)
}

func (a *Activities) complete(ctx workflow.Context, instanceID, tenantID, typ, expectedPrev string) (WorkflowResult, error) {
	if err := workflow.ExecuteActivity(ctx, a.CompleteInstanceActivity, CompleteInput{
		InstanceID: instanceID, TenantID: tenantID, Type: typ,
		ExpectedPrev: expectedPrev, Detail: map[string]any{},
	}).Get(ctx, nil); err != nil {
		return WorkflowResult{}, err
	}
	return WorkflowResult{Completed: true}, nil
}

// fail records the terminal failure best-effort, then fails the workflow so
// Temporal's own failure view stays truthful (no masking).
func (a *Activities) fail(ctx workflow.Context, instanceID, tenantID, typ string, cause error) (WorkflowResult, error) {
	_ = workflow.ExecuteActivity(ctx, a.FailAuditActivity, AuditInput{
		InstanceID: instanceID, TenantID: tenantID, Type: typ, Error: cause.Error(),
	}).Get(ctx, nil)
	return WorkflowResult{}, cause
}

// recordCanceled runs on a disconnected context: audit must survive the
// cancellation that interrupted the workflow.
func (a *Activities) recordCanceled(ctx workflow.Context, instanceID, tenantID, typ string) {
	dctx, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()
	dctx = workflow.WithActivityOptions(dctx, terminalActivityOptions())
	_ = workflow.ExecuteActivity(dctx, a.CancelAuditActivity, AuditInput{
		InstanceID: instanceID, TenantID: tenantID, Type: typ,
	}).Get(dctx, nil)
}

// stepActivityOptions is the bounded retry policy for step activities. The
// engine fails a step on first error; the driver retries briefly first — an
// intentional, documented improvement (ADR-0009 Consequences): the instance
// only goes failed after the policy exhausts.
func stepActivityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		ScheduleToCloseTimeout: 10 * time.Minute,
		StartToCloseTimeout:    2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    time.Minute,
			MaximumAttempts:    5,
		},
	}
}

// terminalActivityOptions keeps terminal bookkeeping short: a failing audit
// backend must not stall cancellation for long.
func terminalActivityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		ScheduleToCloseTimeout: 30 * time.Second,
		StartToCloseTimeout:    10 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    500 * time.Millisecond,
			BackoffCoefficient: 2,
			MaximumAttempts:    2,
		},
	}
}

// RegisterWorkflows registers both workflows on a worker registry. The same
// *Activities must be passed to RegisterActivities so method-value names line
// up with client-side starts.
func RegisterWorkflows(r worker.WorkflowRegistry, a *Activities) {
	r.RegisterWorkflow(a.CallbackWorkflow)
	r.RegisterWorkflow(a.CollectionsWorkflow)
}
