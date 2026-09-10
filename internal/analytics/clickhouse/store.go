// Package clickhouse provides the OLAP facts store for the analytics pipeline
// ([O-26], issue #35): a batched, non-blocking writer that implements the same
// facts interface as the Postgres-backed analytics.Consumer but appends into
// ClickHouse so large aggregates never touch the transactional store.
//
// Selection is env-gated: ORVEXA_CLICKHOUSE_URL unset → FromEnv returns
// (nil, nil) and callers keep their existing facts path byte-identical.
//
// Delivery contract: Handle performs no network I/O — rows are placed on a
// bounded in-process buffer and written by a single background flusher that
// fires when the buffer reaches MaxBatchRows, on a FlushInterval tick, or on
// Close. When the buffer is full (ClickHouse down or slower than the event
// rate) Handle applies backpressure: it blocks up to EnqueueTimeout and then
// fails with a typed error. Memory is hard-capped by the buffer sizes — the
// store never grows unboundedly under backpressure.
package clickhouse

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
	"github.com/Roy-Wanyoike/orvexa/pkg/events"
)

// FactsStore is the write surface of the analytics facts pipeline — exactly
// the Handle contract the Postgres-backed analytics.Consumer exposes to the
// bus. *Store implements it with batched ClickHouse inserts; the Postgres
// consumer implements it with per-event INSERTs, so the worker can swap
// backends on ORVEXA_CLICKHOUSE_URL without changing the bus wiring shape.
// (analytics.Consumer satisfies this interface as-is; asserted in tests.)
type FactsStore interface {
	Handle(ctx context.Context, env *events.Envelope) error
}

// Compile-time proof that the store implements the facts interface.
var _ FactsStore = (*Store)(nil)

// batchConn is the narrow slice of the ClickHouse driver surface the store
// depends on (driver.Conn is a superset: it has PrepareBatch and Close). The
// store is designed against this interface so unit tests can mock the client
// without a server — emulating the ClickHouse wire protocol over httptest is
// not feasible for the native protocol.
type batchConn interface {
	PrepareBatch(ctx context.Context, query string, opts ...driver.PrepareBatchOption) (driver.Batch, error)
	Close() error
}

// Parameterized INSERT statements. Query text is fixed at compile time —
// envelope values are bound via driver batch Append, never string-built.
const (
	insertInteraction = `INSERT INTO interaction_fact (event_id, tenant_id, interaction_id, channel, direction, status, occurred_at) VALUES (?, ?, ?, ?, ?, ?, ?)`
	insertUsage       = `INSERT INTO usage_fact (event_id, tenant_id, metric, amount, occurred_at) VALUES (?, ?, ?, ?, ?)`
)

// Defaults keep the store usable with a bare URL. Tunable via Options or the
// ORVEXA_CLICKHOUSE_* environment variables (see env.go).
const (
	DefaultMaxBatchRows   = 1000            // flush trigger: buffered rows across both fact tables
	DefaultFlushInterval  = 2 * time.Second // background flush cadence
	DefaultWriteTimeout   = 5 * time.Second // per-flush context timeout
	DefaultEnqueueTimeout = 5 * time.Second // how long Handle may block when the buffer is full
)

// Options configures New. The zero-value knobs fall back to the defaults
// above; URL is required.
type Options struct {
	URL            string
	MaxBatchRows   int           // flush when this many buffered rows accumulate
	FlushInterval  time.Duration // background flush cadence
	WriteTimeout   time.Duration // per-flush context timeout
	EnqueueTimeout time.Duration // how long Handle may block when the buffer is full
}

func (o *Options) applyDefaults() {
	if o.MaxBatchRows <= 0 {
		o.MaxBatchRows = DefaultMaxBatchRows
	}
	if o.FlushInterval <= 0 {
		o.FlushInterval = DefaultFlushInterval
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = DefaultWriteTimeout
	}
	if o.EnqueueTimeout <= 0 {
		o.EnqueueTimeout = DefaultEnqueueTimeout
	}
}

// interactionRow mirrors the interaction_fact columns of
// migrations/0010_workflows_analytics.sql (and the ClickHouse DDL in
// migrations/0011_clickhouse_facts.sql); extraction semantics are identical
// to the Postgres consumer in internal/analytics/service.go.
type interactionRow struct {
	eventID       uuid.UUID
	tenantID      uuid.UUID
	interactionID uuid.UUID
	channel       string
	direction     string
	status        string
	occurredAt    time.Time
}

