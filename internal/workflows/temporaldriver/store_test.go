//go:build temporal

package temporaldriver

import (
	"context"
	"testing"
	"time"

	"github.com/Roy-Wanyoike/orvexa/internal/workflows"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

func newInst(t *testing.T, id, tenant, step string) *workflows.Instance {
	t.Helper()
	when := time.Now().Add(time.Hour)
	return &workflows.Instance{
		ID: id, TenantID: tenant, Type: workflows.TypeCallback,
		Status: workflows.StatusWaitingTimer, Payload: map[string]any{"k": "v"},
		State: map[string]any{}, CurrentStep: step, NextRunAt: &when,
	}
}

func TestMemoryStoreStartInstanceConflict(t *testing.T) {
	m := NewMemoryStore()
	ctx := context.Background()
	mustNoErr(t, m.StartInstance(ctx, StartRecord{Instance: newInst(t, "i1", "t1", workflows.StepScheduled)}))
	err := m.StartInstance(ctx, StartRecord{Instance: newInst(t, "i1", "t1", workflows.StepScheduled)})
	wantErrCode(t, err, apperrors.KindConflict, "workflow.conflict")
}

func TestMemoryStoreRecordStepCAS(t *testing.T) {
	m := NewMemoryStore()
	ctx := context.Background()
	mustNoErr(t, m.StartInstance(ctx, StartRecord{Instance: newInst(t, "i1", "t1", workflows.StepScheduled)}))

	// Stale ExpectedPrev: a retried activity arriving late is a no-op, not an error.
	mustNoErr(t, m.RecordStep(ctx, StepRecord{
		InstanceID: "i1", TenantID: "t1", Type: workflows.TypeCallback,
		ExpectedPrev: workflows.StepContacted, // store still holds scheduled
		DoneStep:     workflows.StepContacted,
	}))
	inst, err := m.GetInstance(ctx, "t1", "i1")
	mustNoErr(t, err)
	if inst.CurrentStep != workflows.StepScheduled || len(m.Steps("i1")) != 1 {
		t.Fatalf("stale CAS must not advance: %+v", inst)
	}

	// Matching ExpectedPrev without Next: step recorded, instance running.
	mustNoErr(t, m.RecordStep(ctx, StepRecord{
		InstanceID: "i1", TenantID: "t1", Type: workflows.TypeCallback,
		ExpectedPrev: workflows.StepScheduled, DoneStep: workflows.StepContacted,
		Detail: map[string]any{"channel": "sms"},
	}))
	inst, err = m.GetInstance(ctx, "t1", "i1")
	mustNoErr(t, err)
	if inst.CurrentStep != workflows.StepContacted || inst.Status != workflows.StatusRunning {
		t.Fatalf("step without Next should run in place: %+v", inst)
	}
	steps := m.Steps("i1")
	if len(steps) != 2 || steps[1].Detail["channel"] != "sms" {
		t.Fatalf("step trace not recorded with detail: %+v", steps)
	}

	// Matching ExpectedPrev with Next: waitFor equivalent — waiting_timer + runAt.
	runAt := time.Now().Add(48 * time.Hour)
	mustNoErr(t, m.RecordStep(ctx, StepRecord{
		InstanceID: "i1", TenantID: "t1", Type: workflows.TypeCallback,
		ExpectedPrev: workflows.StepContacted, DoneStep: workflows.StepContacted,
		Next: &NextStep{Step: workflows.StepWaited, RunAt: runAt},
	}))
	inst, err = m.GetInstance(ctx, "t1", "i1")
	mustNoErr(t, err)
	if inst.Status != workflows.StatusWaitingTimer || inst.CurrentStep != workflows.StepWaited {
		t.Fatalf("waitFor parity broken: %+v", inst)
	}
	if inst.NextRunAt == nil || !inst.NextRunAt.Equal(runAt) {
		t.Fatalf("NextRunAt: want %v, got %v", runAt, inst.NextRunAt)
	}
}

func TestMemoryStoreTerminalStates(t *testing.T) {
	m := NewMemoryStore()
	ctx := context.Background()
	mustNoErr(t, m.StartInstance(ctx, StartRecord{Instance: newInst(t, "i1", "t1", workflows.StepContacted)}))

	// Complete behind CAS; a second complete with a stale guard is a no-op.
	mustNoErr(t, m.CompleteInstance(ctx, DoneRecord{InstanceID: "i1", TenantID: "t1", ExpectedPrev: workflows.StepContacted}))
	inst, _ := m.GetInstance(ctx, "t1", "i1")
	if inst.Status != workflows.StatusCompleted || inst.CurrentStep != workflows.StepClosed {
		t.Fatalf("complete parity broken: %+v", inst)
	}
	mustNoErr(t, m.CompleteInstance(ctx, DoneRecord{InstanceID: "i1", TenantID: "t1", ExpectedPrev: workflows.StepContacted}))
	if steps := m.Steps("i1"); len(steps) != 2 { // scheduled+contacted trace + closed, closed recorded once
		t.Fatalf("double complete must be idempotent: %+v", steps)
	}

	// Terminal rows are never re-failed or re-canceled.
	mustNoErr(t, m.MarkFailed(ctx, DoneRecord{InstanceID: "i1", TenantID: "t1", Error: "late failure"}))
	inst, _ = m.GetInstance(ctx, "t1", "i1")
	if inst.Status != workflows.StatusCompleted {
		t.Fatalf("completed must be immutable: %+v", inst)
	}

	// MarkFailed preserves the reason on live instances.
	mustNoErr(t, m.StartInstance(ctx, StartRecord{Instance: newInst(t, "i2", "t1", workflows.StepScheduled)}))
	mustNoErr(t, m.MarkFailed(ctx, DoneRecord{InstanceID: "i2", TenantID: "t1", Error: "boom"}))
	inst, _ = m.GetInstance(ctx, "t1", "i2")
	if inst.Status != workflows.StatusFailed || inst.Error != "boom" {
		t.Fatalf("failed parity broken: %+v", inst)
	}

	// Tenancy: foreign-tenant reads are indistinguishable not_found.
	_, err := m.GetInstance(ctx, "other", "i1")
	wantErrCode(t, err, apperrors.KindNotFound, "workflow.not_found")
	_, err = m.GetInstance(ctx, "t1", "missing")
	wantErrCode(t, err, apperrors.KindNotFound, "workflow.not_found")

	// Cancel only live instances.
	mustNoErr(t, m.StartInstance(ctx, StartRecord{Instance: newInst(t, "i3", "t1", workflows.StepScheduled)}))
	mustNoErr(t, m.MarkCanceled(ctx, DoneRecord{InstanceID: "i3", TenantID: "t1"}))
	inst, _ = m.GetInstance(ctx, "t1", "i3")
	if inst.Status != workflows.StatusCanceled || inst.NextRunAt != nil {
		t.Fatalf("cancel parity broken: %+v", inst)
	}

	// Defensive copies: mutating a returned instance must not corrupt the store.
	got, _ := m.GetInstance(ctx, "t1", "i3")
	got.Status = "smuggled"
	again, _ := m.GetInstance(ctx, "t1", "i3")
	if again.Status != workflows.StatusCanceled {
		t.Fatalf("store leaked mutable state: %+v", again)
	}
}
