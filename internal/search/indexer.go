package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Roy-Wanyoike/orvexa/internal/platform/bus"
	"github.com/Roy-Wanyoike/orvexa/pkg/events"
)

// Topics the indexer projects into orvexa-conversations. Everything else on
// the bus is ignored.
var projectedTopics = []string{
	events.TopicConversationOpened,
	events.TopicConversationClosed,
	events.TopicInteractionCreated,
	events.TopicInteractionUpdated,
	events.TopicInteractionAssigned,
	events.TopicInteractionCompleted,
}

// Indexer bounds (memory safety under backpressure).
const (
	DefaultQueueSize  = 1024 // events buffered between the bus and OpenSearch
	DefaultDedupeSize = 8192 // envelope ids remembered for at-least-once replays
	maxMappingBody    = 1 << 20
)

// IndexerConfig configures the projection consumer.
type IndexerConfig struct {
	QueueSize  int    // bounded event queue; default DefaultQueueSize
	DedupeSize int    // envelope-id ring; default DefaultDedupeSize
	Logger     Logger // structured logger; never receives payload bodies
}

// IndexerStats are counters for observability (all monotonic).
type IndexerStats struct {
	Indexed int64
	Dropped int64 // shed by the bounded queue (drop + log policy)
	Failed  int64 // write errors against OpenSearch
	Deduped int64 // at-least-once replays skipped by envelope id
	Skipped int64 // events not projectable (e.g. unknown interaction→conversation)
}

// Indexer consumes conversation/interaction events from the bus and projects
// them into the orvexa-conversations index (one document per conversation,
// updated by interaction events — an event-sourced read model).
//
// Backpressure contract: the bus handler NEVER blocks. Enqueue is a
// select/default onto a bounded channel; when the queue is full (typically
// because OpenSearch is down or slow) the event is DROPPED with a structured
// log entry and a counter bump. Publishing health is always preserved —
// search lags instead. Search is eventually-consistent by design; the source
// of truth remains PostgreSQL (reindex from the outbox/DB is the recovery
// path, not per-event retry).
type Indexer struct {
	svc    *Service
	log    Logger
	queue  chan *events.Envelope
	quit   chan struct{}
	closed atomic.Bool
	once   sync.Once

	mu    sync.Mutex
	unsub []func()

	seen   *ringMap // envelope id dedupe (at-least-once contract)
	convOf *ringMap // interaction id → conversation id (learned from created events)

	indexed atomic.Int64
	dropped atomic.Int64
	failed  atomic.Int64
	deduped atomic.Int64
	skipped atomic.Int64
}

// NewIndexer builds the projection consumer. It does not subscribe — call
// Start with a bus. The indexer works even when the service is not
// configured, but Start refuses to subscribe (and says so) because indexing
// could never succeed.
func NewIndexer(svc *Service, cfg IndexerConfig) *Indexer {
	qs := cfg.QueueSize
	if qs <= 0 {
		qs = DefaultQueueSize
	}
	ds := cfg.DedupeSize
	if ds <= 0 {
		ds = DefaultDedupeSize
	}
	log := cfg.Logger
	if log == nil {
		log = noopLogger
	}
	return &Indexer{
		svc:    svc,
		log:    log,
		queue:  make(chan *events.Envelope, qs),
		quit:   make(chan struct{}),
		seen:   newRingMap(ds),
		convOf: newRingMap(ds),
	}
}

// Start ensures the index exists and subscribes to the projected topics.
// When OpenSearch is not configured it logs the honest reason and returns
// nil — search stays off, the rest of the platform is unaffected.
func (ix *Indexer) Start(ctx context.Context, b bus.Bus) error {
	if !ix.svc.Configured() {
		ix.log("search indexer disabled", "reason", "not_configured", "env", EnvURL)
		return nil
	}
	if err := ix.ensureIndex(ctx); err != nil {
		// Non-fatal: the cluster may come up later; per-event writes will
		// surface failures through the failed counter and logs.
		ix.log("search index ensure failed (indexing continues)", "err", err.Error())
	}
	for _, topic := range projectedTopics {
		unsub, err := b.Subscribe(topic, ix.Handle)
		if err != nil {
			ix.unsubAll()
			return fmt.Errorf("search indexer subscribe %s: %w", topic, err)
		}
		ix.mu.Lock()
		ix.unsub = append(ix.unsub, unsub)
		ix.mu.Unlock()
	}
	go ix.run(ctx)
	return nil
}

