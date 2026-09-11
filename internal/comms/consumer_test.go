package comms

// DB-backed acceptance for the provider-events ledger consumer (issue #103):
// the INTERNAL ingest path (comms.IngestFunc — what the built-in simulator
// delivers through) persists signed provider events to provider_events and
// does nothing else; before #103 nothing consumed them, so outbound voice
// calls stayed pending forever. These tests reproduce exactly that flow and
// prove the drain applies the lifecycle transitions and marks the ledger
// rows processed — the same Processor the PUBLIC HTTP webhook path uses.
//
// Convention (interactions/clickhouse/opensearch suites): runs against a
// real PostgreSQL when ORVEXA_TEST_DATABASE_URL points at an Orvexa database
// with migrations applied (scripts/devstack.sh start prints the URL) and
// skips cleanly otherwise.

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Roy-Wanyoike/orvexa/internal/interactions"
	"github.com/Roy-Wanyoike/orvexa/internal/messaging"
	"github.com/Roy-Wanyoike/orvexa/internal/platform/outbox"
	"github.com/Roy-Wanyoike/orvexa/internal/telephony"
	"github.com/Roy-Wanyoike/orvexa/internal/webhooks"
)

type consumerHarness struct {
	pool     *pgxpool.Pool
	gateway  *webhooks.Gateway
	secret   string
	sim      *Simulator
	core     *interactions.Service
	consumer *EventConsumer
	org      string
	tenant   string
	customer string
}

func newConsumerHarness(t *testing.T) *consumerHarness {
	t.Helper()
	dsn := os.Getenv("ORVEXA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORVEXA_TEST_DATABASE_URL not set — skipping provider-events consumer suite (devstack path: scripts/devstack.sh start)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect db: %v", err)
	}

	h := &consumerHarness{pool: pool, org: uuid.NewString()}
	if _, err := pool.Exec(ctx,
		`INSERT INTO organizations (id, name, slug) VALUES ($1,$2,$3)`,
		h.org, "comms consumer", "comms-cons-"+h.org[:8]); err != nil {
		t.Fatalf("mint org: %v", err)
	}
	h.tenant = uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO tenants (id, organization_id, name) VALUES ($1,$2,$3)`,
		h.tenant, h.org, "comms-consumer"); err != nil {
		t.Fatalf("mint tenant: %v", err)
	}
	h.customer = uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO customers (id, tenant_id, display_name) VALUES ($1,$2,$3)`,
		h.customer, h.tenant, "consumer cust"); err != nil {
		t.Fatalf("mint customer: %v", err)
	}

	// The full production wiring in miniature: simulator → signed IngestFunc →
	// gateway.Ingest (ledger persist only) → EventConsumer (Processor + mark).
	h.secret = "whsec-" + uuid.NewString()
	h.gateway = webhooks.NewGateway(pool, h.secret)
	ingest := func(ctx context.Context, provider string, body []byte, signature string) error {
		_, err := h.gateway.Ingest(ctx, provider, body, signature)
		return err
	}
	h.sim = NewSimulator(ingest, func(body []byte) string { return webhooks.ComputeSignature(h.secret, body) }, 0)
	h.core = interactions.NewService(pool, outbox.NewWriter(pool), "comms-consumer-test")
	h.consumer = NewEventConsumer(pool, &Processor{Interactions: h.core}, h.gateway,
		EventConsumerConfig{MaxAttempts: 2}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	t.Cleanup(func() { h.teardown() })
	return h
}

// teardown removes every row the harness minted so re-runs never fight
// constraints (best-effort; devstack stays factory-resettable via
// scripts/devstack.sh clean). provider_events has no tenant FK (the
// tenant_hint column is deliberately unresolved) so the ledger rows are
// removed by their payload anchor and the poison marker id.
func (h *consumerHarness) teardown() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stmts := []string{
		`DELETE FROM provider_events WHERE payload->>'interaction_id' IN (SELECT id FROM interactions WHERE tenant_id = $1)
                 OR (provider = 'simulator' AND provider_event_id LIKE 'poison-%')`,
		`DELETE FROM outbox_events WHERE tenant_id = $1`,
		`DELETE FROM interactions WHERE tenant_id = $1`,
		`DELETE FROM conversation_participants WHERE conversation_id IN (SELECT id FROM conversations WHERE tenant_id = $1)`,
		`DELETE FROM conversations WHERE tenant_id = $1`,
		`DELETE FROM customers WHERE tenant_id = $1`,
		`DELETE FROM tenants WHERE id = $1`,
		`DELETE FROM organizations WHERE id = $1`,
	}
	for _, q := range stmts {
		_, _ = h.pool.Exec(ctx, q, h.tenant)
	}
	h.pool.Close()
}

