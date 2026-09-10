//go:build temporal

package temporaldriver

import (
	"context"

	"go.temporal.io/sdk/worker"

	"github.com/Roy-Wanyoike/orvexa/internal/workflows"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// Activities bundles the activity implementations (and, via methods, the
// workflow definitions) a Temporal worker hosts. The same struct is used by
// the API-side Driver for workflow-function references.
type Activities struct {
	// Services is the engine's message-queue port (workflows.Services).
	// Required for message-sending activities; the other activities tolerate
	// a nil Store (execution-only mode) but never a nil Services.
	Services workflows.Services
	// Store is the audit port. Optional: nil means execution-only (no
	// workflow_instances/workflow_steps rows are written — documented in
	// ADR-0009; the read API will not see these instances).
	Store Store
}

// QueueMessageInput carries one outbound platform message for a workflow step.
type QueueMessageInput struct {
	TenantID   string `json:"tenant_id"`
	CustomerID string `json:"customer_id"`
	Channel    string `json:"channel"`
	To         string `json:"to"`
	Body       string `json:"body"`
}

// QueueMessageActivity executes the engine's Services port as a Temporal
// activity: durable inputs, bounded server-side retries with backoff (the
// retry policy lives in stepActivityOptions). No credentials or message
// bodies are logged here — bodies belong to the messaging plane's own audit.
func (a *Activities) QueueMessageActivity(ctx context.Context, in QueueMessageInput) error {
	if a.Services == nil {
		return apperrors.Internal("temporal.services_unwired",
			"message services are not wired into the temporal worker")
	}
	return a.Services.QueueMessage(ctx, in.TenantID, in.CustomerID, in.Channel, in.To, in.Body)
}

// RecordStepInput advances the audit row after a step ran.
type RecordStepInput struct {
	InstanceID   string         `json:"instance_id"`
	TenantID     string         `json:"tenant_id"`
	Type         string         `json:"type"`
	ExpectedPrev string         `json:"expected_prev"`
	DoneStep     string         `json:"done_step"`
	Detail       map[string]any `json:"detail,omitempty"`
	Next         *NextStep      `json:"next,omitempty"`
}

// RecordStepActivity records the completed step and (with Next) schedules the
// durable timer's audit mirror. Idempotent via the current_step CAS guard.
func (a *Activities) RecordStepActivity(ctx context.Context, in RecordStepInput) error {
	if a.Store == nil {
		return nil // execution-only mode: no audit backend wired
	}
	return a.Store.RecordStep(ctx, StepRecord{
		InstanceID: in.InstanceID, TenantID: in.TenantID, Type: in.Type,
		ExpectedPrev: in.ExpectedPrev, DoneStep: in.DoneStep, Detail: in.Detail, Next: in.Next,
	})
}

// CompleteInput seals an instance.
type CompleteInput struct {
	InstanceID   string         `json:"instance_id"`
	TenantID     string         `json:"tenant_id"`
	Type         string         `json:"type"`
	ExpectedPrev string         `json:"expected_prev"`
	Detail       map[string]any `json:"detail,omitempty"`
}

// CompleteInstanceActivity is the terminal success transition.
func (a *Activities) CompleteInstanceActivity(ctx context.Context, in CompleteInput) error {
	if a.Store == nil {
		return nil
	}
	return a.Store.CompleteInstance(ctx, DoneRecord{
		InstanceID: in.InstanceID, TenantID: in.TenantID, Type: in.Type,
		ExpectedPrev: in.ExpectedPrev, Detail: in.Detail,
	})
}

// AuditInput carries terminal bookkeeping (cancel/failure) fields.
type AuditInput struct {
	InstanceID string `json:"instance_id"`
	TenantID   string `json:"tenant_id"`
	Type       string `json:"type"`
	Error      string `json:"error,omitempty"`
}

// CancelAuditActivity records a canceled instance (best-effort, disconnected
// context in the workflow — audit must survive cancellation).
func (a *Activities) CancelAuditActivity(ctx context.Context, in AuditInput) error {
	if a.Store == nil {
		return nil
	}
	return a.Store.MarkCanceled(ctx, DoneRecord{
		InstanceID: in.InstanceID, TenantID: in.TenantID, Type: in.Type,
	})
}

// FailAuditActivity preserves the failure reason (engine markFailed parity:
// err.Error() text only — wrapped causes stay in the Temporal failure view).
func (a *Activities) FailAuditActivity(ctx context.Context, in AuditInput) error {
	if a.Store == nil {
		return nil
	}
	return a.Store.MarkFailed(ctx, DoneRecord{
		InstanceID: in.InstanceID, TenantID: in.TenantID, Type: in.Type, Error: in.Error,
	})
}

// RegisterActivities registers every activity on a worker registry.
func RegisterActivities(r worker.ActivityRegistry, a *Activities) {
	r.RegisterActivity(a.QueueMessageActivity)
	r.RegisterActivity(a.RecordStepActivity)
	r.RegisterActivity(a.CompleteInstanceActivity)
	r.RegisterActivity(a.CancelAuditActivity)
	r.RegisterActivity(a.FailAuditActivity)
}
