package freeswitch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Roy-Wanyoike/orvexa/internal/comms/conformance"
	"github.com/Roy-Wanyoike/orvexa/internal/interactions"
	"github.com/Roy-Wanyoike/orvexa/internal/telephony"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

const testTenant = "conformance-tenant"

// newTestAdapter wires an Adapter to the fake switch through the
// conformance recorder's ingest port (the exact signed-webhook path the
// platform uses). Fast timeouts keep the suite tight.
func newTestAdapter(t *testing.T, fs *fakeSwitch, rec *conformance.Recorder) *Adapter {
	t.Helper()
	a, err := New(Config{
		Addr:             fs.addr(),
		Password:         "ClueCon",
		Ingest:           rec.Ingest,
		DialTimeout:      time.Second,
		HandshakeTimeout: 2 * time.Second,
		CommandTimeout:   2 * time.Second,
		IdleTimeout:      5 * time.Second,
		BackoffBase:      10 * time.Millisecond,
		BackoffMax:       50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(a.Close)
	return a
}

// placeCall is the conformance-shaped dial.
func placeCall(t *testing.T, a *Adapter, interactionID string) {
	t.Helper()
	err := a.PlaceCall(context.Background(), telephony.CallCommand{
		InteractionID:   interactionID,
		From:            "+254700000001",
		To:              "+254711111111",
		ProviderOptions: map[string]any{"tenant_id": testTenant},
	})
	if err != nil {
		t.Fatalf("PlaceCall: %v", err)
	}
}

// TestVoiceConformance runs the shared behavioral contract against the ESL
// adapter + fake switch. Events arrive asynchronously over the event
// socket (native callbacks translated on the stream goroutine), so
// RequireAsyncEvents is set — the kit then polls within the settle window
// instead of demanding synchronous delivery.
func TestVoiceConformance(t *testing.T) {
	fs := newFakeSwitch(t)
	rec := conformance.NewRecorder()
	a := newTestAdapter(t, fs, rec)

	conformance.RunVoiceConformance(t, a, conformance.Options{
		Recorder:                 rec,
		ProviderName:             providerName,
		RequireAsyncEvents:       true,
		EnforceCommandValidation: true, // the adapter re-validates commands (uuid, dial strings, tenant)
	})
}

// TestPlaceCallWireFormat pins the exact originate command on the wire and
// the Job-UUID correlation contract, plus the subscribed event set.
func TestPlaceCallWireFormat(t *testing.T) {
	fs := newFakeSwitch(t)
	rec := conformance.NewRecorder()
	a := newTestAdapter(t, fs, rec)

	waitReady(t, a.client, 2*time.Second)
	if got, want := fs.subscribedEvents(), "CHANNEL_ANSWER CHANNEL_HANGUP BACKGROUND_JOB"; got != want {
		t.Fatalf("subscription = %q, want %q", got, want)
	}

	id := uuid.NewString()
	rec.Seed(testTenant, id, interactions.StatusPending)
	placeCall(t, a, id)

	deadline := time.Now().Add(2 * time.Second)
	for len(fs.originateLines()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the originate command never reached the wire")
		}
		time.Sleep(2 * time.Millisecond)
	}
	want := "bgapi originate {origination_uuid=" + id + "}+254711111111 &bridge(+254700000001)"
	if got := fs.originateLines()[0]; got != want {
		t.Fatalf("originate wire line:\n got  %q\n want %q", got, want)
	}

	// Dial acceptance correlates the job; the result arrives as a
	// BACKGROUND_JOB event and must translate to the first ringing phase.
	if !rec.WaitApplied(2, 5*time.Second) {
		t.Fatalf("expected ringing+connected for the placed call, got %d applied", len(rec.Applied()))
	}
	evs := rec.AppliedFor(testTenant, id)
	if len(evs) != 2 || evs[0].Event != evCallRinging || evs[1].Event != evCallConnected {
		t.Fatalf("lifecycle = %v", evs)
	}
	for _, ev := range evs {
		if ev.InteractionID != id || ev.TenantID != testTenant {
			t.Fatalf("event %q carries interaction=%q tenant=%q", ev.Event, ev.InteractionID, ev.TenantID)
		}
	}
}

// TestBackgroundJobFailureEndsLeg covers the -ERR side of the originate
// correlation: the dial was accepted by the switch but the job completes
// with an error — the leg moves to call.failed (cause in Detail) and is
// retired: later operations must report not_found, not silently succeed.
func TestBackgroundJobFailureEndsLeg(t *testing.T) {
	fs := newFakeSwitch(t)
	fs.setOriginateResult("-ERR NO_ANSWER")
	rec := conformance.NewRecorder()
	a := newTestAdapter(t, fs, rec)

	id := uuid.NewString()
	rec.Seed(testTenant, id, interactions.StatusPending)
	placeCall(t, a, id) // acceptance (Job-UUID) is synchronous; the result is not

	if !rec.WaitApplied(1, 5*time.Second) {
		t.Fatalf("expected the failed dial to surface as call.failed, got %d applied", len(rec.Applied()))
	}
	evs := rec.AppliedFor(testTenant, id)
	if len(evs) != 1 || evs[0].Event != evCallFailed {
		t.Fatalf("lifecycle = %v, want exactly one call.failed", evs)
	}
	if evs[0].Detail != "NO_ANSWER" {
		t.Fatalf("call.failed Detail = %q, want the switch cause", evs[0].Detail)
	}
	if st, _ := rec.Status(testTenant, id); st != interactions.StatusFailed {
		t.Fatalf("status after failed dial = %s", st)
	}

	err := a.Hold(context.Background(), id)
	var ae *apperrors.Error
	if !errors.As(err, &ae) || ae.Kind != apperrors.KindNotFound {
		t.Fatalf("operations on a failed leg must be not_found, got: %v", err)
	}
}

// TestCommandRejectionsAreTyped pins the -ERR translation taxonomy on the
// adapter surface: an ESL auth rejection is unauthorized (operator-fixable
// misconfiguration), a channel the switch no longer knows is not_found (and
// retires the tracked leg), and rejected commands never emit lifecycle
// events.
func TestCommandRejectionsAreTyped(t *testing.T) {
	t.Run("auth rejection is unauthorized", func(t *testing.T) {
		fs := newFakeSwitch(t)
		rec := conformance.NewRecorder()
		a, err := New(Config{
			Addr:           fs.addr(),
			Password:       "wrong-password",
			Ingest:         rec.Ingest,
			DialTimeout:    time.Second,
			CommandTimeout: 300 * time.Millisecond, // bounds the wait on a session that can never become ready
			BackoffBase:    10 * time.Millisecond,
			BackoffMax:     50 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer a.Close()

		err = a.PlaceCall(context.Background(), telephony.CallCommand{
			InteractionID:   uuid.NewString(),
			From:            "+254700000001",
			To:              "+254711111111",
			ProviderOptions: map[string]any{"tenant_id": testTenant},
		})
		var ae *apperrors.Error
		if !errors.As(err, &ae) || ae.Kind != apperrors.KindUnauth {
			t.Fatalf("auth rejection must surface as unauthorized, got: %v", err)
		}
		if len(rec.Deliveries()) != 0 {
			t.Fatalf("a rejected dial must not emit deliveries, got %d", len(rec.Deliveries()))
		}
	})

	t.Run("vanished channel is not_found and retires the leg", func(t *testing.T) {
		fs := newFakeSwitch(t)
		rec := conformance.NewRecorder()
		a := newTestAdapter(t, fs, rec)

		id := uuid.NewString()
		rec.Seed(testTenant, id, interactions.StatusPending)
		placeCall(t, a, id)
		if !rec.WaitApplied(2, 5*time.Second) {
			t.Fatal("call did not reach ringing+connected before the channel vanished")
		}

		// The switch lost the channel server-side (reboot edge, stale state):
		// uuid_kill answers -ERR "no such channel".
		fs.forgetChannel(id)
		err := a.Hangup(context.Background(), id)
		var ae *apperrors.Error
		if !errors.As(err, &ae) || ae.Kind != apperrors.KindNotFound {
			t.Fatalf("kill of a vanished channel must be not_found, got: %v", err)
		}
		if ae.Code != "freeswitch.leg_not_found" {
			t.Fatalf("vanished-channel error code = %q, want freeswitch.leg_not_found", ae.Code)
		}

		// The leg was retired with the not_found answer: the repeat reports
		// the adapter's own unknown-leg error — still not_found, still loud.
		err = a.Hangup(context.Background(), id)
		if !errors.As(err, &ae) || ae.Kind != apperrors.KindNotFound {
			t.Fatalf("operations on a retired leg must stay not_found, got: %v", err)
		}

		// Rejections are lifecycle-silent: exactly the pre-existing
		// ringing+connected pair, nothing more.
		if got := len(rec.Deliveries()); got != 2 {
			t.Fatalf("rejections must not emit deliveries, got %d total", got)
		}
	})
}

// TestReconnectMidCall is the acceptance-critical scenario: the switch
// reboots mid-call. The client must rebuild its session, re-subscribe to
// the event stream, keep serving commands, and keep translating events —
// including for legs placed before the reboot.
func TestReconnectMidCall(t *testing.T) {
	fs := newFakeSwitch(t)
	rec := conformance.NewRecorder()
	a := newTestAdapter(t, fs, rec)

	id1 := uuid.NewString()
	rec.Seed(testTenant, id1, interactions.StatusPending)
	placeCall(t, a, id1)
	if !rec.WaitApplied(2, 5*time.Second) {
		t.Fatal("pre-reboot call did not reach ringing+connected")
	}

	subscribesBefore := fs.numSubscribes()
	fs.restart() // switch reboot: every socket dies, the listener re-opens

	// Re-subscription is the reconnect signal: it only happens on a newly
	// established session.
	deadline := time.Now().Add(5 * time.Second)
	for fs.numSubscribes() == subscribesBefore {
		if time.Now().After(deadline) {
			t.Fatal("client did not reconnect and re-subscribe after the switch reboot")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// The pre-reboot leg is still operable: hang it up through the rebuilt
	// session and observe its terminal event through the re-subscribed
	// stream.
	if err := a.Hangup(context.Background(), id1); err != nil {
		t.Fatalf("Hangup after reconnect: %v", err)
	}
	if !rec.WaitApplied(3, 5*time.Second) {
		t.Fatal("call.ended for the pre-reboot leg was not delivered after reconnect")
	}
	if evs := rec.AppliedFor(testTenant, id1); len(evs) != 3 ||
		evs[0].Event != evCallRinging || evs[1].Event != evCallConnected || evs[2].Event != evCallEnded {
		t.Fatalf("pre-reboot leg lifecycle = %v", evs)
	}
	if evs := rec.AppliedFor(testTenant, id1); evs[2].Detail != "NORMAL_CLEARING" {
		t.Fatalf("call.ended Detail = %q, want the hangup cause", evs[2].Detail)
	}

	// And the platform can keep dialing on the rebuilt session.
	id2 := uuid.NewString()
	rec.Seed(testTenant, id2, interactions.StatusPending)
	placeCall(t, a, id2)
	if !rec.WaitApplied(5, 5*time.Second) {
		t.Fatal("post-reboot call did not reach ringing+connected")
	}
}
