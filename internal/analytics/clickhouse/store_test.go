package clickhouse

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"

	"github.com/Roy-Wanyoike/orvexa/internal/analytics"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
	"github.com/Roy-Wanyoike/orvexa/pkg/events"
)

// Interface mapping (the swap contract): the Postgres-backed consumer
// satisfies FactsStore with its existing Handle method, and the batched
// store satisfies the same interface — the worker can select between them
// on ORVEXA_CLICKHOUSE_URL without reshaping the bus subscription.
var _ FactsStore = analytics.NewConsumer(nil)
var _ FactsStore = (*Store)(nil)

// --- driver fakes -----------------------------------------------------------

// fakeBatch implements driver.Batch by embedding the interface (per the
// driver's own guidance: embed and override; added methods won't break) and
// recording appended rows and lifecycle calls.
type fakeBatch struct {
	driver.Batch

	mu      sync.Mutex
	rows    [][]any
	sent    int
	aborted int
	sendErr error // sticky failure for every Send

	// gate, when non-nil, blocks Send until the channel is closed — simulates
	// a wedged server so tests can fill the bounded buffers deterministically.
	gate chan struct{}
}

func (b *fakeBatch) Append(v ...any) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := make([]any, len(v))
	copy(cp, v)
	b.rows = append(b.rows, cp)
	return nil
}

func (b *fakeBatch) Send() error {
	if b.gate != nil {
		<-b.gate // block until the test opens the gate
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent++
	return b.sendErr
}

func (b *fakeBatch) Abort() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.aborted++
	return nil
}

func (b *fakeBatch) stats() (rows, sent, aborted int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.rows), b.sent, b.aborted
}

// fakeConn implements the store's narrow batchConn interface and hands out
// fresh fakeBatch instances per PrepareBatch call.
type fakeConn struct {
	mu         sync.Mutex
	batches    []*fakeBatch
	queries    []string
	ctxs       []context.Context
	prepareErr error
	closeErr   error
	failSends  int // next N batches fail Send (simulates a flaky server)
	gateSends  int // first N batches block in Send until openGate (wedged server)
	gates      []chan struct{}
	closed     int
}

func (c *fakeConn) PrepareBatch(ctx context.Context, query string, _ ...driver.PrepareBatchOption) (driver.Batch, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.prepareErr != nil {
		return nil, c.prepareErr
	}
	b := &fakeBatch{}
	if c.failSends > 0 {
		b.sendErr = errors.New("simulated send failure")
		c.failSends--
	}
	if c.gateSends > 0 {
		b.gate = make(chan struct{})
		c.gates = append(c.gates, b.gate)
		c.gateSends--
	}
	c.batches = append(c.batches, b)
	c.queries = append(c.queries, query)
	c.ctxs = append(c.ctxs, ctx)
	return b, nil
}

func (c *fakeConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed++
	return c.closeErr
}

// openGate releases the nth gated batch's Send; openAllGates releases every
// pending gate (used before Close so shutdown flushes are not wedged).
func (c *fakeConn) openGate(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n < len(c.gates) {
		close(c.gates[n])
	}
}

func (c *fakeConn) openAllGates() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, g := range c.gates {
		select {
		case <-g: // already closed
		default:
			close(g)
		}
	}
}

func (c *fakeConn) gateCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.gates)
}

func (c *fakeConn) batchFor(query string) *fakeBatch {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, q := range c.queries {
		if q == query {
			return c.batches[i]
		}
	}
	return nil
}

func (c *fakeConn) batchCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.batches)
}

// --- helpers ----------------------------------------------------------------

const (
	testTenantID = "11111111-1111-1111-1111-111111111111"
	testInterID  = "22222222-2222-2222-2222-222222222222"
)

