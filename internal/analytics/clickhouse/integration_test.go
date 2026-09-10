//go:build integration

// Integration test against a REAL ClickHouse (issue #35 acceptance evidence:
// "Integration test green against compose ClickHouse"). Integration-tagged and
// gated on the environment, skipping cleanly when no backend is advertised:
//
//	ORVEXA_TEST_CLICKHOUSE_URL=clickhouse://default@127.0.0.1:19000/default \
//	  go test -race -tags=integration ./internal/analytics/clickhouse
//
// ORVEXA_CLICKHOUSE_URL (the runtime gate the store itself uses) is honored as
// a fallback so the same env contract drives both the service and this test:
//
//	ORVEXA_CLICKHOUSE_URL=clickhouse://default@127.0.0.1:19000/default \
//	  go test -race -tags=integration ./internal/analytics/clickhouse
//
// The compose devstack (docker-compose.dev.yml) applies
// migrations/0011_clickhouse_facts.sql on first init of the clickhouse service;
// this test re-executes that same file defensively (all statements are CREATE
// TABLE IF NOT EXISTS — idempotent) so a bare server is enough.
package clickhouse

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"

	"github.com/Roy-Wanyoike/orvexa/pkg/events"
)

// applyFactsDDL executes the engine-specific migration file against conn.
// It mirrors the compose init mount: comment lines are stripped, the two
// idempotent CREATE TABLE statements are run one by one.
func applyFactsDDL(t *testing.T, conn driver.Conn) {
	t.Helper()
	raw, err := os.ReadFile("../../../migrations/0011_clickhouse_facts.sql")
	if err != nil {
		t.Fatalf("read migrations/0011_clickhouse_facts.sql: %v", err)
	}
	var b strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	for _, stmt := range strings.Split(b.String(), ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if err := conn.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("apply DDL statement: %v\nstatement: %s", err, stmt)
		}
	}
}

func countFacts(t *testing.T, conn driver.Conn, query string, tenant uuid.UUID) uint64 {
	t.Helper()
	var n uint64
	if err := conn.QueryRow(context.Background(), query, tenant).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return n
}

