package bus

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/Roy-Wanyoike/orvexa/pkg/events"
)

// This file is the shared Bus conformance kit ([O-24], issue #33): one set
// of scenarios every driver — InProc and NATS alike — must pass. The kit
// asserts only semantics BOTH drivers guarantee under the Bus contract, so
// a driver cannot pass here while diverging from the reference InProc
// behavior. Driver-specific semantics (NATS ack/nak backoff, durable
// redelivery across restarts, InProc slow-consumer drops) are covered next
// to their implementation, not here.
//
// The InProc scenario set runs unconditionally (this file); the NATS runs
// come from nats_test.go (in-process JetStream-shaped fake) and
// nats_integration_test.go (build tag `integration`, real server from
// docker-compose.dev.yml).

// kitSettle is the polling window a driver gets to deliver an event. InProc
// fans out synchronously into per-handler goroutines; JetStream pull
// consumers fetch in batches — both land well inside it in practice.
const kitSettle = 2 * time.Second

// kitSettleNegative is how long the kit waits to confirm a stopped
// subscription stays silent.
const kitSettleNegative = 150 * time.Millisecond

// kitRecorder collects envelopes delivered to one subscriber. Handlers run
// on driver-owned goroutines, so all access is mutex-guarded.
type kitRecorder struct {
	mu  sync.Mutex
	got []events.Envelope
}

func newKitRecorder() *kitRecorder { return &kitRecorder{} }

func (r *kitRecorder) handler() Handler {
	return func(_ context.Context, env *events.Envelope) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.got = append(r.got, *env)
		return nil
	}
}

func (r *kitRecorder) ids() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.got))
	for i := range r.got {
		out = append(out, r.got[i].ID)
	}
	return out
}

// snapshot returns a copy of the recorded envelopes so header-fidelity
// checks read them without racing the handler goroutines.
func (r *kitRecorder) snapshot() []events.Envelope {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]events.Envelope, len(r.got))
	copy(out, r.got)
	return out
}

func (r *kitRecorder) uniqueCounts() map[string]int {
	counts := map[string]int{}
	for _, id := range r.ids() {
		counts[id]++
	}
	return counts
}

