// Command worker runs Orvexa's asynchronous subsystems:
//   - transactional outbox dispatcher (at-least-once delivery to the bus)
//   - audit consumer (append-only audit_events from domain events)
//
// Consumer wiring for analytics/usage lands with their waves.
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

	"github.com/Roy-Wanyoike/orvexa/internal/platform/bus"
	"github.com/Roy-Wanyoike/orvexa/internal/platform/db"
	"github.com/Roy-Wanyoike/orvexa/internal/platform/outbox"
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