func (r interactionRow) values() []any {
	return []any{r.eventID, r.tenantID, r.interactionID, r.channel, r.direction, r.status, r.occurredAt}
}

// usageRow mirrors the usage_fact columns of the 0010/0011 schemas.
type usageRow struct {
	eventID    uuid.UUID
	tenantID   uuid.UUID
	metric     string
	amount     int64
	occurredAt time.Time
}

func (r usageRow) values() []any {
	return []any{r.eventID, r.tenantID, r.metric, r.amount, r.occurredAt}
}

// Store is the batched ClickHouse facts writer. It is safe for concurrent
// use; Handle may be called from any goroutine (the inproc bus dispatches
// each envelope on its own goroutine).
//
// Buffering model: rows live on two bounded channels (one per fact table,
// each holding MaxBatchRows rows — memory is capped at 2×MaxBatchRows rows
// plus the flusher's in-flight batch). The single flusher goroutine owns all
// batching and network I/O; it is the only receiver of the channels and the
// only user of the driver connection, so flushes are strictly serialized and
// need no locks on the hot path.
type Store struct {
	maxRows        int           // flush trigger: buffered rows (both tables combined)
	flushEvery     time.Duration // background flush cadence
	writeTimeout   time.Duration // per-flush context timeout
	enqueueTimeout time.Duration // Handle block-with-timeout budget when the buffer is full
	log            *slog.Logger

	dialOpts *clickhouse.Options // parsed from Options.URL
	newConn  func(*clickhouse.Options) (batchConn, error)

	interCh chan interactionRow // bounded buffer, cap = maxRows
	usageCh chan usageRow       // bounded buffer, cap = maxRows

	mu     sync.Mutex // guards closed only; the flusher owns everything else
	closed bool

	wg       sync.WaitGroup // in-flight Handle calls (blocked ones included)
	stopping chan struct{}  // closed by Close to release producers blocked on a full buffer
	stop     chan struct{}  // closed by Close (after producers drain) to stop the flusher
	done     chan struct{}  // closed by the flusher on exit

	// flusher-goroutine-owned state (no lock: single accessor).
	conn      batchConn // lazily dialed on first flush
	finalErr  error     // recorded by the flusher shutdown; read by Close after done
	closeOnce sync.Once
}

// New builds a store from explicit options (tests and non-env callers).
// Configuration is validated here (including DSN parsing) but no network
// connection is made — the driver dials lazily on the first flush. The
// flusher goroutine starts immediately; Close must be called exactly once.
func New(opts Options, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if opts.URL == "" {
		return nil, apperrors.Invalid("analytics.clickhouse_url_missing", "ORVEXA_CLICKHOUSE_URL is required")
	}
	dsn, err := clickhouse.ParseDSN(opts.URL)
	if err != nil {
		// ParseDSN errors describe the malformed field, never echo the DSN,
		// so no credential material can leak through the wrapped cause.
		return nil, apperrors.Invalid("analytics.clickhouse_url_invalid", "ORVEXA_CLICKHOUSE_URL is not a valid ClickHouse DSN").WithCause(err)
	}
	opts.applyDefaults()

	// Keep the driver's dial bounded by the same budget as the flush: a
	// dead server must surface as a failed flush within WriteTimeout-ish
	// time, not hang a background goroutine for the 30s driver default.
	if dsn.DialTimeout <= 0 || dsn.DialTimeout > opts.WriteTimeout {
		dsn.DialTimeout = opts.WriteTimeout
	}

	s := &Store{
		maxRows:        opts.MaxBatchRows,
		flushEvery:     opts.FlushInterval,
		writeTimeout:   opts.WriteTimeout,
		enqueueTimeout: opts.EnqueueTimeout,
		log:            log,
		dialOpts:       dsn,
		newConn: func(o *clickhouse.Options) (batchConn, error) {
			return clickhouse.Open(o)
		},
		interCh:  make(chan interactionRow, opts.MaxBatchRows),
		usageCh:  make(chan usageRow, opts.MaxBatchRows),
		stopping: make(chan struct{}),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go s.flushLoop()
	return s, nil
}

// Handle processes one envelope into the matching fact buffer. Topic routing
// and field extraction are identical to the Postgres consumer
// (internal/analytics.Consumer.Handle); non-fact topics and interaction
// events without a subject are ignored, like the Postgres path.
//
// Handle never performs network I/O. When buffer space is available the row
// is accepted immediately (even with a cancelled ctx — a cancelled caller
// context never stalls or drops delivery on its own). When the buffer is
// full, Handle blocks up to EnqueueTimeout (backpressure) and then fails with
// the typed error analytics.clickhouse_backpressure; a cancelled ctx or a
// Close-in-progress unblocks it sooner with the corresponding error. The
// caller (bus consumer) treats the error like any other consume failure —
// delivery is at-least-once upstream.
func (s *Store) Handle(ctx context.Context, env *events.Envelope) error {
	if env == nil {
		return nil
	}
	switch env.Type {
	case events.TopicInteractionCreated, events.TopicInteractionUpdated, events.TopicInteractionCompleted:
		subject := env.Subject
		// interaction.updated events carry the id inside data
		if subject == "" {
			if v, ok := env.Data["interaction_id"].(string); ok {
				subject = v
			}
		}
		if subject == "" {
			return nil
		}
		row, err := newInteractionRow(env, subject)
		if err != nil {
			return err
		}
		return s.enqueueInteraction(ctx, row)
	case events.TopicUsageRecorded:
		row, err := newUsageRow(env)
		if err != nil {
			return err
		}
		return s.enqueueUsage(ctx, row)
	default:
		return nil // not a fact topic
	}
}

// enqueueInteraction places a row on the bounded interaction buffer; see
// Handle for the backpressure contract.
func (s *Store) enqueueInteraction(ctx context.Context, row interactionRow) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errStoreClosed()
	}
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()

	select {
	case s.interCh <- row:
		return nil
	default:
	}
	// Buffer full: block with a timeout, then fail with a typed error. Never
	// grow memory without bound, never block forever.
	timer := time.NewTimer(s.enqueueTimeout)
	defer timer.Stop()
	select {
	case s.interCh <- row:
		return nil
	case <-ctx.Done():
		return writeFailed(ctx.Err())
	case <-s.stopping:
		return errStoreClosed()
	case <-timer.C:
		return errBackpressure()
	}
}

