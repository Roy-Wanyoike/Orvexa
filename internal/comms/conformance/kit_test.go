// Dogfood: the conformance kit's own proof of life. The built-in Simulator
// is the REFERENCE behavior — it must pass the voice contract green, so the
// kit's assertions and the reference can never drift apart silently. An
// additional in-process fake (an async/callback-style voice adapter with
// full command validation) proves the Options knobs are live wiring, not
// dead flags.
package conformance_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Roy-Wanyoike/orvexa/internal/comms"
	"github.com/Roy-Wanyoike/orvexa/internal/comms/conformance"
	"github.com/Roy-Wanyoike/orvexa/internal/interactions"
	"github.com/Roy-Wanyoike/orvexa/internal/telephony"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// testSigner is a deterministic HMAC stand-in over a fixed, obviously-fake
// key. The kit only requires signatures to be present and stable (HMAC
// verification lives in the webhook gateway), so no credential material
// belongs in conformance tests.
func testSigner(body []byte) string {
	sum := sha256.Sum256(append([]byte("orvexa-conformance-test-key\x00"), body...))
	return "sha256=" + hex.EncodeToString(sum[:16])
}

// TestSimulatorPassesVoiceConformance dogfoods the voice contract against
// the reference Simulator: synchronous progression (default Options), the
// "simulator" provider label pinned.
func TestSimulatorPassesVoiceConformance(t *testing.T) {
	rec := conformance.NewRecorder()
	sim := comms.NewSimulator(rec.Ingest, testSigner, 0)
	conformance.RunVoiceConformance(t, sim, conformance.Options{
		Recorder:     rec,
		ProviderName: "simulator",
	})
}

// asyncVoice mimics a real carrier adapter: lifecycle events arrive on
// callback goroutines (translated into comms.ProviderEvent deliveries), it
// fully validates call commands, and it speaks the application error model
// (unknown legs are typed not-found errors). It exists to prove the kit's
// async and validation knobs are honest wiring.
type asyncVoice struct {
	rec  *conformance.Recorder
	mu   sync.Mutex
	legs map[string]string // interaction id -> tenant
}

func newAsyncVoice(rec *conformance.Recorder) *asyncVoice {
	return &asyncVoice{rec: rec, legs: map[string]string{}}
}

// emitSeq delivers a chain of events on one callback goroutine, in order —
// the same ProviderEvent path the Simulator uses, just asynchronous.
func (a *asyncVoice) emitSeq(events ...comms.ProviderEvent) {
	go func() {
		for _, ev := range events {
			time.Sleep(5 * time.Millisecond)
			body, err := ev.Encode()
			if err != nil {
				return
			}
			_ = a.rec.Ingest(context.Background(), "async-voice", body, testSigner(body))
		}
	}()
}

func (a *asyncVoice) tenant(ref string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.legs[ref]
	return t, ok
}

// PlaceCall validates the full command — the contract behind
// Options.EnforceCommandValidation — then registers the leg and rings.
func (a *asyncVoice) PlaceCall(_ context.Context, cmd telephony.CallCommand) error {
	tenant, _ := cmd.ProviderOptions["tenant_id"].(string)
	if cmd.InteractionID == "" || cmd.From == "" || cmd.To == "" || tenant == "" {
		return apperrors.Invalid("voice.invalid_command",
			"interaction_id, from, to and tenant_id (provider options) are required")
	}
	a.mu.Lock()
	a.legs[cmd.InteractionID] = tenant
	a.mu.Unlock()
	a.emitSeq(
		comms.ProviderEvent{Event: "call.ringing", InteractionID: cmd.InteractionID, TenantID: tenant},
		comms.ProviderEvent{Event: "call.connected", InteractionID: cmd.InteractionID, TenantID: tenant},
	)
	return nil
}

func (a *asyncVoice) Hangup(_ context.Context, providerRef string) error {
	tenant, ok := a.tenant(providerRef)
	if !ok {
		return apperrors.NotFound("voice.leg_not_found", "unknown provider leg reference")
	}
	a.emitSeq(comms.ProviderEvent{Event: "call.ended", InteractionID: providerRef, TenantID: tenant, Detail: "hangup"})
	return nil
}

