package conformance

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/Roy-Wanyoike/orvexa/internal/comms"
	"github.com/Roy-Wanyoike/orvexa/internal/interactions"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// Delivery is one raw provider webhook observed by the Recorder: the
// provider label the adapter stamped, the wire body, the signature the
// adapter computed, the decoded event, and the outcome of applying it to the
// guarded interaction lifecycle.
type Delivery struct {
	Provider  string
	Body      []byte
	Signature string
	Event     comms.ProviderEvent
	// Err is the processing result. nil means the event passed
	// comms.Processor and the lifecycle moved (or same-state no-oped);
	// non-nil means the gateway-equivalent path rejected it.
	Err error
}

// Applied reports whether the delivery survived the processor.
func (d Delivery) Applied() bool { return d.Err == nil }

// coreFake is the kit's interaction core: the guarded lifecycle state
// machine comms.Processor drives, backed by an in-memory map. It models the
// production guarantee that an interaction EXISTS before a provider is
// invoked (telephony.Service.PlaceCall / messaging.Service.Send create it
// first) — unknown interactions are hard not-found errors, so a provider
// event for a leg the kit never seeded can never materialize state.
type coreFake struct {
	mu       sync.Mutex
	statuses map[string]interactions.Status
}

func newCoreFake() *coreFake {
	return &coreFake{statuses: map[string]interactions.Status{}}
}

func lifecycleKey(tenantID, interactionID string) string {
	return tenantID + "/" + interactionID
}

// Get implements comms.InteractionCore. Strict mode: unknown interactions
// return the same not-found error the real interactions.Service.Get returns
// for a missing row — one taxonomy all the way down.
func (f *coreFake) Get(_ context.Context, tenantID, id string) (*interactions.Rec, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.statuses[lifecycleKey(tenantID, id)]
	if !ok {
		return nil, apperrors.NotFound("interaction.not_found", "interaction not found")
	}
	return &interactions.Rec{ID: id, TenantID: tenantID, Status: st}, nil
}

// Transition implements comms.InteractionCore with the guarded state
// machine (interactions.CanTransition). The processor already short-circuits
// same-state deliveries; the guard here is defense in depth for direct use.
func (f *coreFake) Transition(_ context.Context, tenantID, id string, to interactions.Status, _ string) (*interactions.Rec, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := lifecycleKey(tenantID, id)
	cur, ok := f.statuses[k]
	if !ok {
		return nil, apperrors.NotFound("interaction.not_found", "interaction not found")
	}
	if cur != to && !interactions.CanTransition(cur, to) {
		return nil, apperrors.Conflict("interaction.invalid_transition",
			"illegal transition "+string(cur)+" -> "+string(to))
	}
	f.statuses[k] = to
	return &interactions.Rec{ID: id, TenantID: tenantID, Status: to}, nil
}

// Recorder is the kit's event receiver. Wire it as the adapter's delivery
// hook at construction time (it satisfies comms.IngestFunc), pass the same
// instance via Options.Recorder, and the conformance runs observe the
// adapter through the exact ProviderEvent path the built-in Simulator uses.
//
// The Recorder is policy-free: it records and applies. Every judgment
// (signatures, sequences, taxonomy) lives in the Run* functions so adapter
// authors can read the assertions in one place.
type Recorder struct {
	mu   sync.Mutex
	core *coreFake
	all  []Delivery
}

// NewRecorder builds an empty recorder. No goroutines, no cleanup: the
// recorder is passive and safe for concurrent use.
func NewRecorder() *Recorder {
	return &Recorder{core: newCoreFake()}
}

// Ingest implements the comms.IngestFunc delivery port — hand this method
// value to the adapter as its webhook-delivery function. It decodes the body
// into a comms.ProviderEvent and applies it through comms.Processor against
// the guarded lifecycle, recording the full outcome either way. The
// returned error is the processing error (if any), so adapters exercising
// their retry paths see the same failures the real gateway would produce.
func (r *Recorder) Ingest(ctx context.Context, provider string, body []byte, signature string) error {
	var ev comms.ProviderEvent
	err := json.Unmarshal(body, &ev)
	if err == nil {
		err = (&comms.Processor{Interactions: r.core}).Process(ctx, &ev)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.all = append(r.all, Delivery{
		Provider:  provider,
		Body:      append([]byte(nil), body...),
		Signature: signature,
		Event:     ev,
		Err:       err,
	})
	return err
}

// Seed pre-creates the interaction the kit is about to exercise, mirroring
// the core's contract: the platform creates the interaction (pending for a
// placed call, active for an outbound message after the service's
// pending->active flip) before the provider is ever invoked. Events for
// interactions that were never seeded fail processing as not-found.
func (r *Recorder) Seed(tenantID, interactionID string, status interactions.Status) {
	r.core.mu.Lock()
	defer r.core.mu.Unlock()
	r.core.statuses[lifecycleKey(tenantID, interactionID)] = status
}

// Deliveries returns a snapshot of every observed delivery, in arrival
// order, applied or not.
func (r *Recorder) Deliveries() []Delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Delivery, len(r.all))
	copy(out, r.all)
	return out
}

// Applied returns the decoded events that passed the processor, in arrival
// order. This is the lifecycle truth the kit asserts sequences against.
func (r *Recorder) Applied() []comms.ProviderEvent {
	return r.appliedFiltered("", "")
}

// AppliedFor returns the applied events for one tenant-scoped interaction,
// in arrival order.
func (r *Recorder) AppliedFor(tenantID, interactionID string) []comms.ProviderEvent {
	return r.appliedFiltered(tenantID, interactionID)
}

func (r *Recorder) appliedFiltered(tenantID, interactionID string) []comms.ProviderEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []comms.ProviderEvent
	for _, d := range r.all {
		if d.Err != nil {
			continue
		}
		if tenantID != "" && d.Event.TenantID != tenantID {
			continue
		}
		if interactionID != "" && d.Event.InteractionID != interactionID {
			continue
		}
		out = append(out, d.Event)
	}
	return out
}

// FailedDeliveries returns the deliveries the processor rejected (unknown
// event names, malformed payloads, events for unknown interactions, illegal
// transitions). Conformance scenarios use the count as a canary: a clean
// scenario must never grow it.
func (r *Recorder) FailedDeliveries() []Delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Delivery
	for _, d := range r.all {
		if d.Err != nil {
			out = append(out, d)
		}
	}
	return out
}

// Status returns the lifecycle status the recorded events produced for the
// interaction, and whether the interaction exists at all.
func (r *Recorder) Status(tenantID, interactionID string) (interactions.Status, bool) {
	r.core.mu.Lock()
	defer r.core.mu.Unlock()
	st, ok := r.core.statuses[lifecycleKey(tenantID, interactionID)]
	return st, ok
}

// WaitApplied blocks until at least want events have been applied (or the
// timeout expires) and reports success. Async adapters — the reason this
// exists — translate native callbacks on goroutines, so the kit polls
// instead of asserting immediately. Synchronous adapters (the reference
// Simulator) satisfy the count on return; the Run* functions deliberately
// do NOT poll for them so a late async emitter fails the sync contract.
func (r *Recorder) WaitApplied(want int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if len(r.Applied()) >= want {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// AwaitSilence gives asynchronous emitters a full settle window to (wrongly)
// emit before the caller asserts that observation counts did not move. With
// synchronous adapters the Run* functions skip the wait entirely: after the
// port call returns, silence is already final.
func (r *Recorder) AwaitSilence(timeout time.Duration) {
	time.Sleep(timeout)
}
