package conformance

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/Roy-Wanyoike/orvexa/internal/comms"
	"github.com/Roy-Wanyoike/orvexa/internal/interactions"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// newLegID mints a fresh platform interaction id. Provider events carry it
// through comms.Processor, which requires a parseable UUID
// (comms.malformed_event otherwise) — the kit always speaks real ids.
func newLegID() string { return uuid.NewString() }

// eventNames projects applied events to their names, for sequence asserts.
func eventNames(evs []comms.ProviderEvent) []string {
	names := make([]string, len(evs))
	for i, ev := range evs {
		names[i] = ev.Event
	}
	return names
}

// equalSeq compares two sequences exactly, order included.
func equalSeq(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// errorShape renders an error into a comparable, taxonomy-aware string:
// application errors are compared by Kind+Code (stable machine identity),
// plain typed errors by their string form. Used to prove unknown-ref
// failures are deterministic rather than freshly-minted messages.
func errorShape(err error) string {
	var ae *apperrors.Error
	if errors.As(err, &ae) {
		return fmt.Sprintf("apperrors:%s:%s", ae.Kind, ae.Code)
	}
	return "plain:" + err.Error()
}

// requireAppError asserts the exact application-error taxonomy: the error
// must be *apperrors.Error with the given Kind and a non-empty stable code.
// Used where the adapter contract explicitly promises the taxonomy (opt-in
// validation knobs); returns the typed error for code-level asserts.
func requireAppError(t *testing.T, err error, kind apperrors.Kind, what string) *apperrors.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected an application error, got nil", what)
		return nil
	}
	var ae *apperrors.Error
	if !errors.As(err, &ae) {
		t.Fatalf("%s: error must be *apperrors.Error per the pkg/errors taxonomy, got %T: %v", what, err, err)
		return nil
	}
	if ae.Kind != kind {
		t.Errorf("%s: kind must be %q, got %q (code %q)", what, kind, ae.Kind, ae.Code)
	}
	if ae.Code == "" {
		t.Errorf("%s: application error must carry a stable machine code", what)
	}
	return ae
}

// requireErrKind asserts the Kind WHEN the error speaks the application
// error model, and is silent for plain typed errors. This is the kit's
// "taxonomy wherever it is present" rule: the reference Simulator surfaces
// plain sentinels (simError), adapters speaking apperrors must map an
// unknown leg to not_found and invalid input to invalid — the HTTP-status
// semantics clients actually observe — while remaining free to choose codes.
func requireErrKind(t *testing.T, err error, kind apperrors.Kind, what string) {
	t.Helper()
	if err == nil {
		return
	}
	var ae *apperrors.Error
	if errors.As(err, &ae) && ae.Kind != kind {
		t.Errorf("%s: must map to kind %q in the application error model, got %q (code %q)",
			what, kind, ae.Kind, ae.Code)
	}
}

// wantSeq asserts the exact applied lifecycle sequence, order included.
func wantSeq(t *testing.T, evs []comms.ProviderEvent, want ...string) {
	t.Helper()
	got := eventNames(evs)
	if !equalSeq(got, want) {
		t.Fatalf("lifecycle event sequence must be exactly %v, got %v", want, got)
	}
}

// assertStatus asserts the lifecycle status the recorded events produced.
func assertStatus(t *testing.T, rec *Recorder, tenantID, interactionID string, want interactions.Status) {
	t.Helper()
	st, ok := rec.Status(tenantID, interactionID)
	if !ok {
		t.Fatalf("interaction %s must exist after its lifecycle events", interactionID)
	}
	if st != want {
		t.Fatalf("interaction must be %s after the scenario, got %s", want, st)
	}
}

