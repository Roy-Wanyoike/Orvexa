package whatsappcloud

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Roy-Wanyoike/orvexa/internal/comms/conformance"
)

// TestWhatsAppCloudMessagingConformance embeds the conformance kit's full
// messaging behavioral contract (internal/comms/conformance — read its
// README for the wiring rules) against the adapter driving the httptest fake
// Graph server.
//
// Wiring (rule 1 — one Recorder, wired twice): the SAME recorder is the
// adapter's delivery hook (via newTestProvider) and Options.Recorder.
//
// Async rule (rule 2): the adapter's HandleWebhook is a synchronous
// translator, but Meta's statuses arrive asynchronously — the fake stands in
// for that, firing each accepted send's status callback on its own
// goroutine, relayed FIFO by statusRelay (per-message sent→delivered is
// Meta's ordering guarantee; cross-message order follows send acceptance).
// RequireAsyncEvents: true is therefore the honest declaration.
//
// EnforceSendValidation: the adapter re-runs messaging.ValidateMessage as
// defense in depth with the core codes verbatim (see
// TestSendReRunsCoreGateVerbatim), so it opts into the kit's re-validation
// scenario — one taxonomy across the platform.
func TestWhatsAppCloudMessagingConformance(t *testing.T) {
	fake := newFakeGraph(t)
	rec := conformance.NewRecorder()
	p := newTestProvider(t, fake, rec, nil)

	// Meta's async status callback, stood in by the fake and serialized by
	// the FIFO relay: every accepted send yields sent+delivered receipts
	// through the adapter's HandleWebhook into the recorder.
	relay := newStatusRelay(t, p)
	fake.onSend = relay.enqueue

	conformance.RunMessagingConformance(t, p, conformance.Options{
		Recorder:              rec,
		ProviderName:          ProviderName,
		RequireAsyncEvents:    true,
		EventSettleTimeout:    2 * time.Second,
		EnforceSendValidation: true,
	})
}

// statusRelay serializes the fake's async status callbacks into one FIFO
// translation goroutine. The fake fires every accepted send's webhook on its
// own goroutine (as Meta does across connections); the relay preserves
// per-message sent→delivered order and follows send-acceptance order across
// messages, so lifecycle scenarios are deterministic under -race.
type statusRelay struct {
	t *testing.T
	p *Provider

	mu      sync.Mutex
	cond    *sync.Cond
	queue   []string
	stopped bool
	done    chan struct{}
}

// newStatusRelay starts the relay and registers its shutdown with the test.
func newStatusRelay(t *testing.T, p *Provider) *statusRelay {
	t.Helper()
	r := &statusRelay{t: t, p: p, done: make(chan struct{})}
	r.cond = sync.NewCond(&r.mu)
	go func() {
		defer close(r.done)
		for {
			r.mu.Lock()
			for len(r.queue) == 0 && !r.stopped {
				r.cond.Wait()
			}
			if len(r.queue) == 0 && r.stopped {
				r.mu.Unlock()
				return
			}
			wamid := r.queue[0]
			r.queue = r.queue[1:]
			r.mu.Unlock()

			r.deliver(wamid)
		}
	}()
	t.Cleanup(func() {
		r.mu.Lock()
		r.stopped = true
		r.mu.Unlock()
		r.cond.Broadcast()
		<-r.done
	})
	return r
}

// enqueue is wired as the fake's onSend hook (non-blocking: the caller is
// the fake's handler goroutine and must never stall on the relay).
func (r *statusRelay) enqueue(wamid string) {
	r.mu.Lock()
	r.queue = append(r.queue, wamid)
	r.mu.Unlock()
	r.cond.Broadcast()
}

// deliver translates one accepted send's async receipts through the
// adapter's HandleWebhook — Meta's per-message sent→delivered pair.
func (r *statusRelay) deliver(wamid string) {
	awaitWAMIDRegistration(r.t, r.p, wamid)
	body := statusWebhookBody(r.t, testPhoneNumberID,
		graphStatusEntry(wamid, "sent"),
		graphStatusEntry(wamid, "delivered"),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.p.HandleWebhook(ctx, body); err != nil {
		r.t.Errorf("relay: status webhook for %s: %v", wamid, err)
	}
}