// waitForUnique polls until want distinct envelope IDs arrived or the
// settle window expires.
func (r *kitRecorder) waitForUnique(t *testing.T, want int) map[string]int {
	t.Helper()
	deadline := time.Now().Add(kitSettle)
	for {
		counts := r.uniqueCounts()
		if len(counts) >= want {
			return counts
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d distinct envelopes, got %d (%v)", want, len(counts), counts)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *kitRecorder) mustBeEmpty(t *testing.T) {
	t.Helper()
	if got := r.ids(); len(got) != 0 {
		t.Fatalf("expected no deliveries, got %d: %v", len(got), got)
	}
}

// kitEnvelope builds a valid envelope on a registered topic with
// JSON-safe data (strings only) so InProc pointer fan-out and NATS JSON
// round-trips are comparable field by field.
func kitEnvelope(t *testing.T, topic string) *events.Envelope {
	t.Helper()
	env, err := events.New(topic, "orvexa-kit", "kit-subject-1", "kit-tenant",
		"kit-correlation", map[string]any{"region": "eu-west", "kind": topic})
	if err != nil {
		t.Fatalf("kit envelope %s: %v", topic, err)
	}
	return env
}

// runBusConformance executes the shared driver scenarios against a freshly
// constructed Bus per subtest. name is used as the subtest prefix.
func runBusConformance(t *testing.T, name string, newBus func(t *testing.T) Bus) {
	t.Helper()

	t.Run(name+"/publish_subscribe_roundtrip", func(t *testing.T) {
		b := newBus(t)
		defer func() { _ = b.Close() }()
		rec := newKitRecorder()
		if _, err := b.Subscribe(events.TopicInteractionCreated, rec.handler()); err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		env := kitEnvelope(t, events.TopicInteractionCreated)
		if err := b.Publish(context.Background(), env); err != nil {
			t.Fatalf("publish: %v", err)
		}
		if counts := rec.waitForUnique(t, 1); counts[env.ID] != 1 {
			t.Fatalf("envelope %s delivered %d times, want exactly 1: %v", env.ID, counts[env.ID], counts)
		}
		// Wire fidelity: the envelope a consumer sees must equal what the
		// publisher sent (NATS proves the JSON round-trip; InProc proves the
		// reference pointer fan-out stays lossless).
		sent := *env
		got := rec.snapshot()
		if len(got) != 1 {
			t.Fatalf("expected exactly one recorded envelope, got %d", len(got))
		}
		have := got[0]
		if have.ID != sent.ID || have.Type != sent.Type || have.Source != sent.Source ||
			have.Subject != sent.Subject || have.TenantID != sent.TenantID || have.CorrelationID != sent.CorrelationID {
			t.Fatalf("envelope header drift:\n sent %+v\n got  %+v", sent, have)
		}
		if !have.Time.Equal(sent.Time) {
			t.Fatalf("time drift: sent %v got %v", sent.Time, have.Time)
		}
		if !reflect.DeepEqual(have.Data, sent.Data) {
			t.Fatalf("data drift: sent %v got %v", sent.Data, have.Data)
		}
	})

	t.Run(name+"/wildcard_subscription_receives_all_types", func(t *testing.T) {
		b := newBus(t)
		defer func() { _ = b.Close() }()
		rec := newKitRecorder()
		if _, err := b.Subscribe("*", rec.handler()); err != nil {
			t.Fatalf("subscribe wildcard: %v", err)
		}
		first := kitEnvelope(t, events.TopicInteractionCreated)
		second := kitEnvelope(t, events.TopicCallRequested)
		if err := b.Publish(context.Background(), first); err != nil {
			t.Fatalf("publish first: %v", err)
		}
		if err := b.Publish(context.Background(), second); err != nil {
			t.Fatalf("publish second: %v", err)
		}
		if counts := rec.waitForUnique(t, 2); counts[first.ID] != 1 || counts[second.ID] != 1 {
			t.Fatalf("wildcard subscriber must see each type exactly once, got %v", counts)
		}
	})

	t.Run(name+"/multiple_subscribers_fan_out", func(t *testing.T) {
		b := newBus(t)
		defer func() { _ = b.Close() }()
		recA := newKitRecorder()
		recB := newKitRecorder()
		if _, err := b.Subscribe(events.TopicInteractionCreated, recA.handler()); err != nil {
			t.Fatalf("subscribe A: %v", err)
		}
		if _, err := b.Subscribe(events.TopicInteractionCreated, recB.handler()); err != nil {
			t.Fatalf("subscribe B: %v", err)
		}
		env := kitEnvelope(t, events.TopicInteractionCreated)
		if err := b.Publish(context.Background(), env); err != nil {
			t.Fatalf("publish: %v", err)
		}
		if counts := recA.waitForUnique(t, 1); counts[env.ID] != 1 {
			t.Fatalf("subscriber A got %v", counts)
		}
		if counts := recB.waitForUnique(t, 1); counts[env.ID] != 1 {
			t.Fatalf("subscriber B got %v", counts)
		}
	})

	t.Run(name+"/unsubscribe_stops_delivery", func(t *testing.T) {
		b := newBus(t)
		defer func() { _ = b.Close() }()
		recA := newKitRecorder()
		recB := newKitRecorder()
		unsubA, err := b.Subscribe(events.TopicInteractionCreated, recA.handler())
		if err != nil {
			t.Fatalf("subscribe A: %v", err)
		}
		if _, err := b.Subscribe(events.TopicInteractionCreated, recB.handler()); err != nil {
			t.Fatalf("subscribe B: %v", err)
		}
		unsubA()
		unsubA() // idempotent: double-unsubscribe must not panic or error

		env := kitEnvelope(t, events.TopicInteractionCreated)
		if err := b.Publish(context.Background(), env); err != nil {
			t.Fatalf("publish after unsubscribe: %v", err)
		}
		if counts := recB.waitForUnique(t, 1); counts[env.ID] != 1 {
			t.Fatalf("remaining subscriber B got %v", counts)
		}
		time.Sleep(kitSettleNegative) // settle window for any (contract-violating) late delivery
		recA.mustBeEmpty(t)
	})

	t.Run(name+"/empty_topic_rejected", func(t *testing.T) {
		b := newBus(t)
		defer func() { _ = b.Close() }()
		if _, err := b.Subscribe("", newKitRecorder().handler()); err == nil {
			t.Fatal("empty topic must be rejected")
		}
	})

	t.Run(name+"/invalid_envelope_rejected", func(t *testing.T) {
		b := newBus(t)
		defer func() { _ = b.Close() }()
		// Same validation surface as events.Envelope.Validate: an
		// unregistered topic (and a non-uuid id) must fail before fan-out.
		if err := b.Publish(context.Background(), &events.Envelope{ID: "not-a-uuid", Type: "not.a.topic", TenantID: "t"}); err == nil {
			t.Fatal("invalid envelope must be rejected")
		}
	})

	t.Run(name+"/publish_is_bounded", func(t *testing.T) {
		b := newBus(t)
		defer func() { _ = b.Close() }()
		env := kitEnvelope(t, events.TopicInteractionCreated) // built on the test goroutine
		done := make(chan struct{})
		go func() {
			defer close(done)
			for i := 0; i < 100; i++ {
				if err := b.Publish(context.Background(), env); err != nil {
					t.Errorf("publish %d: %v", i, err)
					return
				}
			}
		}()
		select {
		case <-done:
		case <-time.After(kitSettle):
			t.Fatal("publish loop exceeded the settle window — publisher blocked")
		}
	})

	t.Run(name+"/close_is_idempotent_and_unsubscribe_safe", func(t *testing.T) {
		b := newBus(t)
		rec := newKitRecorder()
		unsub, err := b.Subscribe(events.TopicInteractionCreated, rec.handler())
		if err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		if err := b.Close(); err != nil {
			t.Fatalf("first close: %v", err)
		}
		if err := b.Close(); err != nil {
			t.Fatalf("second close must stay nil, got %v", err)
		}
		unsub() // must be a safe no-op after close, never a panic
	})
}

// TestInProcBusConformance pins the reference driver to the shared kit
// unconditionally — the InProc default must keep passing even when the
// NATS driver is absent from the build environment.
func TestInProcBusConformance(t *testing.T) {
	runBusConformance(t, "inproc", func(t *testing.T) Bus {
		return NewInProc()
	})
}
