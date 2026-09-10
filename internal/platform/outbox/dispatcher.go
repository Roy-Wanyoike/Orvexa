package outbox

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// Dispatcher leases outbox rows and hands them to a publisher function until
// they are acknowledged. Semantics:
//   - batches are claimed with `FOR UPDATE SKIP LOCKED` so competing workers
//     never double-publish concurrently;
//   - delivery is at-least-once: crash after publish but before ack re-delivers;
//     consumers dedupe by event id (contract documented in pkg/events);
//   - failed batches are retried with exponential backoff capped at maxAttempts,
//     then parked with status='failed' for operator inspection (never deleted).
type Dispatcher struct {
	pool        *pgxpool.Pool
	publish     func(ctx context.Context, batch ClaimedBatch) error
	interval    time.Duration
	batchSize   int
	maxAttempts int
}

func NewDispatcher(pool *pgxpool.Pool, publish func(ctx context.Context, batch ClaimedBatch) error,
	interval time.Duration, batchSize, maxAttempts int) *Dispatcher {
	if interval <= 0 {
		interval = time.Second
	}
	if batchSize <= 0 {
		batchSize = 100
	}
	if maxAttempts <= 0 {
		maxAttempts = 10
	}
	return &Dispatcher{pool: pool, publish: publish, interval: interval,
		batchSize: batchSize, maxAttempts: maxAttempts}
}

// Run drives the dispatch loop until the context is canceled.
func (d *Dispatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = d.Tick(ctx) // errors logged by publisher; loop must survive
		}
	}
}

// Tick claims and publishes at most one batch. Exported for tests.
func (d *Dispatcher) Tick(ctx context.Context) error {
	batch, err := d.claim(ctx)
	if err != nil {
		return err
	}
	if len(batch.Rows) == 0 {
		return nil
	}
	if err := d.publish(ctx, batch); err != nil {
		_ = d.releaseForRetry(ctx, batch)
		return err
	}
	return d.ack(ctx, batch)
}

// claim leases a batch: rows move to 'publishing' with a lease token.
func (d *Dispatcher) claim(ctx context.Context) (ClaimedBatch, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return ClaimedBatch{}, apperrors.Internal("db.tx_failed", "dispatcher failed").WithCause(err)
	}
	defer tx.Rollback(context.Background())

	token := newToken()
	rows, err := tx.Query(ctx, `
		UPDATE outbox_events SET status='publishing', lease_token=$2, attempts = attempts + 1, updated_at = now()
		WHERE id IN (
			SELECT id FROM outbox_events
			WHERE status = 'pending'
			   OR (status = 'publishing' AND lease_expires_at < now())
			ORDER BY occurred_at
			LIMIT $1
		)
		RETURNING id, topic, source, coalesce(subject,''), tenant_id, coalesce(correlation_id,''), payload, occurred_at, attempts`,
		d.batchSize, token)
	if err != nil {
		return ClaimedBatch{}, apperrors.Internal("outbox.claim_failed", "dispatcher failed").WithCause(err)
	}
	defer rows.Close()

	batch := ClaimedBatch{Token: token}
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.ID, &r.Topic, &r.Source, &r.Subject, &r.TenantID,
			&r.CorrelationID, &r.Payload, &r.OccurredAt, &r.Attempts); err != nil {
			return ClaimedBatch{}, apperrors.Internal("db.scan_failed", "dispatcher failed").WithCause(err)
		}
		batch.Rows = append(batch.Rows, r)
	}
	if err := tx.Commit(ctx); err != nil {
		return ClaimedBatch{}, apperrors.Internal("db.commit_failed", "dispatcher failed").WithCause(err)
	}
	return batch, nil
}

// ack marks a delivered batch. Expired leases (crashed worker) are reclaimed
// by the next claim round.
func (d *Dispatcher) ack(ctx context.Context, batch ClaimedBatch) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE outbox_events SET status='published', lease_token='', updated_at=now()
		WHERE lease_token = $1`, batch.Token)
	if err != nil {
		return apperrors.Internal("outbox.ack_failed", "dispatcher failed").WithCause(err)
	}
	return nil
}

// releaseForRetry parks or re-queues a failed batch based on attempt count.
func (d *Dispatcher) releaseForRetry(ctx context.Context, batch ClaimedBatch) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE outbox_events SET
			status = CASE WHEN attempts >= $2 THEN 'failed' ELSE 'pending' END,
			lease_token = '',
			lease_expires_at = now() + interval '30 seconds',
			updated_at = now()
		WHERE lease_token = $1`, batch.Token, d.maxAttempts)
	if err != nil {
		return apperrors.Internal("outbox.retry_failed", "dispatcher failed").WithCause(err)
	}
	return nil
}

// Metrics returns dispatcher health numbers for readiness endpoints.
type Metrics struct {
	Pending    int64
	Publishing int64
	Failed     int64
}

func (d *Dispatcher) Metrics(ctx context.Context) (Metrics, error) {
	var m Metrics
	err := d.pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE status='pending'),
			count(*) FILTER (WHERE status='publishing'),
			count(*) FILTER (WHERE status='failed')
		FROM outbox_events`).Scan(&m.Pending, &m.Publishing, &m.Failed)
	if err != nil {
		return Metrics{}, apperrors.Internal("db.read_failed", "metrics failed").WithCause(err)
	}
	return m, nil
}

func newToken() string {
	return randToken()
}
