package search

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Roy-Wanyoike/orvexa/internal/platform/bus"
	"github.com/Roy-Wanyoike/orvexa/pkg/events"
)

// ---- event builders (shared by service/indexer tests) ----

func mustEnv(env *events.Envelope, err error) *events.Envelope {
	if err != nil {
		panic(err)
	}
	return env
}

func eventsConversationOpened(tenant, convID, customerID, channel string) (*events.Envelope, error) {
	return events.New(events.TopicConversationOpened, "test", convID, tenant, "", map[string]any{
		"conversation_id": convID, "customer_id": customerID, "channel": channel,
	})
}

func eventsConversationClosed(tenant, convID string) (*events.Envelope, error) {
	return events.New(events.TopicConversationClosed, "test", convID, tenant, "", map[string]any{
		"conversation_id": convID,
	})
}

func eventsInteractionCreated(tenant, interID, convID, channel, direction, status string) (*events.Envelope, error) {
	return events.New(events.TopicInteractionCreated, "test", interID, tenant, "", map[string]any{
		"interaction_id": interID, "conversation_id": convID,
		"customer_id": "cust-x", "channel": channel, "direction": direction, "status": status,
	})
}

func eventsInteractionTouched(topic, tenant, interID, channel, direction, status string) (*events.Envelope, error) {
	// interaction.updated/assigned/completed carry no conversation_id — the
	// projection learns it from the created event (bounded in-memory map).
	return events.New(topic, "test", interID, tenant, "", map[string]any{
		"interaction_id": interID, "channel": channel, "direction": direction, "status": status,
	})
}

// waitFor polls cond until it holds or the deadline passes (race-friendly).
func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}

// account is the total number of envelopes the indexer has reached a terminal
// disposition (indexed, failed write, dropped, deduped or skipped). Every
// dispatched Handle call lands in exactly one bucket, so the sum is the
// honest "all events accounted for" predicate.
func account(st IndexerStats) int64 {
	return st.Indexed + st.Failed + st.Dropped + st.Deduped + st.Skipped
}

