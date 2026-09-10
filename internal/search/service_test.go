package search

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

func testService(f *fakeOpenSearch) *Service {
	return NewService(Config{URL: f.URL, Logger: func(string, ...any) {}})
}

func seedConversation(f *fakeOpenSearch, tenant, convID, customerID, channel string) {
	svc := testService(f)
	ix := NewIndexer(svc, IndexerConfig{})
	ctx := context.Background()
	ix.project(ctx, mustEnv(eventsConversationOpened(tenant, convID, customerID, channel)))
}

func TestSearchTenantIsolation(t *testing.T) {
	f := newFakeOpenSearch(t)
	seedConversation(f, "tenant-a", "conv-1", "cust-1", "voice")
	seedConversation(f, "tenant-b", "conv-2", "cust-2", "voice")

	svc := testService(f)
	ctx := context.Background()

	// tenant-a sees exactly its own document
	res, err := svc.Search(ctx, "tenant-a", "voice", 10)
	if err != nil {
		t.Fatalf("search tenant-a: %v", err)
	}
	if res.Total != 1 || len(res.Hits) != 1 || res.Hits[0].ConversationID != "conv-1" {
		t.Fatalf("tenant-a hits = %+v, want exactly conv-1", res.Hits)
	}

	// tenant-b sees exactly its own document
	resB, err := svc.Search(ctx, "tenant-b", "voice", 10)
	if err != nil {
		t.Fatalf("search tenant-b: %v", err)
	}
	if resB.Total != 1 || len(resB.Hits) != 1 || resB.Hits[0].ConversationID != "conv-2" {
		t.Fatalf("tenant-b hits = %+v, want exactly conv-2", resB.Hits)
	}

	// cross-tenant query returns NOTHING (even when the term only exists in
	// the other tenant's documents)
	for _, q := range []string{"cust-2", "conv-2", "voice cust-2"} {
		resX, err := svc.Search(ctx, "tenant-a", q, 10)
		if err != nil {
			t.Fatalf("cross-tenant search %q: %v", q, err)
		}
		if resX.Total != 0 || len(resX.Hits) != 0 {
			t.Fatalf("cross-tenant leak: query %q returned %+v", q, resX.Hits)
		}
	}

	// SECURITY PROOF: every search request carries the server-side term
	// filter on tenant_id, derived from the caller (never request input).
	for _, req := range f.requests("_search") {
		var body map[string]any
		if err := json.Unmarshal([]byte(req.Body), &body); err != nil {
			t.Fatalf("recorded body not json: %v", err)
		}
		if !hasTenantTerm(body, "tenant-a") && !hasTenantTerm(body, "tenant-b") {
			t.Fatalf("search request without mandatory tenant_id filter: %s", req.Body)
		}
	}
}

