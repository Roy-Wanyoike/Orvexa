package realtime

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Roy-Wanyoike/orvexa/pkg/events"
)

func TestPublishTenantIsolation(t *testing.T) {
	h := NewHub(10, 100, nil)

	// simulate two registered clients from different tenants
	cA := &client{tenantID: "tenant-a", send: make(chan []byte, 4), closed: make(chan struct{})}
	cB := &client{tenantID: "tenant-b", send: make(chan []byte, 4), closed: make(chan struct{})}
	h.mu.Lock()
	h.clients[cA] = struct{}{}
	h.clients[cB] = struct{}{}
	h.mu.Unlock()

	env, _ := events.New(events.TopicAgentStateChanged, "test", "agt-1", "tenant-a", "",
		map[string]any{"agent_id": "agt-1"})
	h.Publish(env)

	select {
	case msg := <-cA.send:
		if !strings.Contains(string(msg), "tenant-a") {
			t.Fatal("tenant-a client must receive tenant-a events")
		}
	default:
		t.Fatal("matching tenant client must receive the event")
	}
	select {
	case msg := <-cB.send:
		t.Fatalf("foreign tenant client must NOT receive event: %s", msg)
	default:
	}
}

func TestPublishAgentFiltering(t *testing.T) {
	h := NewHub(10, 100, nil)
	agentClient := &client{tenantID: "tenant-a", agentID: "agent-1", send: make(chan []byte, 4), closed: make(chan struct{})}
	otherAgent := &client{tenantID: "tenant-a", agentID: "agent-2", send: make(chan []byte, 4), closed: make(chan struct{})}
	h.mu.Lock()
	h.clients[agentClient] = struct{}{}
	h.clients[otherAgent] = struct{}{}
	h.mu.Unlock()

	// event for agent-1
	env, _ := events.New(events.TopicInteractionAssigned, "test", "int-1", "tenant-a", "",
		map[string]any{"agent_id": "agent-1"})
	h.Publish(env)

	select {
	case <-agentClient.send:
	default:
		t.Fatal("targeted agent must receive the event")
	}
	select {
	case msg := <-otherAgent.send:
		t.Fatalf("other agent must not receive agent-scoped event: %s", msg)
	default:
	}
}

func TestSlowConsumerDroppedNotBlocking(t *testing.T) {
	h := NewHub(10, 100, nil)
	slow := &client{tenantID: "tenant-a", send: make(chan []byte, 1), closed: make(chan struct{})} // buffer 1
	h.mu.Lock()
	h.clients[slow] = struct{}{}
	h.mu.Unlock()

	env, _ := events.New(events.TopicQueueUpdated, "test", "q-1", "tenant-a", "", map[string]any{})
	// fill the buffer and overflow — Publish must not block or panic
	h.Publish(env)
	h.Publish(env)
	h.Publish(env)
}

func TestConnectionCapEnforced(t *testing.T) {
	h := NewHub(1, 2, nil)
	if !h.admit("tenant-a") {
		t.Fatal("first connection admitted")
	}
	if !h.admit("tenant-a") {
		t.Fatal("second connection admitted (cap 2)")
	}
	if h.admit("tenant-a") {
		t.Fatal("third connection must be refused at cap 2")
	}
	h.release("tenant-a")
	if !h.admit("tenant-a") {
		t.Fatal("after release a slot opens")
	}
}

func TestTenantScopedCount(t *testing.T) {
	h := NewHub(10, 100, nil)
	if h.Count() != 0 {
		t.Fatal("empty hub must count 0")
	}
	c := &client{tenantID: "t", send: make(chan []byte, 1), closed: make(chan struct{})}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	if h.Count() != 1 {
		t.Fatal("count must track clients")
	}
}

var _ = httptest.NewRecorder