// enqueueUsage places a row on the bounded usage buffer; see Handle for the
// backpressure contract.
func (s *Store) enqueueUsage(ctx context.Context, row usageRow) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errStoreClosed()
	}
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()

	select {
	case s.usageCh <- row:
		return nil
	default:
	}
	timer := time.NewTimer(s.enqueueTimeout)
	defer timer.Stop()
	select {
	case s.usageCh <- row:
		return nil
	case <-ctx.Done():
		return writeFailed(ctx.Err())
	case <-s.stopping:
		return errStoreClosed()
	case <-timer.C:
		return errBackpressure()
	}
}

func newInteractionRow(env *events.Envelope, subject string) (interactionRow, error) {
	// Error surface parity with the Postgres consumer: malformed ids fail the
	// INSERT there; here they fail uuid parsing with the same apperror code.
	interactionID, err := uuid.Parse(subject)
	if err != nil {
		return interactionRow{}, writeFailed(err)
	}
	tenantID, err := uuid.Parse(env.TenantID)
	if err != nil {
		return interactionRow{}, writeFailed(err)
	}
	eventID, err := uuid.Parse(env.ID)
	if err != nil {
		return interactionRow{}, writeFailed(err)
	}
	status := dataString(env.Data, "status")
	if status == "" {
		status = "unknown"
	}
	// The Postgres path persists whatever timestamp the envelope carries; a
	// zero time would exceed ClickHouse's DateTime64 range, so normalize like
	// events.Envelope.Validate does (validated envelopes never hit this).
	occurredAt := env.Time
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	return interactionRow{
		eventID:       eventID,
		tenantID:      tenantID,
		interactionID: interactionID,
		channel:       dataString(env.Data, "channel"),
		direction:     dataString(env.Data, "direction"),
		status:        status,
		occurredAt:    occurredAt.UTC(),
	}, nil
}

func newUsageRow(env *events.Envelope) (usageRow, error) {
	tenantID, err := uuid.Parse(env.TenantID)
	if err != nil {
		return usageRow{}, writeFailed(err)
	}
	eventID, err := uuid.Parse(env.ID)
	if err != nil {
		return usageRow{}, writeFailed(err)
	}
	var amount int64
	switch v := env.Data["output_tokens"].(type) {
	case float64:
		amount = int64(v)
	case int:
		amount = int64(v)
	}
	occurredAt := env.Time
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	return usageRow{
		eventID:    eventID,
		tenantID:   tenantID,
		metric:     dataString(env.Data, "metric"),
		amount:     amount,
		occurredAt: occurredAt.UTC(),
	}, nil
}