// Close unsubscribes and stops the worker. In-flight queue contents are
// abandoned (counted as dropped on next observation) — no goroutine leaks.
func (ix *Indexer) Close() {
	ix.once.Do(func() {
		ix.unsubAll()
		close(ix.quit)
	})
}

// Stats returns a snapshot of the counters.
func (ix *Indexer) Stats() IndexerStats {
	return IndexerStats{
		Indexed: ix.indexed.Load(),
		Dropped: ix.dropped.Load(),
		Failed:  ix.failed.Load(),
		Deduped: ix.deduped.Load(),
		Skipped: ix.skipped.Load(),
	}
}

// Handle implements bus.Handler. It is non-blocking by construction: the
// bus fires handlers on their own goroutines and this method must return in
// microseconds regardless of OpenSearch health.
func (ix *Indexer) Handle(ctx context.Context, env *events.Envelope) error {
	if env == nil || !isProjected(env.Type) {
		return nil
	}
	if !ix.svc.Configured() {
		return ErrNotConfigured
	}
	if ix.closed.Load() {
		return nil
	}
	if ix.seen.putIfAbsent(env.ID) {
		ix.deduped.Add(1) // at-least-once replay — single effect
		return nil
	}
	select {
	case ix.queue <- env:
	default:
		// DROP + structured-log policy (issue #36): the queue is full, the
		// backend cannot keep up. Shedding keeps publishers unblocked and
		// memory bounded. Never returns an error — a retry signal would
		// reintroduce backpressure into the hot path.
		n := ix.dropped.Add(1)
		ix.log("search indexer drop", "event_id", env.ID, "topic", env.Type, "dropped_total", n)
	}
	return nil
}

func (ix *Indexer) run(ctx context.Context) {
	for {
		select {
		case <-ix.quit:
			return
		case <-ctx.Done():
			return
		case env := <-ix.queue:
			ix.project(ctx, env)
		}
	}
}

func (ix *Indexer) project(ctx context.Context, env *events.Envelope) {
	now := time.Now().UTC()
	data := env.Data
	str := func(key string) string {
		v, _ := data[key].(string)
		return v
	}

	switch env.Type {
	case events.TopicConversationOpened:
		convID := firstNonEmpty(str("conversation_id"), env.Subject)
		if convID == "" {
			ix.skipped.Add(1)
			return
		}
		d := conversationDoc(env.TenantID, convID, str("customer_id"), str("channel"), now)
		ix.write(ctx, env, http.MethodPut, "/_doc/"+convID, d)

	case events.TopicConversationClosed:
		convID := firstNonEmpty(str("conversation_id"), env.Subject)
		if convID == "" {
			ix.skipped.Add(1)
			return
		}
		body := map[string]any{
			"doc": map[string]any{
				"tenant_id":       env.TenantID,
				"conversation_id": convID,
				"status":          "closed",
				"closed_at":       now,
			},
			"doc_as_upsert": true,
		}
		ix.write(ctx, env, http.MethodPost, "/_update/"+convID, body)

	case events.TopicInteractionCreated:
		convID := str("conversation_id")
		interID := firstNonEmpty(str("interaction_id"), env.Subject)
		if convID == "" || interID == "" {
			ix.skipped.Add(1)
			return
		}
		ix.convOf.put(interID, convID)
		last := &LastInteraction{
			ID: interID, Channel: str("channel"),
			Direction: str("direction"), Status: str("status"),
		}
		body := map[string]any{
			"script": map[string]any{
				"source": scriptInteractionCreated,
				"lang":   "painless",
				"params": map[string]any{
					"last_interaction": last,
					"search_text":      strings.ToLower(strings.TrimSpace(str("channel") + " " + str("direction"))),
					"at":               now,
				},
			},
			"upsert": upsertDoc(env.TenantID, convID, str("customer_id"), str("channel"), last, now),
		}
		ix.write(ctx, env, http.MethodPost, "/_update/"+convID, body)

	case events.TopicInteractionUpdated, events.TopicInteractionAssigned, events.TopicInteractionCompleted:
		interID := firstNonEmpty(str("interaction_id"), env.Subject)
		if interID == "" {
			ix.skipped.Add(1)
			return
		}
		convID, ok := ix.convOf.get(interID)
		if !ok {
			// The created event was never projected (e.g. dropped under
			// backpressure before this worker learned the mapping). Skipping
			// is the honest move; the next interaction.created re-anchors the
			// conversation document.
			ix.skipped.Add(1)
			return
		}
		last := &LastInteraction{
			ID: interID, Channel: str("channel"),
			Direction: str("direction"), Status: str("status"),
		}
		body := map[string]any{
			"script": map[string]any{
				"source": scriptInteractionTouched,
				"lang":   "painless",
				"params": map[string]any{
					"last_interaction": last,
					"search_text":      strings.ToLower(strings.TrimSpace(str("channel") + " " + str("direction"))),
					"at":               now,
				},
			},
			"upsert": upsertDoc(env.TenantID, convID, str("customer_id"), str("channel"), last, now),
		}
		ix.write(ctx, env, http.MethodPost, "/_update/"+convID, body)

	default:
		// not a projected topic (defensive; Subscribe already filters)
	}
}