func TestClickHouseFactsIntegration(t *testing.T) {
	url := os.Getenv("ORVEXA_TEST_CLICKHOUSE_URL")
	if url == "" {
		url = os.Getenv(EnvURL) // ORVEXA_CLICKHOUSE_URL
	}
	if url == "" {
		t.Skip("neither ORVEXA_TEST_CLICKHOUSE_URL nor ORVEXA_CLICKHOUSE_URL set; skipping ClickHouse integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// A dedicated verification connection: the store under test owns its own.
	opts, err := clickhouse.ParseDSN(url)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	verify, err := clickhouse.Open(opts)
	if err != nil {
		t.Fatalf("open verify connection: %v", err)
	}
	defer func() { _ = verify.Close() }()

	// Defensive: the same idempotent DDL the compose init mount applies.
	applyFactsDDL(t, verify)

	// The store under test — real dial, small knobs so the cadence path also
	// runs; Close() performs the final flush this test asserts on.
	store, err := New(Options{
		URL:            url,
		MaxBatchRows:   10,
		FlushInterval:  50 * time.Millisecond,
		WriteTimeout:   5 * time.Second,
		EnqueueTimeout: 2 * time.Second,
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tenant := uuid.New() // per-run tenant: reruns never collide with old data
	inter := uuid.New()
	when := time.Now().UTC().Truncate(time.Hour)

	type fact struct {
		eventID            string
		channel, direction string
		status             string
	}
	facts := []fact{
		{uuid.NewString(), "voice", "inbound", "completed"},
		{uuid.NewString(), "chat", "inbound", "active"},
	}
	for _, f := range facts {
		env := &events.Envelope{
			ID:       f.eventID,
			Type:     events.TopicInteractionCreated,
			Source:   "integration-test",
			Subject:  inter.String(),
			TenantID: tenant.String(),
			Time:     when,
			Data:     map[string]any{"channel": f.channel, "direction": f.direction, "status": f.status},
		}
		if err := store.Handle(ctx, env); err != nil {
			t.Fatalf("handle interaction %s: %v", f.channel, err)
		}
	}
	for i := 1; i <= 2; i++ {
		if err := store.Handle(ctx, &events.Envelope{
			ID:       uuid.NewString(),
			Type:     events.TopicUsageRecorded,
			Source:   "integration-test",
			TenantID: tenant.String(),
			Time:     when,
			Data:     map[string]any{"metric": "tokens", "output_tokens": float64(21 * i)},
		}); err != nil {
			t.Fatalf("handle usage %d: %v", i, err)
		}
	}

	// At-least-once redelivery of the voice interaction envelope: identical
	// event id, identical timestamp and payload — the exact byte-identical
	// row the DDL header promises ReplacingMergeTree will collapse.
	redelivery := &events.Envelope{
		ID:       facts[0].eventID,
		Type:     events.TopicInteractionCreated,
		Source:   "integration-test",
		Subject:  inter.String(),
		TenantID: tenant.String(),
		Time:     when,
		Data:     map[string]any{"channel": facts[0].channel, "direction": facts[0].direction, "status": facts[0].status},
	}
	if err := store.Handle(ctx, redelivery); err != nil {
		t.Fatalf("handle redelivery: %v", err)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("close (final flush): %v", err)
	}

	// Raw row count includes the redelivery duplicate: 3 rows were appended.
	// A background merge may legally already have collapsed them to 2, so
	// only the range is asserted here; FINAL below is the strong guarantee.
	raw := countFacts(t, verify,
		"SELECT count() FROM interaction_fact WHERE tenant_id = ?", tenant)
	if raw < 2 || raw > 3 {
		t.Fatalf("interaction_fact raw count = %d, want 2 or 3 (2 distinct + 1 redelivery, pre/post merge)", raw)
	}
	// FINAL collapses the redelivery — the DDL's sort key dedupes per event.
	if got := countFacts(t, verify,
		"SELECT count() FROM interaction_fact FINAL WHERE tenant_id = ?", tenant); got != 2 {
		t.Fatalf("interaction_fact FINAL count = %d, want 2 (redelivery collapsed)", got)
	}

	// Summary-aggregation shape over the LowCardinality group-by columns.
	rows, err := verify.Query(ctx,
		`SELECT channel, status, count() FROM interaction_fact
		 WHERE tenant_id = ? GROUP BY channel, status ORDER BY channel`, tenant)
	if err != nil {
		t.Fatalf("summary group-by: %v", err)
	}
	defer rows.Close()
	type bucket struct {
		channel, status string
		n               uint64
	}
	var got []bucket
	for rows.Next() {
		var b bucket
		if err := rows.Scan(&b.channel, &b.status, &b.n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, b)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(got) != 2 ||
		got[0] != (bucket{"chat", "active", 1}) ||
		got[1] != (bucket{"voice", "completed", 1}) {
		t.Fatalf("grouped facts = %+v, want chat/active=1 and voice/completed=1", got)
	}

	// Usage facts: two rows, amounts 21+42, FINAL-stable.
	if got := countFacts(t, verify,
		"SELECT count() FROM usage_fact FINAL WHERE tenant_id = ?", tenant); got != 2 {
		t.Fatalf("usage_fact FINAL count = %d, want 2", got)
	}
	var sum int64
	if err := verify.QueryRow(ctx,
		"SELECT sum(amount) FROM usage_fact WHERE tenant_id = ?", tenant).Scan(&sum); err != nil {
		t.Fatalf("sum(amount): %v", err)
	}
	if sum != 63 {
		t.Fatalf("sum(amount) = %d, want 63", sum)
	}

	t.Logf("evidence: tenant=%s interaction_fact FINAL=2 (raw %d incl. redelivery), usage_fact FINAL=2 sum(amount)=63", tenant, raw)
}