// TestFakeRejectsMissingTenantFilter is the tripwire for service regressions:
// the fake enforces the mandatory filter contract server-side.
func TestFakeRejectsMissingTenantFilter(t *testing.T) {
	f := newFakeOpenSearch(t)
	resp, err := f.srv.Client().Post(f.URL+"/orvexa-conversations/_search", "application/json",
		strings.NewReader(`{"query":{"bool":{"must":[{"match_all":{}}]}}}`))
	if err != nil {
		t.Fatalf("raw search: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("filterless search status = %d, want 400", resp.StatusCode)
	}
}

func hasTenantTerm(body map[string]any, want string) bool {
	query, _ := body["query"].(map[string]any)
	boolQ, _ := query["bool"].(map[string]any)
	filters, _ := boolQ["filter"].([]any)
	for _, f := range filters {
		clause, _ := f.(map[string]any)
		term, _ := clause["term"].(map[string]any)
		if v, _ := term["tenant_id"].(string); v == want {
			return true
		}
	}
	return false
}

func TestSearchNotConfigured(t *testing.T) {
	svc := NewService(Config{}) // ORVEXA_OPENSEARCH_URL unset equivalent
	_, err := svc.Search(context.Background(), "tenant-a", "voice", 10)
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
	var appErr *apperrors.Error
	if !errors.As(err, &appErr) || appErr.Kind != KindUnavailable || appErr.Code != "search.not_configured" {
		t.Fatalf("typed error mismatch: %+v", err)
	}
}

func TestSearchBackendDown503(t *testing.T) {
	f := newFakeOpenSearch(t)
	svc := testService(f)
	seedConversation(f, "tenant-a", "conv-1", "cust-1", "voice")

	// degraded backend → typed 503-class error
	f.setDown(true)
	_, err := svc.Search(context.Background(), "tenant-a", "voice", 10)
	if !isSearchUnavailable(err) {
		t.Fatalf("err = %v, want search.unavailable", err)
	}

	// transport-level failure (dead server) → same typed error
	dead := NewService(Config{URL: "http://127.0.0.1:1", Timeout: 200 * time.Millisecond})
	_, err = dead.Search(context.Background(), "tenant-a", "voice", 10)
	if !isSearchUnavailable(err) {
		t.Fatalf("dead backend err = %v, want search.unavailable", err)
	}
}

func isSearchUnavailable(err error) bool {
	var appErr *apperrors.Error
	return errors.As(err, &appErr) && appErr.Kind == KindUnavailable && appErr.Code == "search.unavailable"
}

func TestSearchValidationAndSizeCaps(t *testing.T) {
	f := newFakeOpenSearch(t)
	svc := testService(f)
	ctx := context.Background()

	if _, err := svc.Search(ctx, "tenant-a", "   ", 10); err == nil ||
		appErrCode(err) != "search.query_required" {
		t.Fatalf("empty q err = %v", err)
	}
	if _, err := svc.Search(ctx, "tenant-a", strings.Repeat("x", MaxQueryLen+1), 10); err == nil ||
		appErrCode(err) != "search.query_too_long" {
		t.Fatalf("long q err = %v", err)
	}
	if _, err := svc.Search(ctx, "", "voice", 10); err == nil ||
		appErrCode(err) != "search.tenant_required" {
		t.Fatalf("empty tenant err = %v", err)
	}

	res, err := svc.Search(ctx, "tenant-a", "voice", 1000)
	if err != nil {
		t.Fatalf("capped search: %v", err)
	}
	if res.Limit != MaxLimit {
		t.Fatalf("limit = %d, want size cap %d", res.Limit, MaxLimit)
	}
	res, err = svc.Search(ctx, "tenant-a", "voice", 0)
	if err != nil || res.Limit != DefaultLimit {
		t.Fatalf("default limit = %d err = %v, want %d", res.Limit, err, DefaultLimit)
	}
}

func appErrCode(err error) string {
	var appErr *apperrors.Error
	if errors.As(err, &appErr) {
		return appErr.Code
	}
	return ""
}

func TestSearchHitShape(t *testing.T) {
	f := newFakeOpenSearch(t)
	svc := testService(f)
	ctx := context.Background()
	ix := NewIndexer(svc, IndexerConfig{})
	ix.project(ctx, mustEnv(eventsConversationOpened("tenant-a", "conv-9", "cust-9", "chat")))
	ix.project(ctx, mustEnv(eventsInteractionCreated("tenant-a", "int-1", "conv-9", "chat", "inbound", "active")))
	if err := ix.refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	res, err := svc.Search(ctx, "tenant-a", "chat inbound", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if res.Total != 1 || len(res.Hits) != 1 {
		t.Fatalf("hits = %+v", res.Hits)
	}
	h := res.Hits[0]
	if h.ConversationID != "conv-9" || h.CustomerID != "cust-9" || h.Channel != "chat" || h.Status != "open" {
		t.Fatalf("hit fields = %+v", h)
	}
	if h.InteractionCount != 1 || h.LastInteraction == nil || h.LastInteraction.Direction != "inbound" {
		t.Fatalf("interaction projection = %+v", h)
	}
}