func dataString(data map[string]any, key string) string {
	v, _ := data[key].(string)
	return v
}

// writeFailed matches the Postgres consumer's error code and message so
// operators see one stable signal for fact-append failures.
func writeFailed(cause error) error {
	return apperrors.Internal("analytics.write_failed", "fact append failed").WithCause(cause)
}

// errStoreClosed is returned to Handles that race a Close.
func errStoreClosed() error {
	return apperrors.Invalid("analytics.clickhouse_store_closed", "facts store is closed")
}

// errBackpressure is the typed error returned when the bounded buffer stays
// full for the whole EnqueueTimeout budget.
func errBackpressure() error {
	return apperrors.RateLimited("analytics.clickhouse_backpressure", "facts buffer is full; slow down or retry after backoff")
}

// buffered reports the total number of rows waiting on the bounded buffers.
// Channel len() reads are concurrency-safe; used by tests for backpressure
// assertions.
func (s *Store) buffered() int {
	return len(s.interCh) + len(s.usageCh)
}

// flushLoop is the single flusher goroutine: it owns batching, the driver
// connection, and the final shutdown flush.
func (s *Store) flushLoop() {
	defer close(s.done)
	ticker := time.NewTicker(s.flushEvery)
	defer ticker.Stop()

	var inter []interactionRow // flusher-local batch stash (never locked)
	var usage []usageRow
	for {
		select {
		case <-s.stop:
			s.shutdown(&inter, &usage)
			return
		case r := <-s.interCh:
			inter = append(inter, r)
			inter, usage = s.drain(inter, usage)
			if len(inter)+len(usage) >= s.maxRows {
				inter, usage = s.flushStash(inter, usage, false)
			}
		case r := <-s.usageCh:
			usage = append(usage, r)
			inter, usage = s.drain(inter, usage)
			if len(inter)+len(usage) >= s.maxRows {
				inter, usage = s.flushStash(inter, usage, false)
			}
		case <-ticker.C:
			inter, usage = s.flushStash(inter, usage, false)
		}
	}
}

// drain moves everything currently waiting on the bounded buffers into the
// flusher's stash (non-blocking; the stash stays bounded by buffer capacity).
func (s *Store) drain(inter []interactionRow, usage []usageRow) ([]interactionRow, []usageRow) {
	for {
		select {
		case r := <-s.interCh:
			inter = append(inter, r)
		case r := <-s.usageCh:
			usage = append(usage, r)
		default:
			return inter, usage
		}
	}
}

// flushStash writes one batch per fact table and, on failure, pushes the
// unsent rows back onto the bounded buffers (retried on the next trigger).
// With final=true (Close path) failures are reported instead of requeued.
// Returns the remaining (unsent) rows.
func (s *Store) flushStash(inter []interactionRow, usage []usageRow, final bool) ([]interactionRow, []usageRow) {
	if len(inter) == 0 && len(usage) == 0 {
		return inter, usage
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.writeTimeout)
	defer cancel()
	finter, fusage, err := s.sendAll(ctx, inter, usage)
	if err == nil {
		s.log.Debug("clickhouse facts flushed", "interactions", len(inter), "usage", len(usage))
		return nil, nil
	}
	if final {
		// Last-chance flush failed: report; rows in hand are the documented
		// Close-time loss signal (server rejected them after all retries).
		s.log.Error("clickhouse facts flush on close failed",
			"code", "analytics.write_failed",
			"interactions", len(finter), "usage", len(fusage), "err", err)
		s.finalErr = errors.Join(s.finalErr, err)
		return nil, nil
	}
	// Structured log without PII: counts and the cause only. Rows go back to
	// the bounded buffers and are retried on the next flush trigger.
	s.log.Error("clickhouse facts flush failed; rows requeued for retry",
		"code", "analytics.write_failed",
		"interactions", len(finter), "usage", len(fusage), "err", err)
	dropped := s.requeue(finter, fusage)
	if dropped > 0 {
		s.log.Warn("clickhouse facts buffer overflow during requeue; oldest rows shed",
			"code", "analytics.buffer_overflow", "dropped", dropped)
	}
	return nil, nil
}