// auditDeliveries enforces the transport invariants on every delivery a
// scenario induced, successful or not:
//
//   - Signature presence: every provider reports through the fail-closed
//     HMAC webhook gateway (architecture contract, ADR-0005) — an unsigned
//     delivery is dropped in production, so an adapter that emits one is
//     asserting behavior that cannot survive the gateway.
//   - Provider label: non-empty, stable, and equal to Options.ProviderName
//     when pinned — the gateway routes and scopes by this label.
func auditDeliveries(t *testing.T, deliveries []Delivery, opts Options) {
	t.Helper()
	for i, d := range deliveries {
		if d.Signature == "" {
			t.Errorf("delivery #%d (%s): every provider webhook must carry a non-empty signature — the gateway is fail-closed and drops unsigned traffic", i, d.Event.Event)
		}
		if d.Provider == "" {
			t.Errorf("delivery #%d (%s): provider label must be non-empty", i, d.Event.Event)
		}
		if opts.ProviderName != "" && d.Provider != opts.ProviderName {
			t.Errorf("delivery #%d (%s): provider label must be %q, got %q", i, d.Event.Event, opts.ProviderName, d.Provider)
		}
	}
}

// awaitApplied waits for the cumulative applied-event count to reach want.
// Async adapters poll within their settle window; synchronous adapters are
// asserted immediately — by the time their port call returned, delivery is
// final, and polling would forgive a late async emitter that violates the
// reference contract.
func awaitApplied(t *testing.T, opts Options, rec *Recorder, want int) {
	t.Helper()
	if opts.RequireAsyncEvents {
		if !rec.WaitApplied(want, opts.settle()) {
			t.Fatalf("timed out after %s waiting for %d applied lifecycle events (got %d) — "+
				"the adapter must translate native callbacks into comms.ProviderEvent deliveries "+
				"through its wired Recorder", opts.settle(), want, len(rec.Applied()))
		}
		return
	}
	if got := len(rec.Applied()); got < want {
		t.Fatalf("expected %d applied lifecycle events synchronously, got %d — the reference Simulator "+
			"delivers before the port call returns; async adapters must set Options.RequireAsyncEvents. "+
			"Hint: confirm the adapter is wired to the SAME Recorder instance passed in Options.Recorder",
			want, got)
	}
}

// awaitQuiet gives async emitters a settle window before the caller asserts
// that nothing new was observed; synchronous adapters need no window —
// after the port call returns, silence is already final.
func awaitQuiet(t *testing.T, opts Options, rec *Recorder) {
	t.Helper()
	if opts.RequireAsyncEvents {
		rec.AwaitSilence(opts.settle())
	}
}

// assertNoNewObservations is the shared "the adapter must have done nothing
// on the wire" assertion: no deliveries at all, hence no applied events and
// no failed deliveries either. Rejections must happen before emission.
func assertNoNewObservations(t *testing.T, rec *Recorder, d0, f0, a0 int, what string) {
	t.Helper()
	if got := len(rec.Deliveries()); got != d0 {
		t.Errorf("%s: must not emit any deliveries, got %d new (failed ones count too)", what, got-d0)
	}
	if got := len(rec.FailedDeliveries()); got != f0 {
		t.Errorf("%s: must not produce failed deliveries, got %d", what, got-f0)
	}
	if got := len(rec.Applied()); got != a0 {
		t.Errorf("%s: must not move any interaction lifecycle, got %d new applied events", what, got-a0)
	}
}

// assertNoFailedDeliveries is the scenario canary: every delivery the kit
// induced must apply cleanly — a failed delivery means the adapter emitted
// something the processor rejects (unknown event name, malformed payload,
// illegal transition, foreign interaction), i.e. a broken translation layer.
func assertNoFailedDeliveries(t *testing.T, rec *Recorder, f0 int, what string) {
	t.Helper()
	if got := len(rec.FailedDeliveries()); got != f0 {
		t.Errorf("%s: every induced delivery must apply cleanly, got %d new failed deliveries", what, got-f0)
		for _, d := range rec.FailedDeliveries()[f0:] {
			t.Errorf("  rejected delivery (event=%q interaction=%s tenant=%s): %v", d.Event.Event, d.Event.InteractionID, d.Event.TenantID, d.Err)
		}
	}
}
