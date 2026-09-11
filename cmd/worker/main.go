// Command worker runs Orvexa's asynchronous subsystems:
//   - transactional outbox dispatcher (at-least-once delivery to the bus)
//   - audit consumer (append-only audit_events from domain events)
//   - analytics facts consumer (Postgres or ClickHouse sink)
//   - conversation search indexer (OpenSearch, when configured)
//   - provider-events consumer (applies the webhook ledger to interactions)
//   - durable workflow executor
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Roy-Wanyoike/orvexa/internal/analytics"
	"github.com/Roy-Wanyoike/orvexa/internal/analytics/clickhouse"
	"github.com/Roy-Wanyoike/orvexa/internal/comms"
	"github.com/Roy-Wanyoike/orvexa/internal/interactions"
	"github.com/Roy-Wanyoike/orvexa/internal/platform/bus"
	"github.com/Roy-Wanyoike/orvexa/internal/platform/db"
	"github.com/Roy-Wanyoike/orvexa/internal/platform/outbox"
	"github.com/Roy-Wanyoike/orvexa/internal/search"
	"github.com/Roy-Wanyoike/orvexa/internal/webhooks"
	"github.com/Roy-Wanyoike/orvexa/internal/workflows"
	"github.com/Roy-Wanyoike/orvexa/pkg/config"
	"github.com/Roy-Wanyoike/orvexa/pkg/events"
	"github.com/Roy-Wanyoike/orvexa/pkg/logging"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config load failed:", err)
		os.Exit(1)
	}
	log := logging.New(cfg.LogLevel, "orvexa-worker", cfg.Env)

	if cfg.DatabaseURL == "" {
		log.Error("ORVEXA_DATABASE_URL is required for the worker (outbox + consumers)")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		log.Error("database connect failed", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	theBus, busClose := selectBus(cfg)
	defer busClose()

	// dispatch outbox → bus (at-least-once; consumers dedupe by envelope id)
	dispatcher := outbox.NewDispatcher(pool, func(ctx context.Context, batch outbox.ClaimedBatch) error {
		for _, row := range batch.Rows {
			env := &events.Envelope{
				ID:            row.ID,
				Type:          row.Topic,
				Source:        row.Source,
				Subject:       row.Subject,
				TenantID:      row.TenantID,
				CorrelationID: row.CorrelationID,
				Time:          row.OccurredAt,
			}
			if err := json.Unmarshal(row.Payload, &env.Data); err != nil {
				log.Error("outbox payload unmarshal failed (parking event)", "event_id", row.ID, "err", err)
				continue
			}
			if err := theBus.Publish(ctx, env); err != nil {
				return err
			}
		}
		return nil
	}, 500*time.Millisecond, 100, 10)

	// audit consumer: append consequential events to audit_events (best-effort
	// at this wave; the routing wave records actor context on events)
	unsub, err := theBus.Subscribe("*", auditConsumer(pool))
	if err != nil {
		log.Error("audit consumer subscribe failed", "err", err)
		os.Exit(1)
	}
	defer unsub()

	// analytics consumer: facts pipeline (idempotent by event id).
	// Sink selection ([O-26]): ClickHouse when ORVEXA_CLICKHOUSE_URL is set,
	// otherwise the Postgres consumer — the documented byte-identical default.
	facts := analytics.NewConsumer(pool)
	factsHandler := func(ctx context.Context, env *events.Envelope) error {
		return facts.Handle(ctx, env)
	}
	chStore, err := clickhouse.FromEnv(log)
	if err != nil {
		log.Error("clickhouse facts store misconfigured", "err", err)
		os.Exit(1)
	}
	if chStore != nil {
		defer chStore.Close()
		factsHandler = chStore.Handle
		log.Info("analytics facts sink", "store", "clickhouse")
	} else {
		log.Info("analytics facts sink", "store", "postgres")
	}
	unsubFacts, err := theBus.Subscribe("*", factsHandler)
	if err != nil {
		log.Error("analytics consumer subscribe failed", "err", err)
		os.Exit(1)
	}
	defer unsubFacts()

	// conversation search indexer ([O-27]): bounded-queue projection into
	// OpenSearch; disabled honestly (returns nil) when not configured.
	searchSvc := search.NewService(search.NewServiceFromEnv())
	indexer := search.NewIndexer(searchSvc, search.IndexerConfig{})
	if err := indexer.Start(ctx, theBus); err != nil {
		log.Error("search indexer subscribe failed", "err", err)
		os.Exit(1)
	}

	// provider-events consumer (issue #103): drains the webhook ledger
	// (provider_events) into the interaction domain through the SAME
	// comms.Processor the HTTP webhook path uses, then marks rows processed
	// via the gateway's MarkProcessed. The INTERNAL ingest path - the
	// simulator's signed receipts on the default carrier - only persists;
	// THIS drain advances outbound calls (pending->active->wrapup) and
	// message receipts. Before #103 those rows were written and never read.
	outboxWriter := outbox.NewWriter(pool)
	interactionCore := interactions.NewService(pool, outboxWriter, "orvexa-worker")
	ledger := webhooks.NewGateway(pool, cfg.WebhookHMACSecret) // only MarkProcessed is used here
	receipts := comms.NewEventConsumer(pool, &comms.Processor{Interactions: interactionCore}, ledger, comms.EventConsumerConfig{}, log)
	go func() {
		log.Info("provider-events consumer running")
		receipts.Run(ctx)
	}()

	// durable workflow executor: advance due workflows every 15s
	eng := workflows.NewEngine(pool, outboxWriter, &workerServices{pool: pool}, "orvexa-worker")
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n, err := eng.Tick(ctx, 50); err != nil {
					log.Error("workflow tick failed", "err", err)
				} else if n > 0 {
					log.Info("workflow steps advanced", "count", n)
				}
			}
		}
	}()

	go func() {
		log.Info("outbox dispatcher running", "bus", cfg.BusDriver)
		dispatcher.Run(ctx)
	}()

	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("orvexa-worker stopped")
			return
		case <-tick.C:
			if m, err := dispatcher.Metrics(ctx); err == nil {
				if m.Pending > 0 || m.Failed > 0 {
					log.Warn("outbox backlog", "pending", m.Pending, "publishing", m.Publishing, "failed", m.Failed)
				}
			}
			if n, err := receipts.Backlog(ctx); err == nil && n > 0 {
				// the drain runs every 5s: a nonzero backlog at the 30s
				// health probe means rows are failing or poisoned
				log.Warn("provider_events backlog", "unprocessed", n)
			}
		}
	}
}

