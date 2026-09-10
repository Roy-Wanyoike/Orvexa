//go:build integration

// NATS integration suite ([O-24], issue #33). Runs ONLY against a real
// JetStream server — the compose path:
//
//	docker compose -f docker-compose.dev.yml up -d nats
//	ORVEXA_NATS_URL=nats://localhost:4222 go test -race -tags=integration ./internal/platform/bus/
//
// It skips cleanly (no failure) whenever ORVEXA_NATS_URL is unset, so the
// default `go test ./...` and CI matrices are unaffected.
package bus

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Roy-Wanyoike/orvexa/pkg/events"
)

// integrationNATSBus builds the driver against the compose server.
func integrationNATSBus(t *testing.T) *NATS {
	t.Helper()
	url := os.Getenv("ORVEXA_NATS_URL")
	if url == "" {
		t.Skip("ORVEXA_NATS_URL not set — skipping NATS integration suite (compose path: docker compose -f docker-compose.dev.yml up -d nats)")
	}
	drv, err := NewNATS(context.Background(), NATSConfig{URL: url, ConnectTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("connect to %s: %v", url, err)
	}
	t.Cleanup(func() { _ = drv.Close() })
	return drv
}

// TestNATSBusConformanceIntegration proves the shared kit against a real
// JetStream server — full parity with the InProc reference on live wire
// semantics (JSON envelopes, durable pull consumers, fan-out, unsubscribe,
// close).
func TestNATSBusConformanceIntegration(t *testing.T) {
	runBusConformance(t, "nats-integration", func(t *testing.T) Bus {
		return integrationNATSBus(t)
	})
}

// TestNATSHandlerErrorTriggersBoundedRedelivery proves the ack/nak path on
// a live server: a failing handler must see the envelope again (at-least-
// once), and once the handler recovers the message is acknowledged and the
// stream drains to zero pending.
func TestNATSHandlerErrorTriggersBoundedRedelivery(t *testing.T) {
	drv := integrationNATSBus(t)

	var calls atomic.Int64
	delivered := make(chan *events.Envelope, 16)
	handler := func(_ context.Context, env *events.Envelope) error {
		if calls.Add(1) == 1 {
			return errors.New("transient failure") // first attempt fails -> nak -> redelivery
		}
		select {
		case delivered <- env:
		default:
		}
		return nil
	}
	unsub, err := drv.Subscribe(events.TopicInteractionCreated, handler)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer unsub()

	env := kitEnvelope(t, events.TopicInteractionCreated)
	if err := drv.Publish(context.Background(), env); err != nil {
		t.Fatalf("publish: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case got := <-delivered:
			if got.ID != env.ID {
				t.Fatalf("redelivered envelope id %s, want %s", got.ID, env.ID)
			}
			if n := calls.Load(); n < 2 {
				t.Fatalf("handler recovered after %d calls, want >=2 (at-least-once)", n)
			}
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("envelope not redelivered after handler recovery (calls=%d) — ack/nak path broken", calls.Load())
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestNATSDurableSurvivesDriverRestart proves durable pull consumers make
// delivery survive process restarts: events published while the driver is
// down are delivered to the re-subscribed durable, while already-acked
// events are not replayed (at-least-once, no duplicates beyond contract).
func TestNATSDurableSurvivesDriverRestart(t *testing.T) {
	url := os.Getenv("ORVEXA_NATS_URL")
	if url == "" {
		t.Skip("ORVEXA_NATS_URL not set — skipping NATS integration suite")
	}

	// First process: subscribe (creates the durable), publish, consume, ack.
	drv1, err := NewNATS(context.Background(), NATSConfig{URL: url})
	if err != nil {
		t.Fatalf("connect 1: %v", err)
	}
	rec1 := newKitRecorder()
	unsub1, err := drv1.Subscribe(events.TopicInteractionCreated, rec1.handler())
	if err != nil {
		t.Fatalf("subscribe 1: %v", err)
	}
	first := kitEnvelope(t, events.TopicInteractionCreated)
	if err := drv1.Publish(context.Background(), first); err != nil {
		t.Fatalf("publish 1: %v", err)
	}
	rec1.waitForUnique(t, 1)
	unsub1()
	// Drain cleanly: give the ack a moment to land before teardown.
	time.Sleep(200 * time.Millisecond)
	if err := drv1.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}

	// Second process: fresh driver, same durable name (same topic, same
	// per-topic sequence). The event published while "down" must arrive;
	// the acked first event must not replay.
	drv2, err := NewNATS(context.Background(), NATSConfig{URL: url})
	if err != nil {
		t.Fatalf("connect 2: %v", err)
	}
	defer func() { _ = drv2.Close() }()

	// Published before the consumer (re)subscribes on driver 2.
	whileDown := kitEnvelope(t, events.TopicInteractionCreated)
	if err := drv2.Publish(context.Background(), whileDown); err != nil {
		t.Fatalf("publish while down: %v", err)
	}

	rec2 := newKitRecorder()
	unsub2, err := drv2.Subscribe(events.TopicInteractionCreated, rec2.handler())
	if err != nil {
		t.Fatalf("subscribe 2: %v", err)
	}
	defer unsub2()

	counts := rec2.waitForUnique(t, 1)
	if counts[whileDown.ID] != 1 {
		t.Fatalf("event published while the driver was down must be delivered by the durable, got %v", counts)
	}
	time.Sleep(500 * time.Millisecond) // settle window for any (contract-breaking) replay
	rec2.mustBeEmpty(t)                // the acked first event stays acked across restarts
}
