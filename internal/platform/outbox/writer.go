// Package outbox implements the transactional outbox pattern.
//
// Writers insert domain events into outbox_events INSIDE the same transaction
// as their state change; the Dispatcher polls claimed batches and publishes to
// the Bus with retry + backoff. At-least-once delivery with per-consumer
// deduplication by event id.
package outbox

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
	"github.com/Roy-Wanyoike/orvexa/pkg/events"
)

// Writer persists envelopes into the outbox within a caller's transaction.
type Writer struct {
	pool *pgxpool.Pool
}

func NewWriter(pool *pgxpool.Pool) *Writer { return &Writer{pool: pool} }

// Insert writes one envelope inside the given transaction. This is the ONLY
// sanctioned path for domain events — it keeps DB state and events atomic.
func (w *Writer) Insert(ctx context.Context, tx pgx.Tx, env *events.Envelope) error {
	if err := env.Validate(); err != nil {
		return apperrors.Invalid("event.invalid_envelope", err.Error())
	}
	payload, err := json.Marshal(env.Data)
	if err != nil {
		return apperrors.Internal("event.marshal_failed", "event serialization failed").WithCause(err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO outbox_events (id, topic, source, subject, tenant_id, correlation_id, payload, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		env.ID, env.Type, env.Source, env.Subject, env.TenantID, env.CorrelationID, payload, env.Time)
	if err != nil {
		return apperrors.Internal("outbox.insert_failed", "event persistence failed").WithCause(err)
	}
	return nil
}

// InsertInTx is a package-level helper matching the Outbox port used by
// domain services that hold a Writer.
func (w *Writer) PublishLater(ctx context.Context, env *events.Envelope) error {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return apperrors.Internal("db.tx_failed", "write failed").WithCause(err)
	}
	defer tx.Rollback(context.Background())
	if err := w.Insert(ctx, tx, env); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Row is a claimed outbox record handed to the bus.
type Row struct {
	ID            string
	Topic         string
	Source        string
	Subject       string
	TenantID      string
	CorrelationID string
	Payload       json.RawMessage
	OccurredAt    time.Time
	Attempts      int
}

// ClaimedBatch is a batch leased by the dispatcher with its visibility token.
type ClaimedBatch struct {
	Token string
	Rows  []Row
}
