// Provider-events ledger consumer (issue #103).
//
// The webhook gateway is deliberately ledger-persistence-only: every accepted
// provider delivery lands in provider_events and nothing else (single effect,
// replay-safe). The PUBLIC HTTP path applies the event to the domain right
// after ingest (internal/httpserver webhookHandler), but the INTERNAL path —
// the comms.IngestFunc port the built-in simulator (and any in-process
// adapter) delivers through — only ever persisted. Before #103 those rows
// were written and never read: outbound calls on the default carrier stayed
// `pending` forever and the ledger grew unbounded.
//
// EventConsumer closes that gap in the worker: it drains unprocessed ledger
// rows and applies them through the SAME comms.Processor the HTTP webhook
// path uses, then marks the row processed via the gateway's MarkProcessed
// (keyed on the derived provider_event_id the gateway wrote). One effect per
// event, whichever path delivers it.
package comms

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// LedgerMarker is the completion port into the provider_events ledger.
// *webhooks.Gateway implements it: MarkProcessed keys on the derived
// provider_event_id — the same key persistEvent wrote — so this package does
// not need to depend on the webhook delivery mechanics (HMAC, verifiers).
type LedgerMarker interface {
	MarkProcessed(ctx context.Context, eventID string)
}

// Consumer tuning. Zero values select the documented defaults.
type EventConsumerConfig struct {
	// Interval between drain ticks (default 5s).
	Interval time.Duration
	// BatchSize caps how many unprocessed rows one tick drains (default 100).
	BatchSize int
	// MaxAttempts is how many in-process processing attempts a failing row
	// gets before it is skipped (default 10). See the poison-row note on
	// EventConsumer.
	MaxAttempts int
}

const (
	defaultInterval    = 5 * time.Second
	defaultBatchSize   = 100
	defaultMaxAttempts = 10
)

// EventConsumer drains the provider_events ledger into the interaction
// domain.
//
// Semantics (deliberate, documented):
//
//   - At-least-once: a crash between Process and MarkProcessed re-applies the
//     row on the next tick. That is safe because Processor.Process is
//     idempotent for same-state delivery (its documented contract) and
//     MarkProcessed is a no-op once processed_at is set. Rows are not leased;
//     two workers racing on one row both apply, and same-state idempotency
//     keeps it single-effect.
//
//   - Poison rows: a row that fails (malformed payload, unknown event, illegal
//     transition, interaction not found yet) stays unprocessed and is retried
//     on later ticks. After MaxAttempts in-process failures the row is
//     SKIPPED via an in-memory attempts counter so it cannot wedge the drain.
//     Honest limitations: the counter lives in this process only — it resets
//     on worker restart (the row gets fresh attempts), and the row itself is
//     never mutated or deleted (it remains unprocessed in the ledger for
//     operator inspection; a persistent attempts/parked column would be a
//     schema change the issue explicitly rules out — that belongs to the
//     ledger reaper already tracked in the infra wave). Because the drain is
//     FIFO with a bounded batch, more than BatchSize consecutive poison rows
//     at the head delay fresh rows behind them until restart.
//
//   - Ordering: rows drain FIFO by received_at (ties broken by id). This
//     serves the filter AND the order from the partial index
//     idx_provider_events_unprocessed (received_at) WHERE processed_at IS
//     NULL; ordering by the random row id would sort meaninglessly and
//     bypass the index.
type EventConsumer struct {
	pool      *pgxpool.Pool
	processor *Processor
	marker    LedgerMarker
	log       *slog.Logger

	interval    time.Duration
	batchSize   int
	maxAttempts int

	mu       sync.Mutex
	attempts map[string]int // provider\x00provider_event_id → failed attempts
}

// NewEventConsumer builds the ledger drain. processor is the shared webhook
// processor; marker is normally *webhooks.Gateway. A nil log degrades to the
// default slog logger (the worker always passes its own).
func NewEventConsumer(pool *pgxpool.Pool, processor *Processor, marker LedgerMarker, cfg EventConsumerConfig, log *slog.Logger) *EventConsumer {
	if cfg.Interval <= 0 {
		cfg.Interval = defaultInterval
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = defaultBatchSize
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = defaultMaxAttempts
	}
	if log == nil {
		log = slog.Default()
	}
	return &EventConsumer{
		pool:        pool,
		processor:   processor,
		marker:      marker,
		log:         log,
		interval:    cfg.Interval,
		batchSize:   cfg.BatchSize,
		maxAttempts: cfg.MaxAttempts,
		attempts:    map[string]int{},
	}
}

