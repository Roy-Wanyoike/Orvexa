//go:build temporal

package temporaldriver

import (
	"context"
	"errors"
	"sync"
	"testing"

	"go.temporal.io/api/enums/v1"
	workflowv1 "go.temporal.io/api/workflow/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	sdkclient "go.temporal.io/sdk/client"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// fakeClient is a Client fake: it records starts/cancels and is
// programmatically broken per test. No Temporal server, no network.
type fakeClient struct {
	mu sync.Mutex

	// behavior knobs
	startErr     error
	cancelErr    error
	describeResp *workflowservice.DescribeWorkflowExecutionResponse
	describeErr  error
	healthErr    error

	// recorded calls
	starts   []startCall
	cancels  []cancelCall
	closes   int
	healthed int
}

type startCall struct {
	Opts     sdkclient.StartWorkflowOptions
	Workflow any
	Args     []any
}

type cancelCall struct {
	WorkflowID string
	RunID      string
}

func (f *fakeClient) ExecuteWorkflow(ctx context.Context, opts sdkclient.StartWorkflowOptions, wf any, args ...any) (sdkclient.WorkflowRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, startCall{Opts: opts, Workflow: wf, Args: args})
	if f.startErr != nil {
		return nil, f.startErr
	}
	return &fakeRun{id: opts.ID}, nil
}

func (f *fakeClient) CancelWorkflow(ctx context.Context, workflowID, runID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels = append(f.cancels, cancelCall{WorkflowID: workflowID, RunID: runID})
	return f.cancelErr
}

func (f *fakeClient) DescribeWorkflowExecution(ctx context.Context, workflowID, runID string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	if f.describeResp != nil {
		return f.describeResp, nil
	}
	return &workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: &workflowv1.WorkflowExecutionInfo{
			Status: enums.WORKFLOW_EXECUTION_STATUS_RUNNING,
		},
	}, nil
}

func (f *fakeClient) CheckHealth(ctx context.Context, request *sdkclient.CheckHealthRequest) (*sdkclient.CheckHealthResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.healthed++
	if f.healthErr != nil {
		return nil, f.healthErr
	}
	return &sdkclient.CheckHealthResponse{}, nil
}

func (f *fakeClient) Close() { f.closes++ }

// snapshot copies the recorded calls under the lock (race-safe assertions).
func (f *fakeClient) snapshot() (starts []startCall, cancels []cancelCall, closes, healthed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]startCall(nil), f.starts...), append([]cancelCall(nil), f.cancels...), f.closes, f.healthed
}

// fakeRun is a minimal sdkclient.WorkflowRun.
type fakeRun struct{ id string }

func (r *fakeRun) GetID() string                               { return r.id }
func (r *fakeRun) GetRunID() string                            { return "run-" + r.id }
func (r *fakeRun) GetFirstExecutionRunID() string              { return r.id }
func (r *fakeRun) Get(ctx context.Context, valuePtr any) error { return nil }

func (r *fakeRun) GetWithOptions(ctx context.Context, valuePtr any, _ sdkclient.WorkflowRunGetOptions) error {
	return r.Get(ctx, valuePtr)
}

// fakeServices implements workflows.Services, recording every queued message.
// failChannel+failWith let tests inject carrier failures for one channel
// (the failed call is NOT recorded).
type fakeServices struct {
	mu          sync.Mutex
	failWith    error
	failChannel string
	messages    []queuedMessage
}

type queuedMessage struct {
	TenantID, CustomerID, Channel, To, Body string
}

func (s *fakeServices) QueueMessage(ctx context.Context, tenantID, customerID, typ, to, body string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failWith != nil && (s.failChannel == "" || s.failChannel == typ) {
		return s.failWith
	}
	s.messages = append(s.messages, queuedMessage{
		TenantID: tenantID, CustomerID: customerID, Channel: typ, To: to, Body: body,
	})
	return nil
}

func (s *fakeServices) queued() []queuedMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]queuedMessage(nil), s.messages...)
}

// ─── assertion helpers (stdlib testing; no testify in this repo) ────────────

func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// wantErrCode asserts the canonical error contract: kind + stable code.
func wantErrCode(t *testing.T, err error, kind apperrors.Kind, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error kind=%s code=%s, got nil", kind, code)
	}
	var ae *apperrors.Error
	if !errors.As(err, &ae) {
		t.Fatalf("expected *apperrors.Error, got %T: %v", err, err)
	}
	if ae.Kind != kind || ae.Code != code {
		t.Fatalf("expected kind=%s code=%s, got kind=%s code=%s (%v)", kind, code, ae.Kind, ae.Code, err)
	}
}
