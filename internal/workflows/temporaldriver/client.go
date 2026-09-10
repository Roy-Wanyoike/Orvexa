//go:build temporal

package temporaldriver

import (
	"context"

	"go.temporal.io/api/serviceerror"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// Client is the subset of the Temporal SDK's client.Client the driver uses.
//
// Declaring the subset (rather than taking client.Client directly) documents
// the exact SDK surface in one place and lets unit tests substitute interface
// fakes — no Temporal server required. The SDK's concrete client satisfies it
// implicitly.
type Client interface {
	// ExecuteWorkflow starts a workflow execution on the configured task queue.
	ExecuteWorkflow(ctx context.Context, opts client.StartWorkflowOptions, workflow any, args ...any) (client.WorkflowRun, error)
	// CancelWorkflow requests cancellation of a workflow execution.
	CancelWorkflow(ctx context.Context, workflowID, runID string) error
	// DescribeWorkflowExecution returns server-side execution state (ops view).
	DescribeWorkflowExecution(ctx context.Context, workflowID, runID string) (*workflowservice.DescribeWorkflowExecutionResponse, error)
	// CheckHealth verifies frontend reachability.
	CheckHealth(ctx context.Context, request *client.CheckHealthRequest) (*client.CheckHealthResponse, error)
	// Close releases the underlying gRPC connection.
	Close()
}

// dialClient connects to the Temporal frontend described by cfg. Missing
// configuration is a validation error; unreachable frontends surface as
// temporal.dial_failed with the cause attached (never swallowed).
func dialClient(ctx context.Context, cfg Config) (Client, error) {
	if cfg.HostPort == "" {
		return nil, apperrors.Invalid("temporal.url_required",
			"ORVEXA_TEMPORAL_URL (host:port of the Temporal frontend) is required")
	}
	cl, err := client.DialContext(ctx, client.Options{
		HostPort:  cfg.HostPort,
		Namespace: cfg.Namespace,
	})
	if err != nil {
		return nil, apperrors.Internal("temporal.dial_failed", "temporal connect failed").WithCause(err)
	}
	return cl, nil
}

// workflowIDPrefix namespaces driver-started workflows on the Temporal
// namespace so operators can attribute them at a glance.
const workflowIDPrefix = "orvexa-workflow-"

// WorkflowID derives the Temporal workflow ID for an engine-parity instance
// id. The instance id (a uuid) stays the canonical key everywhere: audit rows,
// events, and the public API never expose the Temporal ID scheme.
func WorkflowID(instanceID string) string {
	return workflowIDPrefix + instanceID
}

// isNotFound reports whether err is Temporal's not-found service error
// (e.g. canceling a workflow that never started).
func isNotFound(err error) bool {
	_, ok := err.(*serviceerror.NotFound)
	return ok
}