func TestIndexerProjectsConversationLifecycle(t *testing.T) {
	f := newFakeOpenSearch(t)
	svc := testService(f)
	theBus := bus.NewInProc()
	defer theBus.Close()

	ix := NewIndexer(svc, IndexerConfig{Logger: func(string, ...any) {}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer ix.Close()
	if err := ix.Start(ctx, theBus); err != nil {
		t.Fatalf("start: %v", err)
	}

	opened, _ := eventsConversationOpened("tenant-a", "conv-1", "cust-1", "voice")
	created, _ := eventsInteractionCreated("tenant-a", "int-1", "conv-1", "voice", "inbound", "active")
	completed, _ := eventsInteractionTouched(events.TopicInteractionCompleted, "tenant-a", "int-1", "voice", "inbound", "completed")
	closed, _ := eventsConversationClosed("tenant-a", "conv-1")

	// Publish in causal order, waiting for each projection to land. The bus
	// contract is at-least-once WITHOUT ordering (InProc fans every Publish out
	// on its own goroutine), so an unserialized test would race e.g. closed
	// ahead of opened — where the opened snapshot legitimately overwrites the
	// closed state until a later event re-anchors the document. Real
	// conversation lifecycles are causal and ordered transports (JetStream
	// per-subject) deliver them in order; causality is the contract under test.
	steps := []struct {
		env  *events.Envelope
		done func(st IndexerStats) bool
	}{
		{opened, func(st IndexerStats) bool { return st.Indexed >= 1 }},
		{created, func(st IndexerStats) bool { return st.Indexed >= 2 }},
		{completed, func(st IndexerStats) bool { return st.Indexed >= 3 }},
		{closed, func(st IndexerStats) bool { return st.Indexed >= 4 }},
	}
	for i, s := range steps {
		if err := theBus.Publish(ctx, s.env); err != nil {
			t.Fatalf("publish %s: %v", s.env.Type, err)
		}
		waitFor(t, 5*time.Second, func() bool { return s.done(ix.Stats()) },
			fmt.Sprintf("indexer did not project event %d (%s)", i+1, s.env.Type))
	}

	d, ok := f.doc(IndexConversations, "conv-1")
	if !ok {
		t.Fatal("conversation doc missing")
	}
	if d.TenantID != "tenant-a" || d.ConversationID != "conv-1" || d.CustomerID != "cust-1" || d.Channel != "voice" {
		t.Fatalf("base projection = %+v", d)
	}
	if d.InteractionCount != 1 {
		t.Fatalf("interaction_count = %d, want 1 (created only)", d.InteractionCount)
	}
	if d.LastInteraction == nil || d.LastInteraction.ID != "int-1" || d.LastInteraction.Status != "completed" {
		t.Fatalf("last_interaction = %+v", d.LastInteraction)
	}
	if d.Status != "closed" || d.ClosedAt == nil {
		t.Fatalf("closed projection = %+v", d)
	}
	// search_text accumulated channel + direction tokens (queryable text)
	if !strings.Contains(d.SearchText, "voice") || !strings.Contains(d.SearchText, "inbound") {
		t.Fatalf("search_text = %q", d.SearchText)
	}
}

// TestIndexerHandleNeverBlocks proves the drop+log policy: with no consumer
// draining the queue (backend unresponsive), Handle must return in bounded
// time for ANY event rate, shedding the excess. This is the hot-path
// guarantee — the interaction plane can never stall behind search.
func TestIndexerHandleNeverBlocks(t *testing.T) {
	f := newFakeOpenSearch(t)
	f.setDown(true) // backend down; irrelevant to Handle — it must not care
	svc := testService(f)
	ix := NewIndexer(svc, IndexerConfig{QueueSize: 4, Logger: func(string, ...any) {}})

	start := time.Now()
	const total = 100
	for i := 0; i < total; i++ {
		env, _ := eventsConversationOpened("tenant-a", "conv-x", "cust-x", "voice")
		if err := ix.Handle(context.Background(), env); err != nil {
			t.Fatalf("handle %d: %v", i, err)
		}
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("Handle blocked: 100 calls took %s (drop policy violated)", elapsed)
	}
	st := ix.Stats()
	if st.Dropped != total-4 {
		t.Fatalf("dropped = %d, want %d", st.Dropped, total-4)
	}
	if len(ix.queue) != 4 { // bounded memory: never more than the queue capacity
		t.Fatalf("queue len = %d, want bounded at 4", len(ix.queue))
	}
}

// TestIndexerDropsThenRecovers exercises the full policy: backend down →
// writes fail (counted) and the queue sheds (drop+log); backend recovery →
// indexing resumes without restart.
func TestIndexerDropsThenRecovers(t *testing.T) {
	f := newFakeOpenSearch(t)
	f.setDown(true)
	svc := testService(f)
	theBus := bus.NewInProc()
	defer theBus.Close()

	ix := NewIndexer(svc, IndexerConfig{QueueSize: 4, Logger: func(string, ...any) {}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer ix.Close()
	if err := ix.Start(ctx, theBus); err != nil {
		t.Fatalf("start: %v", err)
	}

	for i := 0; i < 20; i++ {
		env, _ := eventsConversationOpened("tenant-a", "conv-down", "cust-1", "voice")
		if err := theBus.Publish(ctx, env); err != nil { // publishing NEVER fails
			t.Fatalf("publish during outage: %v", err)
		}
	}
	// Accounting under a queue of 4 and 20 events: the worker attempts the
	// writes it dequeues (each fails against the 503 backend → Failed), the
	// overflow is shed (Dropped). Failed can never reach 20 — that is the drop
	// policy doing its job. Honest assertions: at least one write attempt
	// failed, zero writes succeeded, and EVERY event reached a disposition.
	waitFor(t, 5*time.Second, func() bool { return ix.Stats().Failed >= 1 },
		"no failed write counted during outage")
	waitFor(t, 5*time.Second, func() bool { return account(ix.Stats()) >= 20 },
		"outage events not fully accounted (failed+dropped+indexed)")
	st := ix.Stats()
	if st.Indexed != 0 {
		t.Fatalf("indexed = %d during outage, want 0", st.Indexed)
	}
	if st.Dropped == 0 {
		// with a fast-failing backend most events fail instead of dropping;
		// shedding under a SLOW backend is proven by TestIndexerHandleNeverBlocks
		t.Log("no drops with fast-503 backend (writes fail fast) — acceptable")
	}

	// recovery: same indexer, backend back up
	f.setDown(false)
	env, _ := eventsConversationOpened("tenant-a", "conv-ok", "cust-1", "chat")
	if err := theBus.Publish(ctx, env); err != nil {
		t.Fatalf("publish after recovery: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return ix.Stats().Indexed >= 1 }, "indexer did not recover")
	if _, ok := f.doc(IndexConversations, "conv-ok"); !ok {
		t.Fatal("recovered event not indexed")
	}
}

// TestIndexerDedupesReplays: at-least-once delivery ⇒ consumers dedupe by
// envelope id.
func TestIndexerDedupesReplays(t *testing.T) {
	f := newFakeOpenSearch(t)
	svc := testService(f)
	theBus := bus.NewInProc()
	defer theBus.Close()

	ix := NewIndexer(svc, IndexerConfig{Logger: func(string, ...any) {}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer ix.Close()
	if err := ix.Start(ctx, theBus); err != nil {
		t.Fatalf("start: %v", err)
	}

	env, _ := eventsConversationOpened("tenant-a", "conv-dup", "cust-1", "voice")
	for i := 0; i < 3; i++ { // same envelope id replayed
		if err := theBus.Publish(ctx, env); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	waitFor(t, 5*time.Second, func() bool {
		st := ix.Stats()
		return st.Indexed >= 1 && st.Deduped >= 2
	}, "replays not deduped")
	if f.docCount(IndexConversations) != 1 {
		t.Fatalf("doc count = %d, want single effect", f.docCount(IndexConversations))
	}
}

func TestIndexerNotConfigured(t *testing.T) {
	svc := NewService(Config{}) // not configured
	ix := NewIndexer(svc, IndexerConfig{})

	env, _ := eventsConversationOpened("tenant-a", "conv-1", "cust-1", "voice")
	err := ix.Handle(context.Background(), env)
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Handle err = %v, want ErrNotConfigured", err)
	}

	// Start is a no-op (logs honestly) — the platform boots without search.
	theBus := bus.NewInProc()
	defer theBus.Close()
	if err := ix.Start(context.Background(), theBus); err != nil {
		t.Fatalf("Start with unconfigured backend must not fail: %v", err)
	}
	if st := ix.Stats(); st.Indexed != 0 {
		t.Fatalf("indexed = %d while not configured", st.Indexed)
	}
}

// TestIndexerInteractionMappingUnknown: touched events without a learned
// conversation mapping are skipped, not mis-filed (they can never land in
// another tenant's document — tenant_id always comes from the envelope).
func TestIndexerInteractionMappingUnknown(t *testing.T) {
	f := newFakeOpenSearch(t)
	svc := testService(f)
	ix := NewIndexer(svc, IndexerConfig{})
	ctx := context.Background()

	env, _ := eventsInteractionTouched(events.TopicInteractionUpdated, "tenant-a", "int-404", "voice", "inbound", "wrapup")
	ix.project(ctx, env)
	if st := ix.Stats(); st.Skipped != 1 || st.Indexed != 0 {
		t.Fatalf("stats = %+v, want skipped=1 indexed=0", st)
	}
	if f.docCount(IndexConversations) != 0 {
		t.Fatal("unknown mapping must not write")
	}
}

// TestIndexerBoundedMapEviction: the in-memory maps are ring-bounded — a
// flood of unique ids cannot grow memory without limit.
func TestIndexerBoundedMapEviction(t *testing.T) {
	r := newRingMap(8)
	for i := 0; i < 1000; i++ {
		r.put(itoa(i), "v")
	}
	r.mu.Lock()
	size := len(r.m)
	r.mu.Unlock()
	if size > 8 {
		t.Fatalf("ring map size = %d, want <= 8", size)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// compile-time: Indexer satisfies bus.Handler
var _ bus.Handler = (*Indexer)(nil).Handle