// sendAll writes both fact tables; it returns whichever rows failed to send
// so callers can requeue (flush) or report loss (Close).
func (s *Store) sendAll(ctx context.Context, inter []interactionRow, usage []usageRow) ([]interactionRow, []usageRow, error) {
	conn, err := s.ensureConn()
	if err != nil {
		return inter, usage, err
	}
	var sendErr error
	if len(inter) > 0 {
		if e := sendBatch(ctx, conn, insertInteraction, interactionAppender(inter)); e != nil {
			sendErr = e
		} else {
			inter = nil
		}
	}
	if len(usage) > 0 {
		if e := sendBatch(ctx, conn, insertUsage, usageAppender(usage)); e != nil {
			sendErr = errors.Join(sendErr, e)
		} else {
			usage = nil
		}
	}
	return inter, usage, sendErr
}

// ensureConn dials lazily; the driver's dial is bounded by DialTimeout
// (clamped to WriteTimeout in New). A dial failure behaves like any flush
// failure: rows requeue, the flusher retries on the next trigger. Called only
// from the flusher goroutine.
func (s *Store) ensureConn() (batchConn, error) {
	if s.conn != nil {
		return s.conn, nil
	}
	conn, err := s.newConn(s.dialOpts)
	if err != nil {
		return nil, err
	}
	s.conn = conn
	return conn, nil
}

func sendBatch(ctx context.Context, conn batchConn, query string, fill func(driver.Batch) error) error {
	batch, err := conn.PrepareBatch(ctx, query)
	if err != nil {
		return err
	}
	if err := fill(batch); err != nil {
		_ = batch.Abort()
		return err
	}
	if err := batch.Send(); err != nil {
		_ = batch.Abort()
		return err
	}
	return nil
}

func interactionAppender(rows []interactionRow) func(driver.Batch) error {
	return func(batch driver.Batch) error {
		for i := range rows {
			if err := batch.Append(rows[i].values()...); err != nil {
				return err
			}
		}
		return nil
	}
}

func usageAppender(rows []usageRow) func(driver.Batch) error {
	return func(batch driver.Batch) error {
		for i := range rows {
			if err := batch.Append(rows[i].values()...); err != nil {
				return err
			}
		}
		return nil
	}
}

// requeue pushes failed rows back onto the bounded buffers (newer arrivals
// may sit ahead of them — facts are ordered by occurred_at, not arrival). If
// a buffer is full, the oldest waiting row is shed first (channels are FIFO);
// if a slot still cannot be taken the current row is shed. Returns the number
// of dropped rows. Memory never exceeds the buffer caps; producers waiting on
// a full buffer are NOT starved by this path (offers are non-blocking).
// Called only from the flusher goroutine.
func (s *Store) requeue(inter []interactionRow, usage []usageRow) (dropped int) {
	for _, r := range inter {
		if !offerInteraction(s.interCh, r) {
			dropped++
		}
	}
	for _, r := range usage {
		if !offerUsage(s.usageCh, r) {
			dropped++
		}
	}
	return dropped
}

func offerInteraction(ch chan interactionRow, r interactionRow) bool {
	select {
	case ch <- r:
		return true
	default:
	}
	select { // full: shed the oldest waiting row (FIFO head) to make room
	case <-ch:
	default:
	}
	select {
	case ch <- r:
		return true
	default:
		return false
	}
}

func offerUsage(ch chan usageRow, r usageRow) bool {
	select {
	case ch <- r:
		return true
	default:
	}
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- r:
		return true
	default:
		return false
	}
}

// shutdown is the flusher's exit path (s.stop already closed, all producers
// gone): final drain, last-chance flush, connection close. Errors are
// recorded in s.finalErr for Close to surface.
func (s *Store) shutdown(inter *[]interactionRow, usage *[]usageRow) {
	*inter, *usage = s.drain(*inter, *usage)
	s.flushStash(*inter, *usage, true)
	if s.conn != nil {
		if err := s.conn.Close(); err != nil {
			s.finalErr = errors.Join(s.finalErr, err)
		}
		s.conn = nil
	}
	s.log.Debug("clickhouse facts store closed")
}

// Close stops the flusher, drains everything still buffered (flush-on-close),
// and closes the driver connection. It is idempotent; the first call owns
// shutdown. Producers blocked on a full buffer are released with
// analytics.clickhouse_store_closed; Handles racing Close are awaited via a
// WaitGroup, so no row is lost between the last accept and the final flush.
// If the final flush fails, Close returns the error — rows the server refused
// are the documented loss signal.
func (s *Store) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()

		close(s.stopping) // release producers blocked on a full buffer
		s.wg.Wait()       // every accepted row is now on a buffer
		close(s.stop)     // flusher: final drain + flush + conn close
		<-s.done
		err = s.finalErr
	})
	return err
}