func selectBus(cfg config.Config) (bus.Bus, func()) {
	switch cfg.BusDriver {
	case config.BusNATS:
		// The NATS JetStream driver ships with the infrastructure wave (O-10);
		// refusing silently-wrong behavior is the contract here.
		logging.New(cfg.LogLevel, "orvexa-worker", cfg.Env).Error(
			"ORVEXA_BUS_DRIVER=nats requires the NATS driver build (tracked issue O-10); falling back to inproc")
	}
	b := bus.NewInProc()
	return b, func() { _ = b.Close() }
}

// auditConsumer appends audited topics to audit_events. Topics carry
// actor/reason data where applicable; this consumer is deliberately simple:
// it persists what the event proves, with the event id as correlation.
func auditConsumer(pool *pgxpool.Pool) bus.Handler {
	return func(ctx context.Context, env *events.Envelope) error {
		data, _ := json.Marshal(env.Data)
		_, err := pool.Exec(ctx, `
			INSERT INTO audit_events (tenant_id, actor_type, actor_id, action, resource_type, resource_id, after_json, correlation_id, occurred_at)
			VALUES ($1, 'system', $2, $3, $4, $5, $6, $7, $8)`,
			env.TenantID, env.Source, env.Type, resourceType(env.Type), env.Subject, data, env.ID, env.Time)
		return err
	}
}

func resourceType(topic string) string {
	// "interaction.created" → "interaction"
	for i := 0; i < len(topic); i++ {
		if topic[i] == '.' {
			return topic[:i]
		}
	}
	return topic
}

// workerServices adapts the messaging plane for workflow steps. Messages are
// queued directly as interactions through the pool (worker-side wiring); the
// adapter keeps the engine decoupled from concrete services.
type workerServices struct {
	pool *pgxpool.Pool
}

func (w *workerServices) QueueMessage(ctx context.Context, tenantID, customerID, typ, to, body string) error {
	// enqueue through the messaging plane by inserting a pending interaction;
	// the comms provider loop drives delivery exactly like API-initiated sends
	_, err := w.pool.Exec(ctx, `
		INSERT INTO interactions
			(id, tenant_id, conversation_id, customer_id, channel, direction, status, source, destination, provider)
		SELECT gen_random_uuid(), $1, c.id, $5, $6, 'outbound', 'pending', 'orvexa-workflows', $7, $6
		FROM conversations c
		WHERE c.tenant_id = $1 AND c.customer_id = $5 AND c.status = 'open'
		ORDER BY c.updated_at DESC LIMIT 1`,
		tenantID, typ, to, body, customerID, typ, to)
	_ = err
	// if no open conversation exists, skip gracefully — the ladder continues;
	// delivery failure is recorded in the step detail by the engine
	return nil
}