// unprocessedEvents lists the events still waiting in the ledger for one
// interaction, in arrival order.
func (h *consumerHarness) unprocessedEvents(t *testing.T, ctx context.Context, interactionID string) []string {
	t.Helper()
	rows, err := h.pool.Query(ctx, `
                SELECT payload->>'event' FROM provider_events
                WHERE processed_at IS NULL AND payload->>'interaction_id' = $1
                ORDER BY received_at, id`, interactionID)
	if err != nil {
		t.Fatalf("query unprocessed events: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ev string
		if err := rows.Scan(&ev); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate events: %v", err)
	}
	return out
}

// processedCount counts ledger rows already marked processed for one
// interaction.
func (h *consumerHarness) processedCount(t *testing.T, ctx context.Context, interactionID string) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(ctx, `
                SELECT count(*) FROM provider_events
                WHERE processed_at IS NOT NULL AND payload->>'interaction_id' = $1`,
		interactionID).Scan(&n); err != nil {
		t.Fatalf("count processed: %v", err)
	}
	return n
}

// insertPoison writes a well-formed but unsupported event directly into the
// ledger (bypassing the gateway, exactly what a broken/misbehaving provider
// integration would produce). It returns the ledger's provider_event_id.
func (h *consumerHarness) insertPoison(t *testing.T, ctx context.Context, interactionID string) string {
	t.Helper()
	eventID := "poison-" + uuid.NewString()
	payload := `{"event":"call.detonate","interaction_id":"` + interactionID + `","tenant_id":"` + h.tenant + `"}`
	if _, err := h.pool.Exec(ctx, `
                INSERT INTO provider_events (id, provider, provider_event_id, payload, signature_valid)
                VALUES ($1, 'simulator', $2, $3::jsonb, true)`,
		uuid.NewString(), eventID, payload); err != nil {
		t.Fatalf("insert poison row: %v", err)
	}
	return eventID
}

