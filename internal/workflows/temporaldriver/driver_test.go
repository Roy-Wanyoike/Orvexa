//go:build temporal

package temporaldriver

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	serviceerror "go.temporal.io/api/serviceerror"

	"github.com/Roy-Wanyoike/orvexa/internal/workflows"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

func newTestDriver(t *testing.T) (*Driver, *fakeClient, *MemoryStore) {
	t.Helper()
	cl := &fakeClient{}
	store := NewMemoryStore()
	d := New(cl, Config{HostPort: "unused:7233", TaskQueue: "wf-test", Source: "orvexa-worker"}, WithStore(store))
	return d, cl, store
}

func TestDriverStartCallbackEngineParity(t *testing.T) {
	d, cl, store := newTestDriver(t)
	when := time.Now().Add(time.Hour)

	inst, err := d.StartCallback(context.Background(), "t1", workflows.StartCallbackInput{
		CustomerID: "c1", Phone: "+254700000001", ScheduleAt: when, Notes: "hi",
	})
	mustNoErr(t, err)

	if inst.TenantID != "t1" || inst.Type != workflows.TypeCallback ||
		inst.Status != workflows.StatusWaitingTimer || inst.CurrentStep != workflows.StepScheduled {
		t.Fatalf("instance not engine parity: %+v", inst)
	}
	if inst.NextRunAt == nil || !inst.NextRunAt.Equal(when) {
		t.Fatalf("NextRunAt: want %v, got %v", when, inst.NextRunAt)
	}

	starts, _, _, _ := cl.snapshot()
	if len(starts) != 1 {
		t.Fatalf("expected exactly one workflow start, got %d", len(starts))
	}
	sc := starts[0]
	if sc.Opts.ID != WorkflowID(inst.ID) {
		t.Fatalf("workflow id: want %q, got %q", WorkflowID(inst.ID), sc.Opts.ID)
	}
	if sc.Opts.TaskQueue != "wf-test" {
		t.Fatalf("task queue: want %q, got %q", "wf-test", sc.Opts.TaskQueue)
	}
	if len(sc.Args) != 1 {
		t.Fatalf("expected one serializable input arg, got %d", len(sc.Args))
	}
	in, ok := sc.Args[0].(CallbackInput)
	if !ok {
		t.Fatalf("arg type: want CallbackInput, got %T", sc.Args[0])
	}
	if in.InstanceID != inst.ID {
		// the workflow input must carry the instance id: every audit activity
		// keys its CAS transition off it
		t.Fatalf("input InstanceID = %q, want %q", in.InstanceID, inst.ID)
	}
	if in.TenantID != "t1" || in.CustomerID != "c1" || in.Phone != "+254700000001" || in.Notes != "hi" {
		t.Fatalf("input payload mismatch: %+v", in)
	}

	// Audit row mirrors engine.start: instance + scheduled step trace.
	got, err := store.GetInstance(context.Background(), "t1", inst.ID)
	mustNoErr(t, err)
	if got.CurrentStep != workflows.StepScheduled || got.Status != workflows.StatusWaitingTimer {
		t.Fatalf("audit row not engine parity: %+v", got)
	}
	if steps := store.Steps(inst.ID); len(steps) != 1 || steps[0].Step != workflows.StepScheduled {
		t.Fatalf("expected exactly the scheduled step trace, got %+v", store.Steps(inst.ID))
	}
}

func TestDriverStartCallbackValidationParity(t *testing.T) {
	d, cl, _ := newTestDriver(t)
	cases := []struct {
		name string
		in   workflows.StartCallbackInput
		kind apperrors.Kind
		code string
	}{
		{"customer required", workflows.StartCallbackInput{Phone: "+254700000001"}, apperrors.KindInvalid, "workflow.customer_required"},
		{"phone required", workflows.StartCallbackInput{CustomerID: "c1"}, apperrors.KindInvalid, "workflow.phone_required"},
		{"past schedule", workflows.StartCallbackInput{CustomerID: "c1", Phone: "p", ScheduleAt: time.Now().Add(-time.Minute)}, apperrors.KindInvalid, "workflow.schedule_past"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := d.StartCallback(context.Background(), "t1", tc.in)
			wantErrCode(t, err, tc.kind, tc.code)
			starts, _, _, _ := cl.snapshot()
			if len(starts) != 0 {
				t.Fatalf("rejected start must not reach Temporal, got %d starts", len(starts))
			}
		})
	}
}

