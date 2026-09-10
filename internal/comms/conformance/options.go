package conformance

import (
	"testing"
	"time"

	"github.com/Roy-Wanyoike/orvexa/internal/interactions"
)

// Options configures a conformance run. The zero value is the honest
// default: it matches the built-in Simulator exactly (synchronous
// progression, provider label unchecked, reference validation split).
// Adapters only set the knobs that genuinely differ from the reference.
type Options struct {
	// Recorder is the event receiver wired as the adapter's delivery hook.
	// Required: construct with NewRecorder, hand Recorder.Ingest to the
	// adapter, and pass the SAME instance here. The kit fails fast when it
	// is missing, and its first lifecycle failure carries a wiring hint.
	Recorder *Recorder

	// ProviderName pins the provider label the adapter must stamp on every
	// delivery (the webhook gateway routes by it). Empty (the default)
	// only requires a non-empty, stable label — the reference Simulator is
	// labelled "simulator" and the dogfood test pins exactly that.
	ProviderName string

	// RequireAsyncEvents declares that the adapter emits lifecycle events
	// asynchronously (native callbacks translated on goroutines). Default
	// false = the reference contract: by the time a port call returns, its
	// events have been delivered and applied. Async adapters MUST set this,
	// or the kit fails them for late deliveries.
	RequireAsyncEvents bool

	// EventSettleTimeout is the window async adapters get for each expected
	// event batch (and each expected silence). Only consulted when
	// RequireAsyncEvents is set. Default 2s.
	EventSettleTimeout time.Duration

	// TenantID is the tenant context the kit exercises with. Default
	// "conformance-tenant". Every scenario seeds, dials and asserts within
	// this single tenant so cross-tenant bleed is always observable.
	TenantID string

	// EnforceCommandValidation opts the adapter into full call-command
	// validation checks: PlaceCall must reject malformed commands with
	// *apperrors.Error (KindInvalid). Default false — the reference
	// Simulator enforces only the tenant context, because the core service
	// already validated the rest before dialing.
	EnforceCommandValidation bool

	// EnforceSendValidation opts the adapter into message re-validation
	// checks: Send must reject invalid messages with *apperrors.Error
	// carrying EXACTLY the messaging.ValidateMessage codes. Default false —
	// the core pre-validates every outbound message (messaging.Service.Send)
	// and the reference Simulator accepts anything the core would send.
	EnforceSendValidation bool
}

// normalize applies the documented defaults so zero-value knobs mean
// reference behavior.
func (o Options) normalize() Options {
	if o.TenantID == "" {
		o.TenantID = "conformance-tenant"
	}
	if o.EventSettleTimeout <= 0 {
		o.EventSettleTimeout = 2 * time.Second
	}
	return o
}

// settle returns the effective async settle window.
func (o Options) settle() time.Duration {
	if o.EventSettleTimeout <= 0 {
		return 2 * time.Second
	}
	return o.EventSettleTimeout
}

// requireRecorder is the shared fail-fast guard for the Run* entry points.
func requireRecorder(t *testing.T, opts Options) *Recorder {
	t.Helper()
	if opts.Recorder == nil {
		t.Fatal("conformance: Options.Recorder is required — construct with conformance.NewRecorder(), " +
			"wire rec.Ingest as the adapter's delivery hook, and pass the same instance here (see package README)")
	}
	return opts.Recorder
}

// seedPending pre-creates a pending interaction for a placed call. The core
// creates the interaction before dialing (telephony.Service.PlaceCall);
// unseeded interactions hard-fail event processing by design.
func seedPending(t *testing.T, rec *Recorder, tenantID, interactionID string) {
	t.Helper()
	rec.Seed(tenantID, interactionID, interactions.StatusPending)
}

// seedActive pre-creates an active interaction for an outbound message,
// mirroring messaging.Service.Send's pending->active flip before provider
// handoff.
func seedActive(t *testing.T, rec *Recorder, tenantID, interactionID string) {
	t.Helper()
	rec.Seed(tenantID, interactionID, interactions.StatusActive)
}
