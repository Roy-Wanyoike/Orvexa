//go:build integration

// Integration suite for DB-backed paths. Runs ONLY when ORVEXA_TEST_DATABASE_URL
// points at an Orvexa database with migrations applied:
//
//	ORVEXA_TEST_DATABASE_URL=postgres://... go test -race -tags=integration ./...
package integration

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Roy-Wanyoike/orvexa/internal/platform/db"
	"github.com/Roy-Wanyoike/orvexa/internal/platform/outbox"
	"github.com/Roy-Wanyoike/orvexa/internal/webhooks"
	"github.com/Roy-Wanyoike/orvexa/pkg/events"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("ORVEXA_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ORVEXA_TEST_DATABASE_URL not set — skipping integration suite")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, url, 5)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestWebhookIngestDeduplicatesReplays(t *testing.T) {
	pool := testPool(t)
	g := webhooks.NewGateway(pool, "secret-1")

	body := []byte(`{"event":"call.ringing","provider_ref":"CA-1","nonce":"n-42"}`)
	sig := webhooks.ComputeSignature("secret-1", body)

	r1, err := g.Ingest(context.Background(), "simulator", body, sig)
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if r1.Duplicate {
		t.Fatal("first delivery must not be duplicate")
	}
	r2, err := g.Ingest(context.Background(), "simulator", body, sig)
	if err != nil {
		t.Fatalf("replay ingest: %v", err)
	}
	if !r2.Duplicate || r2.EventID != r1.EventID {
		t.Fatalf("replay must return original event id: %+v vs %+v", r1, r2)
	}
}

func TestWebhookRejectsBadSignature(t *testing.T) {
	pool := testPool(t)
	g := webhooks.NewGateway(pool, "secret-1")
	body := []byte(`{"event":"call.ringing","nonce":"n-43"}`)
	if _, err := g.Ingest(context.Background(), "simulator", body, "deadbeef"); err == nil {
		t.Fatal("invalid signature must be rejected")
	}
}

func TestOutboxDispatcherDeliversSeededEvent(t *testing.T) {
	pool := testPool(t)
	w := outbox.NewWriter(pool)

	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	env, err := events.New(events.TopicUsageRecorded, "test", "usage-1", "11111111-1111-1111-1111-111111111111", "", map[string]any{"amount": 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Insert(context.Background(), tx, env); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}

	var delivered []string
	d := outbox.NewDispatcher(pool, func(_ context.Context, batch outbox.ClaimedBatch) error {
		for _, row := range batch.Rows {
			delivered = append(delivered, row.ID)
		}
		return nil
	}, 50*time.Millisecond, 10, 3)

	deadline := time.Now().Add(5 * time.Second)
	for len(delivered) == 0 && time.Now().Before(deadline) {
		_ = d.Tick(context.Background())
		time.Sleep(50 * time.Millisecond)
	}
	if len(delivered) == 0 {
		t.Fatal("dispatcher did not deliver the seeded event")
	}

	m, err := d.Metrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.Publishing != 0 || m.Failed != 0 {
		t.Fatalf("unexpected dispatcher metrics: %+v", m)
	}
}