// write performs one REST call against OpenSearch. Failures bump the failed
// counter and log structurally — they never panic, never retry inline (the
// read model self-heals from later events) and never reach callers.
func (ix *Indexer) write(ctx context.Context, env *events.Envelope, method, path string, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		ix.failed.Add(1)
		ix.log("search indexer marshal failed", "event_id", env.ID, "topic", env.Type, "err", err.Error())
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, ix.svc.cfg.Timeout)
	defer cancel()

	endpoint := strings.TrimSuffix(ix.svc.cfg.URL, "/") + "/" + ix.svc.cfg.Index + path
	req, err := http.NewRequestWithContext(writeCtx, method, endpoint, bytes.NewReader(raw))
	if err != nil {
		ix.failed.Add(1)
		ix.log("search indexer request build failed", "event_id", env.ID, "err", err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := ix.svc.client.Do(req)
	if err != nil {
		ix.failed.Add(1)
		ix.log("search indexer write failed", "event_id", env.ID, "topic", env.Type, "err", err.Error())
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxMappingBody))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		ix.failed.Add(1)
		ix.log("search indexer write rejected", "event_id", env.ID, "topic", env.Type,
			"status", resp.StatusCode)
		return
	}
	ix.indexed.Add(1)
}

// ensureIndex creates orvexa-conversations with the tenant filter field
// (tenant_id as exact keyword) and the projection mappings. Index-exists is
// treated as success so restarts are idempotent.
func (ix *Indexer) ensureIndex(ctx context.Context) error {
	endpoint := strings.TrimSuffix(ix.svc.cfg.URL, "/") + "/" + ix.svc.cfg.Index
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := ix.svc.client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxMappingBody))
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil // already exists
	}

	mappings := map[string]any{
		"settings": map[string]any{
			"index": map[string]any{"number_of_shards": 1, "number_of_replicas": 0},
		},
		"mappings": map[string]any{
			"properties": map[string]any{
				// MANDATORY tenant isolation field: exact keyword, filtered
				// server-side on every query.
				"tenant_id":         map[string]any{"type": "keyword"},
				"conversation_id":   map[string]any{"type": "keyword"},
				"customer_id":       map[string]any{"type": "keyword"},
				"channel":           map[string]any{"type": "keyword"},
				"direction":         map[string]any{"type": "keyword"},
				"status":            map[string]any{"type": "keyword"},
				"interaction_count": map[string]any{"type": "integer"},
				"search_text":       map[string]any{"type": "text"},
				"last_interaction": map[string]any{
					"properties": map[string]any{
						"id":        map[string]any{"type": "keyword"},
						"channel":   map[string]any{"type": "keyword"},
						"direction": map[string]any{"type": "keyword"},
						"status":    map[string]any{"type": "keyword"},
					},
				},
				"opened_at":           map[string]any{"type": "date"},
				"updated_at":          map[string]any{"type": "date"},
				"last_interaction_at": map[string]any{"type": "date"},
				"closed_at":           map[string]any{"type": "date"},
			},
		},
	}
	raw, err := json.Marshal(mappings)
	if err != nil {
		return err
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err = ix.svc.client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxMappingBody))
	resp.Body.Close()
	// 400 with resource_already_exists_exception means a concurrent creator
	// won the race — that is success for our purposes.
	if resp.StatusCode == http.StatusBadRequest {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("opensearch index create returned status %d", resp.StatusCode)
	}
	return nil
}

// refresh forces the index to be searchable (used by tests and the
// integration suite; never called on the hot path).
func (ix *Indexer) refresh(ctx context.Context) error {
	endpoint := strings.TrimSuffix(ix.svc.cfg.URL, "/") + "/" + ix.svc.cfg.Index + "/_refresh"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := ix.svc.client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxMappingBody))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("opensearch refresh returned status %d", resp.StatusCode)
	}
	return nil
}