func TestDriverStartCollectionsDefaultsScheduleToNow(t *testing.T) {
	d, cl, store := newTestDriver(t)
	before := time.Now()
	inst, err := d.StartCollections(context.Background(), "t1", workflows.StartCollectionsInput{
		CustomerID: "c1", InvoiceRef: "INV-1", AmountDue: 500, Phone: "+254700000001",
	})
	mustNoErr(t, err)
	if inst.CurrentStep != workflows.StepContacted || inst.Status != workflows.StatusWaitingTimer {
		t.Fatalf("collections must start at contacted/waiting_timer (engine parity): %+v", inst)
	}
	if inst.NextRunAt == nil || inst.NextRunAt.Before(before) {
		t.Fatalf("NextRunAt should default to now, got %v", inst.NextRunAt)
	}
	starts, _, _, _ := cl.snapshot()
	if len(starts) != 1 {
		t.Fatalf("expected one start, got %d", len(starts))
	}
	in, ok := starts[0].Args[0].(CollectionsInput)
	if !ok {
		t.Fatalf("arg type: want CollectionsInput, got %T", starts[0].Args[0])
	}
	if in.AmountDue != 500 || in.InvoiceRef != "INV-1" {
		t.Fatalf("input payload mismatch: %+v", in)
	}
	if got, err := store.GetInstance(context.Background(), "t1", inst.ID); err != nil || got.Type != workflows.TypeCollections {
		t.Fatalf("audit row missing/mistyped: %v %+v", err, got)
	}
}

func TestDriverStartCollectionsValidationParity(t *testing.T) {
	d, _, _ := newTestDriver(t)
	cases := []struct {
		name string
		in   workflows.StartCollectionsInput
		code string
	}{
		{"fields required", workflows.StartCollectionsInput{CustomerID: "c1", AmountDue: 1, Phone: "p"}, "workflow.fields_required"},
		{"amount invalid", workflows.StartCollectionsInput{CustomerID: "c1", InvoiceRef: "INV", AmountDue: 0, Phone: "p"}, "workflow.amount_invalid"},
		{"phone required", workflows.StartCollectionsInput{CustomerID: "c1", InvoiceRef: "INV", AmountDue: 1}, "workflow.phone_required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := d.StartCollections(context.Background(), "t1", tc.in)
			wantErrCode(t, err, apperrors.KindInvalid, tc.code)
		})
	}
}

func TestDriverStartTemporalFailureMarksRowFailed(t *testing.T) {
	cl := &fakeClient{startErr: errors.New("namespace suspended")}
	store := NewMemoryStore()
	d := New(cl, Config{HostPort: "x:7233"}, WithStore(store))

	inst, err := d.StartCallback(context.Background(), "t1", workflows.StartCallbackInput{
		CustomerID: "c1", Phone: "p", ScheduleAt: time.Now().Add(time.Hour),
	})
	if inst != nil {
		t.Fatalf("start must not return an instance on failure, got %+v", inst)
	}
	wantErrCode(t, err, apperrors.KindInternal, "temporal.start_failed")
	var ae *apperrors.Error
	if !errors.As(err, &ae) || ae.Unwrap() == nil || !strings.Contains(ae.Unwrap().Error(), "namespace suspended") {
		t.Fatalf("cause must be preserved for triage, got %v", err)
	}

	got, gerr := store.GetInstance(context.Background(), "t1", instIDOf(t, cl))
	mustNoErr(t, gerr)
	if got.Status != workflows.StatusFailed || got.Error == "" {
		t.Fatalf("audit row must be failed with reason preserved: %+v", got)
	}
}

// instIDOf extracts the instance id the driver derived for the single start
// call (the fake saw the workflow id; the audit row is keyed by the same uuid).
func instIDOf(t *testing.T, cl *fakeClient) string {
	t.Helper()
	starts, _, _, _ := cl.snapshot()
	if len(starts) != 1 {
		t.Fatalf("expected one start, got %d", len(starts))
	}
	if !strings.HasPrefix(starts[0].Opts.ID, workflowIDPrefix) {
		t.Fatalf("unexpected workflow id %q", starts[0].Opts.ID)
	}
	return strings.TrimPrefix(starts[0].Opts.ID, workflowIDPrefix)
}

func TestDriverStartWithoutStoreIsExecutionOnly(t *testing.T) {
	d := New(&fakeClient{}, Config{HostPort: "x:7233"})
	inst, err := d.StartCallback(context.Background(), "t1", workflows.StartCallbackInput{
		CustomerID: "c1", Phone: "p", ScheduleAt: time.Now().Add(time.Hour),
	})
	mustNoErr(t, err)
	if inst == nil || inst.ID == "" {
		t.Fatalf("execution-only start must still return the instance, got %+v", inst)
	}
	_, err = d.Get(context.Background(), "t1", inst.ID)
	wantErrCode(t, err, apperrors.KindInternal, "temporal.store_required")
	_, err = d.Cancel(context.Background(), "t1", inst.ID)
	wantErrCode(t, err, apperrors.KindInternal, "temporal.store_required")
}

