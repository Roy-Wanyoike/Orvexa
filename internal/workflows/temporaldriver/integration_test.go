//go:build temporal && integration

package temporaldriver_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Roy-Wanyoike/orvexa/internal/workflows"
	"github.com/Roy-Wanyoike/orvexa/internal/workflows/temporaldriver"
<<<<<<< HEAD
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
=======
>>>>>>> origin/main
)

// TestTemporalDriverIntegration proves the driver against a REAL Temporal
// frontend (issue #34 acceptance): a worker hosting the driver's workflows and
// activities runs in-process, the API-side Driver starts/cancels via the
// server, and the MemoryStore audit trail is advanced by the activities that
// land through the task queue — the same code path production uses.
//
// Gated twice, deliberately:
//   - build tag `integration` (repo convention — `make integration` suite);
//   - ORVEXA_TEMPORAL_URL: without a reachable Temporal frontend the test
//     skips cleanly (no fake pass, no hard failure), matching the docker
//     compose dev profile (`docker compose -f docker-compose.dev.yml --profile
//     temporal up -d`).
func TestTemporalDriverIntegration(t *testing.T) {
	cfg := temporaldriver.FromEnv()
	if cfg.HostPort == "" {
		t.Skip("ORVEXA_TEMPORAL_URL not set — no Temporal frontend to integrate with")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Unique task queue per run: parallel runs never steal each other's tasks.
	cfg.TaskQueue += "-" + uuid.NewString()[:8]

	// API side: engine-parity lifecycle over the real server. The MemoryStore
	// audit trail is advanced by the activities that land through the task
	// queue — the same CAS transitions the PostgresStore performs in prod.
	mem := temporaldriver.NewMemoryStore()
	d, err := temporaldriver.Dial(ctx, cfg, temporaldriver.WithStore(mem))
	if err != nil {
		t.Fatalf("dial %s: %v", cfg.HostPort, err)
	}
	defer d.Close()
	if err := d.Healthy(ctx); err != nil {
		t.Fatalf("frontend unhealthy: %v", err)
	}

	// Worker side: same shape as cmd/worker (Services + audit Store).
	svc := &recordServices{}
	workerCfg := cfg
	wcl, err := temporaldriver.DialSDK(ctx, workerCfg)
	if err != nil {
		t.Fatalf("worker dial: %v", err)
	}
	defer wcl.Close()
	wctx, wcancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- temporaldriver.RunWorker(wctx, wcl, workerCfg, &temporaldriver.Activities{Services: svc, Store: mem})
	}()
	defer func() {
		wcancel()
		if err := <-done; err != nil {
			t.Errorf("worker: %v", err)
		}
	}()

	t.Run("callback runs to completion", func(t *testing.T) {
		inst, err := d.StartCallback(ctx, "itn-1", temporaldriver.StartCallbackInput{
			CustomerID: "cust-1", Phone: "+254700000001", ScheduleAt: time.Now().Add(2 * time.Second), Notes: "it",
		})
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		waitStatus(t, ctx, d, "itn-1", inst.ID, workflows.StatusCompleted, 60*time.Second)
		if got := mem.Steps(inst.ID); len(got) < 3 {
			t.Fatalf("expected scheduled->contacted->closed trace, got %+v", got)
		}
		msgs := svc.queued()
<<<<<<< HEAD
		if len(msgs) != 1 || msgs[0] != "itn-1|sms||" {
			t.Fatalf("expected one tenant-scoped SMS contact (itn-1|sms||), got %+v", msgs)
=======
		if len(msgs) != 1 || msgs[0].Channel != "sms" || msgs[0].TenantID != "itn-1" {
			t.Fatalf("expected one tenant-scoped SMS contact, got %+v", msgs)
>>>>>>> origin/main
		}
	})

	t.Run("collections ladder completes", func(t *testing.T) {
		// Full ladder = 120h of durable timers. The workflow must simply run in
		// the background server-side; we assert the START + audit parity, not
		// the 5-day completion (see unit tests for the ladder end-to-end).
		inst, err := d.StartCollections(ctx, "itn-1", temporaldriver.StartCollectionsInput{
			CustomerID: "cust-2", InvoiceRef: "INV-IT", AmountDue: 100, Phone: "+254700000002",
		})
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		got, err := d.Get(ctx, "itn-1", inst.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.CurrentStep != workflows.StepContacted || got.Status != workflows.StatusWaitingTimer {
			t.Fatalf("collections start parity broken: %+v", got)
		}
	})

	t.Run("cancel stops a live workflow", func(t *testing.T) {
		inst, err := d.StartCallback(ctx, "itn-1", temporaldriver.StartCallbackInput{
			CustomerID: "cust-3", Phone: "+254700000003", ScheduleAt: time.Now().Add(24 * time.Hour), Notes: "cancel-me",
		})
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		got, err := d.Cancel(ctx, "itn-1", inst.ID)
		if err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if got.Status != workflows.StatusCanceled {
			t.Fatalf("expected canceled, got %+v", got)
		}
		// Server-side status converges to canceled shortly after the request.
		waitTemporalStatus(t, ctx, d, inst.ID, "Canceled", 30*time.Second)
	})

	t.Run("cross-tenant reads are not_found", func(t *testing.T) {
		inst, err := d.StartCallback(ctx, "itn-1", temporaldriver.StartCallbackInput{
			CustomerID: "cust-4", Phone: "+254700000004", ScheduleAt: time.Now().Add(24 * time.Hour), Notes: "hidden",
		})
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		if _, err := d.Get(ctx, "itn-2", inst.ID); err == nil {
			t.Fatal("foreign tenant must read not_found")
		} else {
			var ae *apperrors.Error
			if !errors.As(err, &ae) || ae.Code != "workflow.not_found" {
				t.Fatalf("want workflow.not_found, got %v", err)
			}
		}
	})
}

// ─── helpers ────────────────────────────────────────────────────────────────

// recordServices is a workflows.Services recorder for the worker under test
// (mutex-guarded: the worker goroutine writes, test goroutines read).
type recordServices struct {
	mu   sync.Mutex
	msgs []string
}

func (s *recordServices) QueueMessage(ctx context.Context, tenantID, customerID, typ, to, body string) error {
	_ = customerID
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, tenantID+"|"+typ+"|"+to+"|"+body)
	return nil
}

func (s *recordServices) queued() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.msgs...)
}

func waitStatus(t *testing.T, ctx context.Context, d *temporaldriver.Driver, tenant, id, want string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		inst, err := d.Get(ctx, tenant, id)
		if err == nil && inst.Status == want {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("context done waiting for %s: %v", want, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	inst, err := d.Get(ctx, tenant, id)
	if err != nil {
		t.Fatalf("status never reached %s (read failed: %v)", want, err)
	}
	t.Fatalf("status never reached %s within %v: %+v", want, within, inst)
}

func waitTemporalStatus(t *testing.T, ctx context.Context, d *temporaldriver.Driver, id, want string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		status, err := d.TemporalStatus(ctx, id)
		if err == nil && strings.Contains(status, want) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("context done waiting for temporal status %s: %v", want, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Fatalf("temporal status never reached %s within %v", want, within)
}