// TestProviderEventsConsumerVoiceLifecycle is the #103 acceptance: a call
// placed on the default simulator carrier persists call.ringing +
// call.connected unprocessed (defect: stays pending), then one consumer tick
// advances it to active with the ledger stamped processed; hangup follows
// the same path to wrapup.
func TestProviderEventsConsumerVoiceLifecycle(t *testing.T) {
	h := newConsumerHarness(t)
	ctx := context.Background()

	tel := telephony.NewService(h.core, h.sim)
	rec, err := tel.PlaceCall(ctx, h.tenant, telephony.PlaceCallInput{CustomerID: h.customer, To: "+254710000009"})
	if err != nil {
		t.Fatalf("place call: %v", err)
	}

	// The defect symptom: receipts are persisted (signed, deduped) but
	// nothing applies them — the call is still pending.
	if rec.Status != interactions.StatusPending {
		t.Fatalf("pre-consumer status = %q, want pending", rec.Status)
	}
	got := h.unprocessedEvents(t, ctx, rec.ID)
	if len(got) != 2 || got[0] != "call.ringing" || got[1] != "call.connected" {
		t.Fatalf("unprocessed ledger events = %v, want [call.ringing call.connected]", got)
	}

	n, err := h.consumer.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if n != 2 {
		t.Fatalf("tick applied %d rows, want 2", n)
	}
	cur, err := h.core.Get(ctx, h.tenant, rec.ID)
	if err != nil {
		t.Fatalf("get interaction: %v", err)
	}
	if cur.Status != interactions.StatusActive {
		t.Fatalf("post-tick status = %q, want active", cur.Status)
	}
	if cur.AnsweredAt == nil {
		t.Fatal("active transition should stamp answered_at")
	}
	if left := h.unprocessedEvents(t, ctx, rec.ID); len(left) != 0 {
		t.Fatalf("unprocessed after tick = %v, want none", left)
	}
	if c := h.processedCount(t, ctx, rec.ID); c != 2 {
		t.Fatalf("processed ledger rows = %d, want 2 (processed_at must be stamped)", c)
	}

	// A second tick is a clean no-op: nothing unprocessed remains.
	if n, err := h.consumer.Tick(ctx); err != nil || n != 0 {
		t.Fatalf("second tick = (%d, %v), want (0, nil)", n, err)
	}

	// hangup: the service deliberately does NOT duplicate provider-driven
	// transitions — call.ended lands in the ledger and the consumer moves
	// active → wrapup.
	if _, err := tel.ApplyAction(ctx, h.tenant, rec.ID, "hangup", ""); err != nil {
		t.Fatalf("apply hangup: %v", err)
	}
	if cur, _ = h.core.Get(ctx, h.tenant, rec.ID); cur.Status != interactions.StatusActive {
		t.Fatalf("status after hangup action = %q, want still active (webhook drives it)", cur.Status)
	}
	if n, err := h.consumer.Tick(ctx); err != nil || n != 1 {
		t.Fatalf("tick after hangup = (%d, %v), want (1, nil)", n, err)
	}
	if cur, _ = h.core.Get(ctx, h.tenant, rec.ID); cur.Status != interactions.StatusWrapup {
		t.Fatalf("status after consumed call.ended = %q, want wrapup", cur.Status)
	}
	if left := h.unprocessedEvents(t, ctx, rec.ID); len(left) != 0 {
		t.Fatalf("unprocessed after wrapup = %v, want none", left)
	}
}