func (a *asyncVoice) Transfer(_ context.Context, providerRef, destination string) error {
	tenant, ok := a.tenant(providerRef)
	if !ok {
		return apperrors.NotFound("voice.leg_not_found", "unknown provider leg reference")
	}
	a.emitSeq(comms.ProviderEvent{
		Event: "call.ringing", InteractionID: providerRef, TenantID: tenant, Detail: "transfer:" + destination,
	})
	return nil
}

func (a *asyncVoice) Hold(_ context.Context, providerRef string) error {
	if _, ok := a.tenant(providerRef); !ok {
		return apperrors.NotFound("voice.leg_not_found", "unknown provider leg reference")
	}
	return nil // media-state flip only: lifecycle-silent, like the reference
}

func (a *asyncVoice) Resume(_ context.Context, providerRef string) error {
	if _, ok := a.tenant(providerRef); !ok {
		return apperrors.NotFound("voice.leg_not_found", "unknown provider leg reference")
	}
	return nil
}

// TestAsyncValidatingAdapterPassesVoiceConformance proves the async knobs
// (RequireAsyncEvents + EventSettleTimeout) and EnforceCommandValidation are
// live: this adapter emits on goroutines and validates full commands, so the
// default (sync) mode would fail it on late deliveries, and without the
// enforcement flag the command-validation scenario would never run.
func TestAsyncValidatingAdapterPassesVoiceConformance(t *testing.T) {
	rec := conformance.NewRecorder()
	conformance.RunVoiceConformance(t, newAsyncVoice(rec), conformance.Options{
		Recorder:                 rec,
		ProviderName:             "async-voice",
		RequireAsyncEvents:       true,
		EventSettleTimeout:       300 * time.Millisecond, // the fake emits within ~10ms; keep the suite fast
		EnforceCommandValidation: true,
	})
}

// TestRecorderRejectsEventsForUnknownInteractions pins the Recorder's strict
// core: a provider event for an interaction the kit never seeded must fail
// processing as not-found — phantom legs can never materialize state.
func TestRecorderRejectsEventsForUnknownInteractions(t *testing.T) {
	rec := conformance.NewRecorder()
	ev := comms.ProviderEvent{Event: "call.ended", InteractionID: "019393a0-1000-7000-8000-00000000dead", TenantID: "t", Detail: "hangup"}
	body, err := ev.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := rec.Ingest(context.Background(), "simulator", body, "sig-1"); err == nil {
		t.Fatal("events for unseeded interactions must fail processing (strict lifecycle)")
	}
	if got := len(rec.FailedDeliveries()); got != 1 {
		t.Fatalf("the rejected delivery must be recorded as failed, got %d failed deliveries", got)
	}
	if got := len(rec.Applied()); got != 0 {
		t.Fatalf("no event may be applied for an unseeded interaction, got %d", got)
	}
}

// TestRecorderAppliesSeededLifecycle pins the Recorder's harness: seeded
// pending -> ringing -> connected -> ended must apply cleanly and land
// wrapup — the exact path the Simulator dogfood relies on.
func TestRecorderAppliesSeededLifecycle(t *testing.T) {
	rec := conformance.NewRecorder()
	const tenant = "tenant-1"
	const id = "019393a0-1000-7000-8000-00000000beef"
	rec.Seed(tenant, id, interactions.StatusPending)

	for _, ev := range []comms.ProviderEvent{
		{Event: "call.ringing", InteractionID: id, TenantID: tenant},
		{Event: "call.connected", InteractionID: id, TenantID: tenant},
		{Event: "call.ended", InteractionID: id, TenantID: tenant, Detail: "hangup"},
	} {
		body, err := ev.Encode()
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if err := rec.Ingest(context.Background(), "simulator", body, "sig-1"); err != nil {
			t.Fatalf("ingest %s: %v", ev.Event, err)
		}
	}

	names := make([]string, 0, 3)
	for _, ev := range rec.AppliedFor(tenant, id) {
		names = append(names, ev.Event)
	}
	if strings.Join(names, ",") != "call.ringing,call.connected,call.ended" {
		t.Fatalf("applied sequence mismatch: %v", names)
	}
	if st, ok := rec.Status(tenant, id); !ok || st != interactions.StatusWrapup {
		t.Fatalf("lifecycle must land wrapup, got status=%s ok=%v", st, ok)
	}
	if got := len(rec.FailedDeliveries()); got != 0 {
		t.Fatalf("clean lifecycle must not produce failed deliveries, got %d", got)
	}
}
