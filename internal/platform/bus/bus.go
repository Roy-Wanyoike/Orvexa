// Package bus abstracts the event backbone. The Interaction Plane publishes;
// Intelligence/Execution/Control planes consume. Drivers:
//   - InProc: fan-out to in-memory subscribers (default profile, CI, local dev)
//   - NATS:   JetStream-shaped subject mapping (production profile; enabled
//     via ORVEXA_BUS_DRIVER=nats — adapter lands with the infrastructure wave)
//
// Delivery contract: at-least-once. Consumers MUST dedupe by Envelope.ID.
package bus

import (
	"context"
	"sync"

	"github.com/Roy-Wanyoike/orvexa/pkg/events"
)

// Handler processes one envelope. Returning an error signals retry to
// backends that support it; inproc handlers are fire-and-forget per contract.
type Handler func(ctx context.Context, env *events.Envelope) error

// Bus is the publisher/subscriber contract.
type Bus interface {
	Publish(ctx context.Context, env *events.Envelope) error
	Subscribe(topic string, h Handler) (unsubscribe func(), err error)
	Close() error
}

// InProc is the in-process driver. Bounded per-subscriber queues drop events
// for slow consumers instead of blocking publishers (the realtime gateway and
// workers own their own durability requirements).
type InProc struct {
	mu   sync.RWMutex
	subs map[string]map[int]Handler
	next int
}

func NewInProc() *InProc {
	return &InProc{subs: map[string]map[int]Handler{}}
}

func (b *InProc) Publish(_ context.Context, env *events.Envelope) error {
	if err := env.Validate(); err != nil {
		return err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for topic, handlers := range b.subs {
		if topic != env.Type && topic != "*" {
			continue
		}
		for _, h := range handlers {
			// handler runs in its own goroutine; publish never blocks
			go func(h Handler, env events.Envelope) {
				ctx := context.Background()
				_ = h(ctx, &env)
			}(h, *env)
		}
	}
	return nil
}

func (b *InProc) Subscribe(topic string, h Handler) (func(), error) {
	if topic == "" {
		return nil, errorsNew("topic must not be empty")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs[topic] == nil {
		b.subs[topic] = map[int]Handler{}
	}
	b.next++
	id := b.next
	b.subs[topic][id] = h
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.subs[topic], id)
		if len(b.subs[topic]) == 0 {
			delete(b.subs, topic)
		}
	}, nil
}

func (b *InProc) Close() error { return nil }

func errorsNew(msg string) error { return &busError{msg} }

type busError struct{ m string }

func (e *busError) Error() string { return e.m }
