//go:build temporal

package temporaldriver

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"github.com/Roy-Wanyoike/orvexa/internal/workflows"
)

// newTestEnv builds a server-less TestWorkflowEnvironment with the driver's
// workflows and activities registered against a MemoryStore + fakeServices
// (svc==nil gets a fresh recorder). The env's mock clock auto-forwards while
// the workflow is blocked on a timer, so the engine's 30m/48h/72h waits
// complete instantly — the ladder runs to completion in milliseconds.
func newTestEnv(t *testing.T, svc *fakeServices) (*testsuite.TestWorkflowEnvironment, *MemoryStore, *fakeServices, *Activities) {
	t.Helper()
	store := NewMemoryStore()
	if svc == nil {
		svc = &fakeServices{}
	}
	acts := &Activities{Services: svc, Store: store}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(acts.CallbackWorkflow)
	env.RegisterWorkflow(acts.CollectionsWorkflow)
	env.RegisterActivity(acts.QueueMessageActivity)
	env.RegisterActivity(acts.RecordStepActivity)
	env.RegisterActivity(acts.CompleteInstanceActivity)
	env.RegisterActivity(acts.CancelAuditActivity)
	env.RegisterActivity(acts.FailAuditActivity)
	return env, store, svc, acts
}

// seed writes the audit row the Driver.start would have written before the
// workflow began (activities advance it via CAS — they never fabricate one).
func seed(t *testing.T, store *MemoryStore, id, tenant, typ, step string) {
	t.Helper()
	when := time.Now().Add(time.Hour)
	mustNoErr(t, store.StartInstance(context.Background(), StartRecord{
		Instance: &workflows.Instance{
			ID: id, TenantID: tenant, Type: typ, Status: workflows.StatusWaitingTimer,
			Payload: map[string]any{}, State: map[string]any{},
			CurrentStep: step, NextRunAt: &when,
		},
		StepDetail: map[string]any{"scheduled_for": when},
	}))
}

func TestCallbackWorkflowHappyPath(t *testing.T) {
	env, store, svc, acts := newTestEnv(t, nil)

	in := CallbackInput{
		InstanceID: "cb-1", TenantID: "t1", CustomerID: "c1",
		Phone: "+254700000001", Notes: "hello", ScheduleAt: env.Now().Add(30 * time.Minute),
	}
	seed(t, store, in.InstanceID, "t1", workflows.TypeCallback, workflows.StepScheduled)
	env.ExecuteWorkflow(acts.CallbackWorkflow, in)

	var res WorkflowResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("workflow failed: %v", err)
	}
	if !res.Completed {
		t.Fatalf("expected Completed=true, got %+v", res)
	}

	// Engine parity: one SMS contact, audit sealed completed with the full trace.
	msgs := svc.queued()
	if len(msgs) != 1 || msgs[0].Channel != "sms" || msgs[0].To != "+254700000001" ||
		msgs[0].Body != "Callback reminder: hello" || msgs[0].TenantID != "t1" || msgs[0].CustomerID != "c1" {
		t.Fatalf("message mismatch: %+v", msgs)
	}
	inst, err := store.GetInstance(context.Background(), "t1", "cb-1")
	mustNoErr(t, err)
	if inst.Status != workflows.StatusCompleted || inst.CurrentStep != workflows.StepClosed {
		t.Fatalf("audit not sealed by the workflow: %+v", inst)
	}
	steps := store.Steps("cb-1")
	if len(steps) != 3 { // scheduled, contacted, closed
		t.Fatalf("expected scheduled->contacted->closed trace, got %+v", steps)
	}
	if steps[1].Step != workflows.StepContacted || steps[1].Detail["channel"] != "sms" {
		t.Fatalf("contacted step detail mismatch: %+v", steps[1])
	}
}