// Run drains the ledger on the configured interval until ctx is canceled
// (same lifecycle shape as the outbox dispatcher: errors are logged, the
// loop survives).
func (c *EventConsumer) Run(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n, err := c.Tick(ctx); err != nil {
				c.log.Warn("provider_events tick failed", "err", err)
			} else if n > 0 {
				c.log.Info("provider events applied", "count", n)
			}
		}
	}
}

// Tick drains at most batchSize unprocessed ledger rows: decode →
// Processor.Process → MarkProcessed. Returns the number of rows applied.
// Exported for tests and for operators who want to force a drain.
func (c *EventConsumer) Tick(ctx context.Context) (int, error) {
	rows, err := c.pool.Query(ctx, `
		SELECT provider, provider_event_id, payload
		FROM provider_events
		WHERE processed_at IS NULL
		ORDER BY received_at, id
		LIMIT $1`, c.batchSize)
	if err != nil {
		return 0, apperrors.Internal("provider_events.read_failed", "ledger drain failed").WithCause(err)
	}
	defer rows.Close()

	applied := 0
	for rows.Next() {
		var provider, eventID string
		var payload []byte
		if err := rows.Scan(&provider, &eventID, &payload); err != nil {
			return applied, apperrors.Internal("db.scan_failed", "ledger drain failed").WithCause(err)
		}
		if c.skipped(provider, eventID) {
			continue
		}
		if err := c.apply(ctx, provider, eventID, payload); err != nil {
			c.recordFailure(provider, eventID, err)
			continue
		}
		c.marker.MarkProcessed(ctx, eventID)
		applied++
	}
	if err := rows.Err(); err != nil {
		return applied, apperrors.Internal("provider_events.read_failed", "ledger drain failed").WithCause(err)
	}
	return applied, nil
}

// Backlog reports how many ledger rows are still unprocessed (readiness and
// backlog warnings; the same health signal the outbox dispatcher exposes).
func (c *EventConsumer) Backlog(ctx context.Context) (int64, error) {
	var n int64
	if err := c.pool.QueryRow(ctx,
		`SELECT count(*) FROM provider_events WHERE processed_at IS NULL`).Scan(&n); err != nil {
		return 0, apperrors.Internal("db.read_failed", "backlog read failed").WithCause(err)
	}
	return n, nil
}

// apply decodes one payload and pushes it through the webhook processor —
// the exact code path the HTTP webhook handler runs after Ingest.
func (c *EventConsumer) apply(ctx context.Context, provider, eventID string, payload []byte) error {
	var ev ProviderEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return apperrors.Invalid("comms.malformed_event",
			"provider event payload is not a ProviderEvent").WithCause(err)
	}
	if err := c.processor.Process(ctx, &ev); err != nil {
		return err
	}
	c.log.Debug("provider event applied", "provider", provider, "event_id", eventID,
		"event", ev.Event, "interaction_id", ev.InteractionID)
	return nil
}

// recordFailure counts a failed processing attempt. Every failure is logged
// with the attempt number; the final allowed attempt logs the poison
// sentence so operators see the row was skipped, not silently dropped.
func (c *EventConsumer) recordFailure(provider, eventID string, err error) {
	key := provider + "\x00" + eventID
	c.mu.Lock()
	c.attempts[key]++
	attempt := c.attempts[key]
	c.mu.Unlock()

	if attempt >= c.maxAttempts {
		c.log.Warn("provider event skipped as poison",
			"provider", provider, "event_id", eventID, "attempt", attempt,
			"max_attempts", c.maxAttempts,
			"note", "left unprocessed in ledger for inspection; in-process counter resets on restart",
			"err", err)
		return
	}
	c.log.Warn("provider event processing failed (will retry next tick)",
		"provider", provider, "event_id", eventID, "attempt", attempt,
		"max_attempts", c.maxAttempts, "err", err)
}

// skipped reports whether a row exhausted its in-process attempts.
func (c *EventConsumer) skipped(provider, eventID string) bool {
	key := provider + "\x00" + eventID
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attempts[key] >= c.maxAttempts
}
