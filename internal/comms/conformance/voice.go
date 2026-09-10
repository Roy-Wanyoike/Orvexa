package conformance

import (
	"context"
	"strings"
	"testing"

	"github.com/Roy-Wanyoike/orvexa/internal/interactions"
	"github.com/Roy-Wanyoike/orvexa/internal/telephony"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// RunVoiceConformance runs the full voice behavioral contract against a
// VoiceProvider adapter. It is the gate every carrier adapter (twilio,
// africastalking voice, freeswitch, asterisk, ...) passes before merge; the
// built-in Simulator is the reference behavior and passes it green.
//
// Wiring: construct the Recorder first, hand Recorder.Ingest to the adapter
// as its delivery hook, pass the same Recorder via Options.Recorder. See
// the package README for the embedding snippet.
//
// Scenarios (each documents the invariant it protects):
//
//  1. place-call lifecycle: ringing then connected
//  2. hangup moves the call to wrapup with a cause
//  3. transfer re-rings the live leg
//  4. hold and resume are silent leg-state flips
//  5. unknown provider refs fail loudly and emit nothing
//  6. place-call requires tenant context
//  7. call-command validation (only with Options.EnforceCommandValidation)
func RunVoiceConformance(t *testing.T, p telephony.VoiceProvider, opts Options) {
	t.Helper()
	if p == nil {
		t.Fatal("conformance: nil telephony.VoiceProvider")
	}
	rec := requireRecorder(t, opts)
	opts = opts.normalize()
	ctx := context.Background()

	const (
		from = "+254700000001" // E.164-shape sample origin (extension-style ids are equally legal)
		to   = "+254711111111" // sample destination
		dest = "+254722000000" // transfer destination
	)

	t.Run("place-call lifecycle: ringing then connected", func(t *testing.T) {
		id := newLegID()
		seedPending(t, rec, opts.TenantID, id)
		d0, f0, a0 := len(rec.Deliveries()), len(rec.FailedDeliveries()), len(rec.Applied())

		err := p.PlaceCall(ctx, telephony.CallCommand{
			InteractionID:   id,
			From:            from,
			To:              to,
			ProviderOptions: map[string]any{"tenant_id": opts.TenantID},
		})
		// Invariant: a well-formed, tenant-bound place-call is accepted. The
		// core only dials interactions it created (pending) and always
		// stamps tenant_id into ProviderOptions — an adapter that rejects
		// this command cannot carry the platform's basic dial.
		if err != nil {
			t.Fatalf("PlaceCall must succeed for a valid tenant-bound command, got: %v", err)
		}
		awaitApplied(t, opts, rec, a0+2)

		evs := rec.AppliedFor(opts.TenantID, id)
		// Invariant: the dial phase is observable as exactly
		// call.ringing -> call.connected, in that order, with nothing else.
		// Routing, presence and the realtime fan-out depend on a connected
		// call having had an observed ringing phase; missing, extra or
		// reordered events would drive the guarded state machine into a
		// different public state (or 409 it).
		wantSeq(t, evs, "call.ringing", "call.connected")

		for _, ev := range evs {
			// Invariant: every lifecycle event is stamped with the dialed
			// interaction's own id and tenant — cross-leg or cross-tenant
			// bleed would mutate a foreign conversation, and tenant-less
			// events are rejected outright by the processor.
			if ev.InteractionID != id || ev.TenantID != opts.TenantID {
				t.Errorf("lifecycle event %q must carry interaction_id=%s tenant_id=%s, got interaction_id=%q tenant_id=%q",
					ev.Event, id, opts.TenantID, ev.InteractionID, ev.TenantID)
			}
		}
		auditDeliveries(t, rec.Deliveries()[d0:], opts)
		assertNoFailedDeliveries(t, rec, f0, "place-call lifecycle")

		// Invariant: after connected, the interaction is active — the
		// routing plane assigns agents only to live interactions, so a
		// connected call that is still pending/wrapup is a routing outage.
		assertStatus(t, rec, opts.TenantID, id, interactions.StatusActive)
	})

	t.Run("hangup moves the call to wrapup with a cause", func(t *testing.T) {
		id := newLegID()
		seedPending(t, rec, opts.TenantID, id)
		d0, f0, a0 := len(rec.Deliveries()), len(rec.FailedDeliveries()), len(rec.Applied())

		if err := p.PlaceCall(ctx, telephony.CallCommand{
			InteractionID:   id,
			From:            from,
			To:              to,
			ProviderOptions: map[string]any{"tenant_id": opts.TenantID},
		}); err != nil {
			t.Fatalf("PlaceCall: %v", err)
		}
		awaitApplied(t, opts, rec, a0+2)

		if err := p.Hangup(ctx, id); err != nil {
			// Invariant: hanging up a live leg the provider owns is
			// accepted — the customer side ended the call.
			t.Fatalf("Hangup on a live leg must succeed, got: %v", err)
		}
		awaitApplied(t, opts, rec, a0+3)

		evs := rec.AppliedFor(opts.TenantID, id)
		// Invariant: the observable order across one call is
		// ringing -> connected -> ended. ended must never overtake the
		// dial phase it terminates.
		wantSeq(t, evs, "call.ringing", "call.connected", "call.ended")

		ended := evs[len(evs)-1]
		// Invariant: call.ended carries a non-empty hangup cause. The
		// processor persists Detail as the interaction's end_reason — the
		// audit trail for why the customer left. The reference reports
		// "hangup"; adapters map their native cause, but silence is not a
		// cause.
		if strings.TrimSpace(ended.Detail) == "" {
			t.Errorf("call.ended must carry a non-empty Detail (hangup cause / end_reason), got %q", ended.Detail)
		}
		auditDeliveries(t, rec.Deliveries()[d0:], opts)
		assertNoFailedDeliveries(t, rec, f0, "hangup lifecycle")

		// Invariant: ended lands the interaction in wrapup, NOT completed —
		// after-call work (wrapup -> completed) belongs to the agent side,
		// and a provider event claiming completion would erase that phase.
		assertStatus(t, rec, opts.TenantID, id, interactions.StatusWrapup)
	})

	t.Run("transfer re-rings the live leg", func(t *testing.T) {
		id := newLegID()
		seedPending(t, rec, opts.TenantID, id)
		d0, f0, a0 := len(rec.Deliveries()), len(rec.FailedDeliveries()), len(rec.Applied())

		if err := p.PlaceCall(ctx, telephony.CallCommand{
			InteractionID:   id,
			From:            from,
			To:              to,
			ProviderOptions: map[string]any{"tenant_id": opts.TenantID},
		}); err != nil {
			t.Fatalf("PlaceCall: %v", err)
		}
		awaitApplied(t, opts, rec, a0+2)

		if err := p.Transfer(ctx, id, dest); err != nil {
			// Invariant: transferring a live leg is accepted — transfer is
			// a normal in-call operation, never an exceptional one.
			t.Fatalf("Transfer on a live leg must succeed, got: %v", err)
		}
		awaitApplied(t, opts, rec, a0+3)

		evs := rec.AppliedFor(opts.TenantID, id)
		// Invariant: the transfer is a NEW RINGING PHASE on the same leg —
		// faithful carrier behavior (the destination rings) — and it is the
		// only lifecycle-visible effect. The closed event vocabulary has no
		// "transfer" event; a new ringing phase is the honest translation.
		wantSeq(t, evs, "call.ringing", "call.connected", "call.ringing")

		ring := evs[len(evs)-1]
		// Invariant: the destination is observable in the event Detail —
		// the audit trail must record where a live customer was sent.
		if !strings.Contains(ring.Detail, dest) {
			t.Errorf("transfer ringing event must carry the destination in Detail, got %q (want it to contain %q)", ring.Detail, dest)
		}

		// Invariant: transfer must not move the lifecycle backward or
		// terminal — mid-transfer the leg is still an active call.
		assertStatus(t, rec, opts.TenantID, id, interactions.StatusActive)

		if err := p.Hangup(ctx, id); err != nil {
			t.Fatalf("Hangup after transfer must succeed, got: %v", err)
		}
		awaitApplied(t, opts, rec, a0+4)
		// Invariant: the full observable order across a transferred call is
		// ringing -> connected -> ringing -> ended: the second ringing phase
		// is sandwiched between connected and ended, never after the call
		// was reported over.
		wantSeq(t, rec.AppliedFor(opts.TenantID, id), "call.ringing", "call.connected", "call.ringing", "call.ended")
		assertStatus(t, rec, opts.TenantID, id, interactions.StatusWrapup)
		auditDeliveries(t, rec.Deliveries()[d0:], opts)
		assertNoFailedDeliveries(t, rec, f0, "transfer lifecycle")
	})

	t.Run("hold and resume are silent leg-state flips", func(t *testing.T) {
		id := newLegID()
		seedPending(t, rec, opts.TenantID, id)
		d0, a0 := len(rec.Deliveries()), len(rec.Applied())

		if err := p.PlaceCall(ctx, telephony.CallCommand{
			InteractionID:   id,
			From:            from,
			To:              to,
			ProviderOptions: map[string]any{"tenant_id": opts.TenantID},
		}); err != nil {
			t.Fatalf("PlaceCall: %v", err)
		}
		awaitApplied(t, opts, rec, a0+2)
		h0, f0 := len(rec.Applied()), len(rec.FailedDeliveries())

		// Invariant: holding a live leg is accepted and Resume un-holds it.
		if err := p.Hold(ctx, id); err != nil {
			t.Fatalf("Hold on a live leg must succeed, got: %v", err)
		}
		if err := p.Resume(ctx, id); err != nil {
			t.Fatalf("Resume on a held leg must succeed, got: %v", err)
		}
		awaitQuiet(t, opts, rec)

		// Invariant: Hold/Resume emit NO lifecycle events — not even failed
		// ones. The ProviderEvent vocabulary is closed (the processor's
		// known-event switch) and contains no hold semantics; any event an
		// adapter invents here would be a real lifecycle claim, and the
		// plausible ones are catastrophic (call.ended would wrap up the
		// call mid-hold; call.ringing would fake a re-dial).
		if got := len(rec.Applied()); got != h0 {
			t.Errorf("Hold/Resume must be lifecycle-silent, got %d new applied events", got-h0)
		}
		if got := len(rec.Deliveries()); got != d0+2 {
			t.Errorf("Hold/Resume must not emit any deliveries, got %d new", got-(d0+2))
		}

		// Invariant: the leg is still live while held — hold is a media
		// operation, not a lifecycle one.
		assertStatus(t, rec, opts.TenantID, id, interactions.StatusActive)

		if err := p.Hangup(ctx, id); err != nil {
			t.Fatalf("Hangup after hold/resume must succeed — the leg stayed operable, got: %v", err)
		}
		awaitApplied(t, opts, rec, h0+1)
		assertStatus(t, rec, opts.TenantID, id, interactions.StatusWrapup)
		auditDeliveries(t, rec.Deliveries()[d0:], opts)
		assertNoFailedDeliveries(t, rec, f0, "hold/resume lifecycle")
	})

	t.Run("unknown provider refs fail loudly and emit nothing", func(t *testing.T) {
		ref := "conformance-unknown-" + newLegID()
		d0, f0, a0 := len(rec.Deliveries()), len(rec.FailedDeliveries()), len(rec.Applied())

		type op struct {
			name string
			run  func() error
		}
		ops := []op{
			{"Hangup", func() error { return p.Hangup(ctx, ref) }},
			{"Transfer", func() error { return p.Transfer(ctx, ref, dest) }},
			{"Hold", func() error { return p.Hold(ctx, ref) }},
			{"Resume", func() error { return p.Resume(ctx, ref) }},
		}
		for _, o := range ops {
			err1 := o.run()
			// Invariant: an operation against a leg the provider does not
			// own must FAIL. Silent success would strand a live interaction
			// forever: the platform-side hangup/transfer/hold would be a
			// no-op while the carrier keeps billing the leg.
			if err1 == nil {
				t.Errorf("%s on unknown providerRef must return an error, got nil", o.name)
				continue
			}
			err2 := o.run()
			// Invariant: the failure is deterministic — the same unknown ref
			// yields the same typed error on repeat, so callers (and the
			// core's error mapping) can classify it reliably instead of
			// flipping between error and success.
			if err2 == nil {
				t.Errorf("%s on unknown providerRef must fail consistently, second call returned nil", o.name)
				continue
			}
			if s1, s2 := errorShape(err1), errorShape(err2); s1 != s2 {
				t.Errorf("%s on unknown providerRef must be shape-stable across repeats: %q then %q", o.name, s1, s2)
			}
			// Invariant: where the adapter speaks the application error
			// model, an unknown leg is not_found — 404 semantics (the
			// resource does not exist), never invalid/conflict/internal.
			requireErrKind(t, err1, apperrors.KindNotFound, o.name+" on unknown providerRef")
		}
		awaitQuiet(t, opts, rec)

		// Invariant: unknown-ref operations emit nothing — a provider event
		// for a leg the platform never placed would 404 at the processor at
		// best, or materialize state for a foreign interaction at worst.
		assertNoNewObservations(t, rec, d0, f0, a0, "unknown-ref operations")
	})

	t.Run("place-call requires tenant context", func(t *testing.T) {
		cases := []struct {
			name    string
			options map[string]any
		}{
			{"nil provider options", nil},
			{"missing tenant_id option", map[string]any{}},
			{"non-string tenant_id option", map[string]any{"tenant_id": 42}},
		}
		for _, tc := range cases {
			id := newLegID()
			seedPending(t, rec, opts.TenantID, id)
			d0, f0, a0 := len(rec.Deliveries()), len(rec.FailedDeliveries()), len(rec.Applied())

			err := p.PlaceCall(ctx, telephony.CallCommand{
				InteractionID:   id,
				From:            from,
				To:              to,
				ProviderOptions: tc.options,
			})
			// Invariant: tenant context is mandatory. The core always stamps
			// tenant_id (telephony.Service.PlaceCall), adapters map tenant ->
			// carrier account, and every emitted event must carry tenant_id
			// for the processor. An adapter that dials without tenant context
			// cannot scope its leg or its events — and non-string values must
			// be rejected, not silently coerced.
			if err == nil {
				t.Errorf("%s: PlaceCall without tenant context must be rejected, got nil error", tc.name)
				continue
			}
			// Invariant: a rejected dial is an INPUT problem — where the
			// adapter speaks the application error model it maps to invalid
			// (422), not internal (500): the caller can fix the input.
			requireErrKind(t, err, apperrors.KindInvalid, tc.name+": place-call without tenant context")
			awaitQuiet(t, opts, rec)

			// Invariant: a rejected dial must not ring — no deliveries, no
			// lifecycle movement, no failed deliveries. A rejected PlaceCall
			// that still emits events would drive an interaction the
			// provider just claimed it could not start.
			assertNoNewObservations(t, rec, d0, f0, a0, tc.name+": rejected place-call")
		}
	})

	if opts.EnforceCommandValidation {
		t.Run("call-command validation", func(t *testing.T) {
			cases := []struct {
				name string
				cmd  telephony.CallCommand
			}{
				{
					name: "missing interaction id",
					cmd:  telephony.CallCommand{From: from, To: to, ProviderOptions: map[string]any{"tenant_id": opts.TenantID}},
				},
				{
					name: "missing destination",
					cmd:  telephony.CallCommand{InteractionID: newLegID(), From: from, ProviderOptions: map[string]any{"tenant_id": opts.TenantID}},
				},
				{
					name: "missing origin",
					cmd:  telephony.CallCommand{InteractionID: newLegID(), To: to, ProviderOptions: map[string]any{"tenant_id": opts.TenantID}},
				},
				{
					name: "missing tenant context",
					cmd:  telephony.CallCommand{InteractionID: newLegID(), From: from, To: to},
				},
			}
			for _, tc := range cases {
				d0, a0 := len(rec.Deliveries()), len(rec.Applied())
				seedPending(t, rec, opts.TenantID, tc.cmd.InteractionID)

				err := p.PlaceCall(ctx, tc.cmd)
				// Invariant: adapters that advertise command validation use
				// the application error model with KindInvalid (422 — input
				// failed validation) and a stable machine code. Codes are
				// adapter-specific (there is no core-side voice command
				// taxonomy beyond tenant context); the kind is the contract.
				ae := requireAppError(t, err, apperrors.KindInvalid, tc.name+": PlaceCall must reject malformed commands")
				if ae != nil && ae.Code == "" {
					t.Errorf("%s: validation error must carry a stable machine code", tc.name)
				}
				awaitQuiet(t, opts, rec)

				// Invariant: a rejected command must not touch the carrier
				// or the lifecycle — zero deliveries, zero lifecycle moves.
				if got := len(rec.Deliveries()); got != d0 {
					t.Errorf("%s: rejected command must not emit deliveries, got %d new", tc.name, got-d0)
				}
				if got := len(rec.Applied()); got != a0 {
					t.Errorf("%s: rejected command must not move the lifecycle, got %d new applied events", tc.name, got-a0)
				}
			}
		})
	}
}