func TestCollectionsWorkflowFullLadder(t *testing.T) {
	env, store, svc, acts := newTestEnv(t, nil)

	in := CollectionsInput{
		InstanceID: "col-1", TenantID: "t1", CustomerID: "c1",
		InvoiceRef: "INV-9", AmountDue: 1500, Phone: "+254700000002",
	}
	seed(t, store, in.InstanceID, "t1", workflows.TypeCollections, workflows.StepContacted)
	env.ExecuteWorkflow(acts.CollectionsWorkflow, in)

	var res WorkflowResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("workflow failed: %v", err)
	}
	if !res.Completed {
		t.Fatalf("expected Completed=true, got %+v", res)
	}

	msgs := svc.queued()
	if len(msgs) != 2 {
		t.Fatalf("ladder must send SMS then WhatsApp, got %+v", msgs)
	}
	if msgs[0].Channel != "sms" || !strings.Contains(msgs[0].Body, "INV-9") || !strings.Contains(msgs[0].Body, "1500") {
		t.Fatalf("first touch mismatch: %+v", msgs[0])
	}
	if msgs[1].Channel != "whatsapp" || !strings.Contains(msgs[1].Body, "outstanding") {
		t.Fatalf("second touch mismatch: %+v", msgs[1])
	}

	inst, err := store.GetInstance(context.Background(), "t1", "col-1")
	mustNoErr(t, err)
	if inst.Status != workflows.StatusCompleted || inst.CurrentStep != workflows.StepClosed {
		t.Fatalf("audit not sealed: %+v", inst)
	}
	var got []string
	for _, s := range store.Steps("col-1") {
		got = append(got, s.Step)
	}
	// Engine waitFor parity: the done-step re-records the step that fired the
	// wait; the next step appears only as current_step until it runs. So the
	// trace is contacted(start) -> contacted(SMS) -> waited -> task_created -> closed.
	want := []string{workflows.StepContacted, workflows.StepContacted, workflows.StepWaited,
		workflows.StepTaskCreated, workflows.StepClosed}
	if !equalStrings(got, want) {
		t.Fatalf("ladder trace mismatch:\n got %v\nwant %v", got, want)
	}
	for _, s := range store.Steps("col-1") {
		if s.Step == workflows.StepTaskCreated && s.Detail["channel"] != "voice" {
			t.Fatalf("escalation must record the voice channel: %+v", s)
		}
	}
}

func TestCollectionsWorkflowMessageFailureMarksFailed(t *testing.T) {
	// The WhatsApp touch (post-48h-wait) fails; retries exhaust; the workflow
	// must surface the failure AND preserve the reason in the audit trail.
	env, store, _, acts := newTestEnv(t, &fakeServices{failChannel: "whatsapp", failWith: errors.New("carrier down")})

	in := CollectionsInput{
		InstanceID: "col-2", TenantID: "t1", CustomerID: "c1",
		InvoiceRef: "INV-9", AmountDue: 1500, Phone: "+254700000002",
	}
	seed(t, store, in.InstanceID, "t1", workflows.TypeCollections, workflows.StepContacted)
	env.ExecuteWorkflow(acts.CollectionsWorkflow, in)

	var res WorkflowResult
	if err := env.GetWorkflowResult(&res); err == nil {
		t.Fatal("workflow must surface the carrier failure, not mask it")
	} else if !strings.Contains(err.Error(), "carrier down") {
		t.Fatalf("failure cause must be preserved, got %v", err)
	}
	inst, gerr := store.GetInstance(context.Background(), "t1", "col-2")
	mustNoErr(t, gerr)
	if inst.Status != workflows.StatusFailed || !strings.Contains(inst.Error, "carrier down") {
		t.Fatalf("audit row must be failed with the reason preserved: %+v", inst)
	}
}

func TestCallbackWorkflowCancellationRecordsAudit(t *testing.T) {
	env, store, _, acts := newTestEnv(t, nil)

	in := CallbackInput{
		InstanceID: "cb-3", TenantID: "t1", CustomerID: "c1",
		Phone: "+254700000003", Notes: "later", ScheduleAt: env.Now().Add(120 * time.Hour),
	}
	seed(t, store, in.InstanceID, "t1", workflows.TypeCallback, workflows.StepScheduled)
	// Cancel mid-wait (durable timer): registered BEFORE ExecuteWorkflow so the
	// delayed callback lands while the workflow is still blocked on the timer.
	// The workflow records cancellation audit on a disconnected context, then
	// returns the cancellation error.
	env.RegisterDelayedCallback(func() { env.CancelWorkflow() }, 10*time.Hour)
	env.ExecuteWorkflow(acts.CallbackWorkflow, in)

	var res WorkflowResult
	if err := env.GetWorkflowResult(&res); err == nil {
		t.Fatal("canceled workflow must return a cancellation error")
	}
	inst, err := store.GetInstance(context.Background(), "t1", "cb-3")
	mustNoErr(t, err)
	if inst.Status != workflows.StatusCanceled {
		t.Fatalf("cancellation must be recorded in the audit trail: %+v", inst)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