// TestProviderEventsConsumerMessageReceipts covers the messaging side of
// #103: Send transitions pending→active itself (which masked the defect),
// the simulator's sent/delivered receipts are consumed as no-ops, and a
// message.read delivery through the public gateway completes the lifecycle.
func TestProviderEventsConsumerMessageReceipts(t *testing.T) {
	h := newConsumerHarness(t)
	ctx := context.Background()

	msgs := messaging.NewService(h.core, h.sim)
	rec, err := msgs.Send(ctx, h.tenant, messaging.SendInput{
		CustomerID: h.customer, Channel: "whatsapp", From: "orvexa-messaging",
		To: "+254710000010", Body: "receipt check",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	got := h.unprocessedEvents(t, ctx, rec.ID)
	if len(got) != 2 || got[0] != "message.sent" || got[1] != "message.delivered" {
		t.Fatalf("unprocessed ledger events = %v, want [message.sent message.delivered]", got)
	}
	n, err := h.consumer.Tick(ctx)
	if err != nil || n != 2 {
		t.Fatalf("tick = (%d, %v), want (2, nil)", n, err)
	}
	if cur, _ := h.core.Get(ctx, h.tenant, rec.ID); cur.Status != interactions.StatusActive {
		t.Fatalf("status after receipts = %q, want active (same-state no-op)", cur.Status)
	}

	// message.read arrives through the PUBLIC webhook gateway — the same
	// entry a real carrier callback uses — and the consumer completes it.
	body, err := (&ProviderEvent{Event: "message.read", InteractionID: rec.ID, TenantID: h.tenant}).Encode()
	if err != nil {
		t.Fatalf("encode read receipt: %v", err)
	}
	if _, err := h.gateway.Ingest(ctx, "simulator", body, webhooks.ComputeSignature(h.secret, body)); err != nil {
		t.Fatalf("ingest read receipt: %v", err)
	}
	if n, err := h.consumer.Tick(ctx); err != nil || n != 1 {
		t.Fatalf("tick after read = (%d, %v), want (1, nil)", n, err)
	}
	if cur, _ := h.core.Get(ctx, h.tenant, rec.ID); cur.Status != interactions.StatusCompleted {
		t.Fatalf("status after consumed read = %q, want completed", cur.Status)
	}
}

// TestProviderEventsConsumerPoisonRows proves the honest failure story: a
// poison row (well-formed JSON, unsupported event) fails, stays unprocessed,
// exhausts its in-process attempts and is then skipped — while the drain
// keeps flowing for healthy rows behind it.
func TestProviderEventsConsumerPoisonRows(t *testing.T) {
	h := newConsumerHarness(t)
	ctx := context.Background()

	// Backlog is a global health signal; the devstack database is shared, so
	// assert the DELTA around this test, not an absolute number.
	baseline, err := h.consumer.Backlog(ctx)
	if err != nil {
		t.Fatalf("baseline backlog: %v", err)
	}
	tel := telephony.NewService(h.core, h.sim)
	rec, err := tel.PlaceCall(ctx, h.tenant, telephony.PlaceCallInput{CustomerID: h.customer, To: "+254710000011"})
	if err != nil {
		t.Fatalf("place call: %v", err)
	}
	poisonID := h.insertPoison(t, ctx, rec.ID)

	// Tick 1: two healthy rows apply, the poison row fails (attempt 1).
	if n, err := h.consumer.Tick(ctx); err != nil || n != 2 {
		t.Fatalf("tick 1 = (%d, %v), want (2, nil)", n, err)
	}
	// Tick 2: poison fails again — attempt 2 = cap reached.
	if n, err := h.consumer.Tick(ctx); err != nil || n != 0 {
		t.Fatalf("tick 2 = (%d, %v), want (0, nil)", n, err)
	}
	if !h.consumer.skipped("simulator", poisonID) {
		t.Fatal("poison row should be skipped after max attempts")
	}
	// Tick 3: the poison row is skipped without another attempt.
	if n, err := h.consumer.Tick(ctx); err != nil || n != 0 {
		t.Fatalf("tick 3 = (%d, %v), want (0, nil)", n, err)
	}
	// The row itself is never mutated: still unprocessed in the ledger for
	// operator inspection (no schema change, no silent deletion).
	var stillUnprocessed int
	if err := h.pool.QueryRow(ctx, `
                SELECT count(*) FROM provider_events
                WHERE processed_at IS NULL AND provider_event_id = $1`, poisonID).Scan(&stillUnprocessed); err != nil {
		t.Fatalf("check poison row: %v", err)
	}
	if stillUnprocessed != 1 {
		t.Fatalf("poison row unprocessed count = %d, want 1", stillUnprocessed)
	}

	// No starvation within a batch: fresh healthy rows behind the poison row
	// still drain.
	rec2, err := tel.PlaceCall(ctx, h.tenant, telephony.PlaceCallInput{CustomerID: h.customer, To: "+254710000012"})
	if err != nil {
		t.Fatalf("place second call: %v", err)
	}
	if n, err := h.consumer.Tick(ctx); err != nil || n != 2 {
		t.Fatalf("tick after second call = (%d, %v), want (2, nil)", n, err)
	}
	if cur, _ := h.core.Get(ctx, h.tenant, rec2.ID); cur.Status != interactions.StatusActive {
		t.Fatalf("second call status = %q, want active", cur.Status)
	}
	if left := h.unprocessedEvents(t, ctx, rec2.ID); len(left) != 0 {
		t.Fatalf("second call unprocessed = %v, want none", left)
	}

	// Net effect on the ledger: exactly the poison row remains unprocessed.
	backlog, err := h.consumer.Backlog(ctx)
	if err != nil {
		t.Fatalf("backlog: %v", err)
	}
	if backlog != baseline+1 {
		t.Fatalf("backlog = %d, want baseline+1 = %d (the poison row)", backlog, baseline+1)
	}
}

// TestEventConsumerConfigDefaults pins the documented tuning defaults
// without touching a database.
func TestEventConsumerConfigDefaults(t *testing.T) {
	c := NewEventConsumer(nil, nil, nil, EventConsumerConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if c.interval != 5*time.Second {
		t.Fatalf("default interval = %v, want 5s", c.interval)
	}
	if c.batchSize != 100 {
		t.Fatalf("default batchSize = %d, want 100", c.batchSize)
	}
	if c.maxAttempts != 10 {
		t.Fatalf("default maxAttempts = %d, want 10", c.maxAttempts)
	}
	if c.log == nil {
		t.Fatal("nil logger must degrade to the default slog logger")
	}
}