var testURL = "clickhouse://default:orvexa@127.0.0.1:9000/orvexa"

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// newTestStore builds a store through New (so production wiring runs) and
// then swaps the driver constructor for the fake — valid because tests hold
// the same package and no flush can race the swap before events arrive (the
// flusher only touches the connection once rows are buffered).
func newTestStore(t *testing.T, fc *fakeConn, maxRows int, flushEvery time.Duration) *Store {
	t.Helper()
	s, err := New(Options{
		URL:            testURL,
		MaxBatchRows:   maxRows,
		FlushInterval:  flushEvery,
		WriteTimeout:   2 * time.Second,
		EnqueueTimeout: 2 * time.Second,
	}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.newConn = func(*clickhouse.Options) (batchConn, error) { return fc, nil }
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func envelope(t *testing.T, id, topic, subject string, data map[string]any) *events.Envelope {
	t.Helper()
	return &events.Envelope{
		ID:       id,
		Type:     topic,
		Source:   "test",
		Subject:  subject,
		TenantID: testTenantID,
		Time:     time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
		Data:     data,
	}
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(msg)
}

// --- batching lifecycle -------------------------------------------------------

func TestFlushOnMaxRows(t *testing.T) {
	fc := &fakeConn{}
	s := newTestStore(t, fc, 4, time.Hour)

	data := map[string]any{"channel": "voice", "direction": "inbound", "status": "completed"}
	for i := 0; i < 2; i++ {
		if err := s.Handle(context.Background(), envelope(t, uuid.NewString(), events.TopicInteractionCreated, testInterID, data)); err != nil {
			t.Fatalf("handle interaction: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := s.Handle(context.Background(), envelope(t, uuid.NewString(), events.TopicUsageRecorded, "", map[string]any{"metric": "tokens", "output_tokens": float64(42)})); err != nil {
			t.Fatalf("handle usage: %v", err)
		}
	}
	waitFor(t, func() bool { return fc.batchCount() >= 2 }, "cap-flush did not fire both batches")

	ib, ub := fc.batchFor(insertInteraction), fc.batchFor(insertUsage)
	if ib == nil || ub == nil {
		t.Fatalf("missing batches: got %v", fc.queries)
	}
	rows, sent, _ := ib.stats()
	if rows != 2 || sent != 1 {
		t.Fatalf("interaction batch rows=%d sent=%d, want 2/1", rows, sent)
	}
	rows, sent, _ = ub.stats()
	if rows != 2 || sent != 1 {
		t.Fatalf("usage batch rows=%d sent=%d, want 2/1", rows, sent)
	}

	// Bound values: parameterized append must carry typed values in column order.
	ib.mu.Lock()
	first := ib.rows[0]
	ib.mu.Unlock()
	if _, ok := first[0].(uuid.UUID); !ok {
		t.Fatalf("event_id not a uuid.UUID: %T", first[0])
	}
	if first[3] != "voice" || first[4] != "inbound" || first[5] != "completed" {
		t.Fatalf("interaction strings wrong: %v", first[3:6])
	}
	if ts, ok := first[6].(time.Time); !ok || ts.IsZero() {
		t.Fatalf("occurred_at not a time.Time: %T", first[6])
	}
	ub.mu.Lock()
	urow := ub.rows[0]
	ub.mu.Unlock()
	if urow[2] != "tokens" || urow[3] != int64(42) {
		t.Fatalf("usage row wrong: %v", urow)
	}
}

func TestFlushOnInterval(t *testing.T) {
	fc := &fakeConn{}
	s := newTestStore(t, fc, 100, 10*time.Millisecond)

	if err := s.Handle(context.Background(), envelope(t, uuid.NewString(), events.TopicUsageRecorded, "", map[string]any{"metric": "tokens", "output_tokens": 7})); err != nil {
		t.Fatalf("handle: %v", err)
	}
	waitFor(t, func() bool { return fc.batchCount() > 0 }, "interval flush did not fire")
	ub := fc.batchFor(insertUsage)
	rows, sent, _ := ub.stats()
	if rows != 1 || sent != 1 {
		t.Fatalf("usage batch rows=%d sent=%d, want 1/1", rows, sent)
	}
}

func TestFlushOnClose(t *testing.T) {
	fc := &fakeConn{}
	s := newTestStore(t, fc, 100, time.Hour)

	data := map[string]any{"channel": "sms"}
	for i := 0; i < 3; i++ {
		if err := s.Handle(context.Background(), envelope(t, uuid.NewString(), events.TopicInteractionUpdated, testInterID, data)); err != nil {
			t.Fatalf("handle: %v", err)
		}
	}
	if got := fc.batchCount(); got != 0 {
		t.Fatalf("no batch expected before close, got %d", got)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	ib := fc.batchFor(insertInteraction)
	if ib == nil {
		t.Fatal("flush-on-close did not prepare interaction batch")
	}
	rows, sent, _ := ib.stats()
	if rows != 3 || sent != 1 {
		t.Fatalf("interaction batch rows=%d sent=%d, want 3/1", rows, sent)
	}
	if fc.closed == 0 {
		t.Fatal("driver connection was not closed")
	}
	// Double close is a no-op; Handle after close errors.
	if err := s.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	err := s.Handle(context.Background(), envelope(t, uuid.NewString(), events.TopicUsageRecorded, "", nil))
	var appErr *apperrors.Error
	if !errors.As(err, &appErr) || appErr.Code != "analytics.clickhouse_store_closed" {
		t.Fatalf("handle after close: want closed-store error, got %v", err)
	}
}

// --- failure, retry, backpressure ---------------------------------------------

func TestSendFailureRequeuesThenRecovers(t *testing.T) {
	fc := &fakeConn{failSends: 1} // first Send fails, retries succeed
	s := newTestStore(t, fc, 2, 15*time.Millisecond)

	if err := s.Handle(context.Background(), envelope(t, uuid.NewString(), events.TopicUsageRecorded, "", map[string]any{"metric": "m", "output_tokens": 1})); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if err := s.Handle(context.Background(), envelope(t, uuid.NewString(), events.TopicUsageRecorded, "", map[string]any{"metric": "m", "output_tokens": 2})); err != nil {
		t.Fatalf("handle: %v", err)
	}
	// Flush attempt 1 fails server-side; rows are requeued, not dropped.
	// Flush attempt 2 (interval tick) delivers both buffered rows.
	waitFor(t, func() bool {
		ub := fc.batchFor(insertUsage)
		if ub == nil {
			return false
		}
		rows, sent, aborted := ub.stats()
		return rows == 2 && sent == 1 && aborted == 1
	}, "failed flush was not retried to success (requeue path broken)")

	if err := s.Close(); err != nil {
		t.Fatalf("close after recovery: %v", err)
	}
}

func TestSendFailureOnCloseReturnsError(t *testing.T) {
	s, err := New(Options{URL: testURL, FlushInterval: time.Hour, WriteTimeout: time.Second}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	alwaysFail := &fakeConn{prepareErr: errors.New("no server")}
	s.newConn = func(*clickhouse.Options) (batchConn, error) { return alwaysFail, nil }

	if err := s.Handle(context.Background(), envelope(t, uuid.NewString(), events.TopicUsageRecorded, "", nil)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if err := s.Close(); err == nil {
		t.Fatal("close must surface the failed final flush")
	}
}

// TestBackpressureBlocksThenFailsWithTypedError pins the backpressure
// contract: with a wedged server the bounded buffers fill, Handle blocks up
// to EnqueueTimeout, and then fails with analytics.clickhouse_backpressure —
// it never blocks forever and never grows memory without bound.
func TestBackpressureBlocksThenFailsWithTypedError(t *testing.T) {
	fc := &fakeConn{gateSends: 1}
	s, err := New(Options{
		URL:            testURL,
		MaxBatchRows:   2,
		FlushInterval:  time.Hour, // no ticker escapes; only the gated Send path runs
		WriteTimeout:   time.Second,
		EnqueueTimeout: 80 * time.Millisecond,
	}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.newConn = func(*clickhouse.Options) (batchConn, error) { return fc, nil }
	t.Cleanup(func() {
		fc.openAllGates() // never leave the flusher wedged during shutdown
		_ = s.Close()
	})

	usage := func(n int) *events.Envelope {
		return envelope(t, uuid.NewString(), events.TopicUsageRecorded, "", map[string]any{"metric": "m", "output_tokens": n})
	}
	// Row 1 triggers a flush whose Send wedges on the gate; rows keep the
	// buffer occupied. Fill: 1 in flight to Send, then buffer cap (2), then
	// one more batch prepared (gate) while the flusher waits to hand rows in.
	var backpressured int
	for i := 0; i < 64; i++ {
		err := s.Handle(context.Background(), usage(i))
		if err == nil {
			continue
		}
		var appErr *apperrors.Error
		if !errors.As(err, &appErr) || appErr.Code != "analytics.clickhouse_backpressure" {
			t.Fatalf("row %d: want typed backpressure error, got %v", i, err)
		}
		backpressured++
		break
	}
	if backpressured == 0 {
		t.Fatal("expected at least one backpressure error with a wedged server and full buffer")
	}
	if got := s.buffered(); got > 2 { // usage buffer cap = MaxBatchRows = 2; only usage rows flow
		t.Fatalf("buffer exceeded its bound: %d rows", got)
	}

	// Producers recover once the server un-wedges: open the gate, wait for
	// the flush to complete, then a fresh Handle is accepted immediately.
	fc.openGate(0)
	waitFor(t, func() bool {
		ub := fc.batchFor(insertUsage)
		if ub == nil {
			return false
		}
		_, sent, _ := ub.stats()
		return sent > 0
	}, "gated Send never completed")
	if err := s.Handle(context.Background(), usage(100)); err != nil {
		t.Fatalf("handle after recovery: %v", err)
	}
}

// TestBufferBoundedUnderPersistentFailure pins the no-unbounded-memory rule:
// with a permanently dead server the flusher keeps recycling rows through the
// bounded buffers, Handle calls always complete promptly (some with the typed
// backpressure error when they land on a momentarily full buffer), the buffer
// never exceeds its caps, and Close still reports the failed final flush.
func TestBufferBoundedUnderPersistentFailure(t *testing.T) {
	fc := &fakeConn{prepareErr: errors.New("no server")}
	s, err := New(Options{
		URL:            testURL,
		MaxBatchRows:   4,
		FlushInterval:  5 * time.Millisecond,
		WriteTimeout:   time.Second,
		EnqueueTimeout: 50 * time.Millisecond, // short: the loop expects prompt outcomes
	}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.newConn = func(*clickhouse.Options) (batchConn, error) { return fc, nil }
	t.Cleanup(func() { _ = s.Close() })

	for i := 0; i < 40; i++ {
		env := envelope(t, uuid.NewString(), events.TopicUsageRecorded, "", map[string]any{"metric": "m"})
		if err := s.Handle(context.Background(), env); err != nil {
			var appErr *apperrors.Error
			if !errors.As(err, &appErr) || appErr.Code != "analytics.clickhouse_backpressure" {
				t.Fatalf("row %d: want typed backpressure error, got %v", i, err)
			}
		}
		if got := s.buffered(); got > 8 { // 2 tables × cap 4
			t.Fatalf("buffer exceeded its bound mid-loop: %d rows", got)
		}
	}
	if err := s.Close(); err == nil {
		t.Fatal("close on dead server must report the flush failure")
	}
}

// --- envelope semantics (parity with the Postgres consumer) ---------------------

func TestNonFactTopicsAndMissingSubjectIgnored(t *testing.T) {
	fc := &fakeConn{}
	s := newTestStore(t, fc, 10, time.Hour)

	cases := []struct {
		name string
		env  *events.Envelope
	}{
		{"audit topic", envelope(t, uuid.NewString(), events.TopicAuditRecorded, "x", nil)},
		{"webhook topic", envelope(t, uuid.NewString(), events.TopicWebhookReceived, "x", nil)},
		{"assigned topic (not a fact)", envelope(t, uuid.NewString(), events.TopicInteractionAssigned, testInterID, nil)},
		{"nil envelope", nil},
		{"interaction without subject", envelope(t, uuid.NewString(), events.TopicInteractionCreated, "", map[string]any{"channel": "voice"})},
	}
	for _, tc := range cases {
		if err := s.Handle(context.Background(), tc.env); err != nil {
			t.Fatalf("%s: handle: %v", tc.name, err)
		}
	}
	if got := s.buffered(); got != 0 {
		t.Fatalf("buffered=%d, want 0 (nothing enqueued)", got)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := fc.batchCount(); got != 0 {
		t.Fatalf("no server I/O expected, got %d batches", got)
	}
}

func TestInvalidIdentifiersMirrorPostgresErrorSurface(t *testing.T) {
	fc := &fakeConn{}
	s := newTestStore(t, fc, 10, time.Hour)

	cases := []struct {
		name string
		env  *events.Envelope
	}{
		{"bad interaction id", envelope(t, uuid.NewString(), events.TopicInteractionCreated, "not-a-uuid", nil)},
		{"bad tenant id", &events.Envelope{ID: uuid.NewString(), Type: events.TopicInteractionCreated, Subject: testInterID, TenantID: "nope", Time: time.Now().UTC(), Data: map[string]any{}}},
		{"bad event id", &events.Envelope{ID: "nope", Type: events.TopicUsageRecorded, TenantID: testTenantID, Time: time.Now().UTC(), Data: map[string]any{}}},
		{"bad usage tenant", &events.Envelope{ID: uuid.NewString(), Type: events.TopicUsageRecorded, TenantID: "", Time: time.Now().UTC(), Data: map[string]any{}}},
	}
	for _, tc := range cases {
		err := s.Handle(context.Background(), tc.env)
		var appErr *apperrors.Error
		if !errors.As(err, &appErr) || appErr.Code != "analytics.write_failed" {
			t.Fatalf("%s: want analytics.write_failed, got %v", tc.name, err)
		}
	}
	if got := s.buffered(); got != 0 {
		t.Fatalf("failed envelopes must not buffer (buffered=%d)", got)
	}
}

// --- parameterization, timeouts, hygiene ----------------------------------------

func TestQueriesAreFixedParameterizedStatements(t *testing.T) {
	fc := &fakeConn{}
	s := newTestStore(t, fc, 2, time.Hour)

	if err := s.Handle(context.Background(), envelope(t, uuid.NewString(), events.TopicInteractionCompleted, testInterID, map[string]any{"channel": "voice'; DROP TABLE x"})); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if err := s.Handle(context.Background(), envelope(t, uuid.NewString(), events.TopicUsageRecorded, "", map[string]any{"metric": "m'}"})); err != nil {
		t.Fatalf("handle: %v", err)
	}
	waitFor(t, func() bool { return fc.batchCount() >= 2 }, "flush did not fire")

	fc.mu.Lock()
	queries := append([]string(nil), fc.queries...)
	fc.mu.Unlock()
	want := map[string]bool{insertInteraction: false, insertUsage: false}
	for _, q := range queries {
		if _, ok := want[q]; !ok {
			t.Fatalf("unexpected query text (string-built SQL?): %q", q)
		}
		want[q] = true
	}
	for q, seen := range want {
		if !seen {
			t.Fatalf("expected parameterized statement never used: %q", q)
		}
	}
}

// TestHandleContextCancellationNeverBlocksDelivery: a cancelled caller ctx
// must not stall or drop delivery while the buffer has room — the fast path
// accepts the row without consulting ctx.
func TestHandleContextCancellationNeverBlocksDelivery(t *testing.T) {
	fc := &fakeConn{}
	s := newTestStore(t, fc, 1, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // hostile caller context — must not affect delivery
	if err := s.Handle(ctx, envelope(t, uuid.NewString(), events.TopicInteractionCreated, testInterID, nil)); err != nil {
		t.Fatalf("handle with cancelled ctx: %v", err)
	}
	waitFor(t, func() bool { return fc.batchCount() > 0 }, "flush did not fire despite cancelled caller ctx")
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestCancelledContextUnblocksBackpressuredHandle: when the buffer is full
// AND the caller ctx is cancelled, Handle must return promptly instead of
// burning the whole EnqueueTimeout.
func TestCancelledContextUnblocksBackpressuredHandle(t *testing.T) {
	fc := &fakeConn{gateSends: 1}
	s, err := New(Options{
		URL:            testURL,
		MaxBatchRows:   1,
		FlushInterval:  time.Hour,
		WriteTimeout:   time.Second,
		EnqueueTimeout: 5 * time.Second, // longer than the test is willing to wait
	}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.newConn = func(*clickhouse.Options) (batchConn, error) { return fc, nil }
	t.Cleanup(func() {
		fc.openAllGates() // never leave the flusher wedged during shutdown
		_ = s.Close()
	})

	usage := map[string]any{"metric": "m"}
	// Row 1: accepted, consumed by the flusher, whose Send wedges on the gate.
	if err := s.Handle(context.Background(), envelope(t, uuid.NewString(), events.TopicUsageRecorded, "", usage)); err != nil {
		t.Fatalf("first handle: %v", err)
	}
	waitFor(t, func() bool { return fc.gateCount() > 0 }, "server was never wedged")

	// Row 2: refills the buffer (cap 1) via the fast path.
	if err := s.Handle(context.Background(), envelope(t, uuid.NewString(), events.TopicUsageRecorded, "", usage)); err != nil {
		t.Fatalf("second handle: %v", err)
	}

	// Row 3: the buffer is full → Handle blocks; a cancelled ctx must release
	// it promptly with the Postgres-parity error surface.
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		done <- s.Handle(ctx, envelope(t, uuid.NewString(), events.TopicUsageRecorded, "", usage))
	}()
	// The Handle is now in the slow path (nothing observed it yet — give the
	// goroutine a beat to block, then cancel).
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		var appErr *apperrors.Error
		if !errors.As(err, &appErr) || appErr.Code != "analytics.write_failed" {
			t.Fatalf("want ctx-cancel surfaced as analytics.write_failed, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled ctx did not unblock the backpressured Handle")
	}
}

func TestFlushCarriesWriteTimeoutDeadline(t *testing.T) {
	fc := &fakeConn{}
	s := newTestStore(t, fc, 1, time.Hour)

	if err := s.Handle(context.Background(), envelope(t, uuid.NewString(), events.TopicUsageRecorded, "", nil)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	waitFor(t, func() bool { return fc.batchCount() > 0 }, "flush did not fire")

	fc.mu.Lock()
	ctx := fc.ctxs[0]
	fc.mu.Unlock()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("flush context must carry a deadline (context timeout contract)")
	}
	if remaining := time.Until(deadline); remaining > 2*time.Second+250*time.Millisecond {
		t.Fatalf("deadline too far out: %v", remaining)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestZeroTimeNormalized(t *testing.T) {
	fc := &fakeConn{}
	s := newTestStore(t, fc, 1, time.Hour)

	env := envelope(t, uuid.NewString(), events.TopicUsageRecorded, "", nil)
	env.Time = time.Time{} // unvalidated envelope with zero time
	if err := s.Handle(context.Background(), env); err != nil {
		t.Fatalf("handle: %v", err)
	}
	waitFor(t, func() bool { return fc.batchCount() > 0 }, "flush did not fire")
	ub := fc.batchFor(insertUsage)
	ub.mu.Lock()
	ts := ub.rows[0][4].(time.Time)
	ub.mu.Unlock()
	if ts.IsZero() || ts.Before(time.Now().Add(-time.Minute)) {
		t.Fatalf("zero time must normalize to now, got %v", ts)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}
