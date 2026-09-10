//go:build integration

// Integration test against a REAL OpenSearch (issue #36 testing requirements).
// Integration-tagged and gated on the environment, skipping cleanly when no
// backend is advertised:
//
//	ORVEXA_TEST_OPENSEARCH_URL=http://localhost:9200 \
//	  go test -race -tags=integration ./internal/search/...
//
// ORVEXA_OPENSEARCH_URL (the runtime gate the indexer itself uses) is honored
// as a fallback so the same env contract drives both the service and this test:
//
//	ORVEXA_OPENSEARCH_URL=http://localhost:9200 go test -race -tags=integration ./internal/search/...
package search

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Roy-Wanyoike/orvexa/internal/platform/bus"
)

func TestOpenSearchIntegration(t *testing.T) {
	url := os.Getenv("ORVEXA_TEST_OPENSEARCH_URL")
	if url == "" {
		url = os.Getenv(EnvURL) // ORVEXA_OPENSEARCH_URL
	}
	if url == "" {
		t.Skip("neither ORVEXA_TEST_OPENSEARCH_URL nor ORVEXA_OPENSEARCH_URL set; skipping OpenSearch integration test")
	}

	svc := NewService(Config{URL: url, Timeout: 10 * time.Second, Logger: func(string, ...any) {}})
	ix := NewIndexer(svc, IndexerConfig{Logger: func(string, ...any) {}})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	theBus := bus.NewInProc()
	defer theBus.Close()
	if err := ix.Start(ctx, theBus); err != nil {
		t.Fatalf("indexer start: %v", err)
	}
	defer ix.Close()

	opened, err := eventsConversationOpened("it-tenant", "conv-it-1", "cust-it-1", "voice")
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	created, err := eventsInteractionCreated("it-tenant", "int-it-1", "conv-it-1", "voice", "inbound", "active")
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	ix.project(ctx, opened)
	ix.project(ctx, created)
	if err := ix.refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	res, err := svc.Search(ctx, "it-tenant", "voice inbound", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if res.Total != 1 || len(res.Hits) != 1 || res.Hits[0].ConversationID != "conv-it-1" {
		t.Fatalf("hits = %+v, want conv-it-1", res.Hits)
	}

	// tenant isolation against the real engine
	resX, err := svc.Search(ctx, "other-tenant", "voice inbound", 10)
	if err != nil {
		t.Fatalf("cross-tenant search: %v", err)
	}
	if resX.Total != 0 {
		t.Fatalf("cross-tenant leak: %+v", resX.Hits)
	}

	// degradation: kill reachability by pointing at an unreachable port —
	// the typed 503-class error must surface, never a hang.
	dead := NewService(Config{URL: "http://127.0.0.1:1", Timeout: 500 * time.Millisecond})
	if _, err := dead.Search(ctx, "it-tenant", "voice", 10); err == nil {
		t.Fatal("expected unavailable error for dead backend")
	}
}