func TestDriverCancelParity(t *testing.T) {
	t.Run("happy path cancels Temporal and seals the row", func(t *testing.T) {
		d, cl, _ := newTestDriver(t)
		inst, err := d.StartCallback(context.Background(), "t1", workflows.StartCallbackInput{
			CustomerID: "c1", Phone: "p", ScheduleAt: time.Now().Add(time.Hour),
		})
		mustNoErr(t, err)

		got, err := d.Cancel(context.Background(), "t1", inst.ID)
		mustNoErr(t, err)
		if got.Status != workflows.StatusCanceled {
			t.Fatalf("expected canceled, got %+v", got)
		}
		_, cancels, _, _ := cl.snapshot()
		if len(cancels) != 1 || cancels[0].WorkflowID != WorkflowID(inst.ID) || cancels[0].RunID != "" {
			t.Fatalf("cancel call mismatch: %+v", cancels)
		}
	})

	t.Run("unknown id is not_found", func(t *testing.T) {
		d, cl, _ := newTestDriver(t)
		_, err := d.Cancel(context.Background(), "t1", "nope")
		wantErrCode(t, err, apperrors.KindNotFound, "workflow.not_found")
		_, cancels, _, _ := cl.snapshot()
		if len(cancels) != 0 {
			t.Fatalf("not-found must not reach Temporal: %+v", cancels)
		}
	})

	t.Run("foreign tenant reads as not_found", func(t *testing.T) {
		d, _, _ := newTestDriver(t)
		inst, err := d.StartCallback(context.Background(), "t1", workflows.StartCallbackInput{
			CustomerID: "c1", Phone: "p", ScheduleAt: time.Now().Add(time.Hour),
		})
		mustNoErr(t, err)
		_, err = d.Cancel(context.Background(), "other", inst.ID)
		wantErrCode(t, err, apperrors.KindNotFound, "workflow.not_found")
	})

	t.Run("terminal instance is a conflict and never hits Temporal", func(t *testing.T) {
		d, cl, store := newTestDriver(t)
		inst, err := d.StartCallback(context.Background(), "t1", workflows.StartCallbackInput{
			CustomerID: "c1", Phone: "p", ScheduleAt: time.Now().Add(time.Hour),
		})
		mustNoErr(t, err)
		mustNoErr(t, store.MarkCanceled(context.Background(), DoneRecord{InstanceID: inst.ID, TenantID: "t1"}))

		_, err = d.Cancel(context.Background(), "t1", inst.ID)
		wantErrCode(t, err, apperrors.KindConflict, "workflow.not_cancelable")
		_, cancels, _, _ := cl.snapshot()
		if len(cancels) != 0 {
			t.Fatalf("terminal instance must not be re-canceled: %+v", cancels)
		}
	})

	t.Run("temporal not-found maps to not_found", func(t *testing.T) {
		cl := &fakeClient{cancelErr: serviceerror.NewNotFound("never started")}
		d := New(cl, Config{HostPort: "x:7233"}, WithStore(NewMemoryStore()))
		inst, err := d.StartCallback(context.Background(), "t1", workflows.StartCallbackInput{
			CustomerID: "c1", Phone: "p", ScheduleAt: time.Now().Add(time.Hour),
		})
		mustNoErr(t, err)
		_, err = d.Cancel(context.Background(), "t1", inst.ID)
		wantErrCode(t, err, apperrors.KindNotFound, "workflow.not_found")
	})
}

func TestDriverGetParity(t *testing.T) {
	d, _, _ := newTestDriver(t)
	inst, err := d.StartCallback(context.Background(), "t1", workflows.StartCallbackInput{
		CustomerID: "c1", Phone: "p", ScheduleAt: time.Now().Add(time.Hour),
	})
	mustNoErr(t, err)

	got, err := d.Get(context.Background(), "t1", inst.ID)
	mustNoErr(t, err)
	if got.ID != inst.ID {
		t.Fatalf("round-trip mismatch: %+v vs %+v", got, inst)
	}

	_, err = d.Get(context.Background(), "t1", "missing")
	wantErrCode(t, err, apperrors.KindNotFound, "workflow.not_found")
}

func TestDriverTemporalStatusOpsWindow(t *testing.T) {
	t.Run("running", func(t *testing.T) {
		d, _, _ := newTestDriver(t)
		status, err := d.TemporalStatus(context.Background(), "abc")
		mustNoErr(t, err)
		if status != "Running" { // enumspb.WorkflowExecutionStatusRunning.String()
			t.Fatalf("status = %q, want the server-side enum string", status)
		}
	})
	t.Run("not found", func(t *testing.T) {
		cl := &fakeClient{describeErr: serviceerror.NewNotFound("gone")}
		d := New(cl, Config{HostPort: "x:7233"})
		_, err := d.TemporalStatus(context.Background(), "abc")
		wantErrCode(t, err, apperrors.KindNotFound, "workflow.not_found")
	})
}

func TestDriverHealthy(t *testing.T) {
	d, cl, _ := newTestDriver(t)
	mustNoErr(t, d.Healthy(context.Background()))
	if _, _, _, healthed := cl.snapshot(); healthed != 1 {
		t.Fatalf("health check should hit the client once, got %d", healthed)
	}

	d2 := New(&fakeClient{healthErr: errors.New("front door shut")}, Config{HostPort: "x:7233"})
	wantErrCode(t, d2.Healthy(context.Background()), apperrors.KindInternal, "temporal.unhealthy")
}

func TestDriverCloseClosesClient(t *testing.T) {
	d, cl, _ := newTestDriver(t)
	d.Close()
	if _, _, closes, _ := cl.snapshot(); closes != 1 {
		t.Fatalf("Close must release the client connection, got %d closes", closes)
	}
}