func (ix *Indexer) unsubAll() {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	for _, u := range ix.unsub {
		u()
	}
	ix.unsub = nil
}

func isProjected(topic string) bool {
	for _, t := range projectedTopics {
		if t == topic {
			return true
		}
	}
	return false
}

// ---- projection helpers ----

// doc is the orvexa-conversations projection document.
type doc struct {
	TenantID          string           `json:"tenant_id"`
	ConversationID    string           `json:"conversation_id"`
	CustomerID        string           `json:"customer_id,omitempty"`
	Channel           string           `json:"channel,omitempty"`
	Status            string           `json:"status,omitempty"`
	InteractionCount  int              `json:"interaction_count,omitempty"`
	LastInteraction   *LastInteraction `json:"last_interaction,omitempty"`
	SearchText        string           `json:"search_text,omitempty"`
	OpenedAt          *time.Time       `json:"opened_at,omitempty"`
	UpdatedAt         *time.Time       `json:"updated_at,omitempty"`
	LastInteractionAt *time.Time       `json:"last_interaction_at,omitempty"`
	ClosedAt          *time.Time       `json:"closed_at,omitempty"`
}

func conversationDoc(tenantID, convID, customerID, channel string, now time.Time) doc {
	return doc{
		TenantID:       tenantID,
		ConversationID: convID,
		CustomerID:     customerID,
		Channel:        channel,
		Status:         "open",
		SearchText:     searchTextOf(channel, customerID),
		OpenedAt:       &now,
		UpdatedAt:      &now,
	}
}

func upsertDoc(tenantID, convID, customerID, channel string, last *LastInteraction, now time.Time) doc {
	d := conversationDoc(tenantID, convID, customerID, channel, now)
	d.LastInteraction = last
	d.LastInteractionAt = &now
	return d
}

func searchTextOf(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, strings.ToLower(p))
		}
	}
	return strings.Join(out, " ")
}

// painless scripts: increment-free touch vs. count-incrementing create.
// The fake OpenSearch in tests implements exactly these transforms.
const (
	scriptInteractionCreated = `
ctx._source.interaction_count = (ctx._source.interaction_count == null ? 1 : ctx._source.interaction_count + 1);
ctx._source.last_interaction = params.last_interaction;
ctx._source.last_interaction_at = params.at;
ctx._source.updated_at = params.at;
if (params.search_text != null && ctx._source.search_text != null && ctx._source.search_text.indexOf(params.search_text) == -1) {
  ctx._source.search_text = ctx._source.search_text + ' ' + params.search_text;
}
`
	scriptInteractionTouched = `
ctx._source.last_interaction = params.last_interaction;
ctx._source.last_interaction_at = params.at;
ctx._source.updated_at = params.at;
if (params.search_text != null && ctx._source.search_text != null && ctx._source.search_text.indexOf(params.search_text) == -1) {
  ctx._source.search_text = ctx._source.search_text + ' ' + params.search_text;
}
`
)

// ---- bounded memory helpers ----

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ringMap is a fixed-capacity string→string map with FIFO eviction (and a
// fixed-capacity membership set for envelope ids). Bounded memory: it can
// never exceed cap entries no matter the event rate.
type ringMap struct {
	mu    sync.Mutex
	cap   int
	m     map[string]string
	order []string
	next  int
}

func newRingMap(capacity int) *ringMap {
	if capacity <= 0 {
		capacity = 1
	}
	return &ringMap{
		cap:   capacity,
		m:     make(map[string]string, capacity),
		order: make([]string, capacity),
	}
}

// putIfAbsent records id and reports whether it was ALREADY present
// (true = duplicate).
func (r *ringMap) putIfAbsent(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.m[id]; ok {
		return true
	}
	r.evictSlot()
	r.m[id] = ""
	r.order[r.next] = id
	r.next = (r.next + 1) % r.cap
	return false
}

func (r *ringMap) put(key, val string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.m[key]; ok && old != "" {
		// keep the existing ring slot; just update the value
		r.m[key] = val
		return
	}
	r.evictSlot()
	r.m[key] = val
	r.order[r.next] = key
	r.next = (r.next + 1) % r.cap
}

func (r *ringMap) get(key string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.m[key]
	return v, ok
}

// evictSlot removes whatever currently occupies the next ring slot.
// Caller must hold r.mu.
func (r *ringMap) evictSlot() {
	if old := r.order[r.next]; old != "" {
		delete(r.m, old)
	}
}
