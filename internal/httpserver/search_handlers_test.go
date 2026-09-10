package httpserver

// Transport tests for the search plane (issue #36): envelope shape, tenant
// derivation from the authenticated principal (never a query parameter), and
// the degradation contract (503 typed on outage / not-configured).
//
// The fake backend here is deliberately minimal — it only has to speak the
// slice of the OpenSearch REST surface the service uses (_search with a
// MANDATORY tenant_id term filter, _doc writes for seeding). Deeper indexing
// semantics are covered by internal/search's own fake, which enforces the
// same filter tripwire.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Roy-Wanyoike/orvexa/internal/platform/httpx"
	"github.com/Roy-Wanyoike/orvexa/internal/search"
	"github.com/Roy-Wanyoike/orvexa/internal/tenancy"
)

// ---- minimal OpenSearch stand-in ----

type searchFake struct {
	srv  *httptest.Server
	URL  string
	docs map[string]map[string]map[string]any // index → doc id → _source
	down bool

	queries []string // recorded tenant term values per _search body
}

func newSearchFake(t *testing.T) *searchFake {
	t.Helper()
	f := &searchFake{docs: map[string]map[string]map[string]any{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	f.URL = f.srv.URL
	return f
}

func (f *searchFake) seed(t *testing.T, tenant, convID, customer, channel string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"tenant_id": tenant, "conversation_id": convID,
		"customer_id": customer, "channel": channel, "status": "open",
		"search_text": strings.ToLower(channel + " " + customer),
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPut, f.URL+"/orvexa-conversations/_doc/"+convID, strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("seed status %d", resp.StatusCode)
	}
}

func (f *searchFake) serve(w http.ResponseWriter, r *http.Request) {
	if f.down {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "down"})
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	switch {
	case strings.HasSuffix(r.URL.Path, "/_search") && r.Method == http.MethodPost:
		f.search(w, body)
	case strings.HasPrefix(r.URL.Path, "/orvexa-conversations/_doc/") && r.Method == http.MethodPut:
		var src map[string]any
		if err := json.Unmarshal(body, &src); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/orvexa-conversations/_doc/")
		if f.docs["orvexa-conversations"] == nil {
			f.docs["orvexa-conversations"] = map[string]map[string]any{}
		}
		f.docs["orvexa-conversations"][id] = src
		writeFakeSearchJSON(w, http.StatusOK, map[string]any{"_id": id})
	default:
		writeFakeSearchJSON(w, http.StatusNotFound, map[string]any{"error": "unknown_route"})
	}
}

// search enforces the same tripwire as the internal/search fake: a query
// without the server-side tenant_id term filter is a hard 400, so a service
// regression cannot silently leak cross-tenant rows.
func (f *searchFake) search(w http.ResponseWriter, body []byte) {
	var qb struct {
		Size  int `json:"size"`
		Query struct {
			Bool struct {
				Filter []map[string]map[string]any `json:"filter"`
				Must   []map[string]map[string]any `json:"must"`
			} `json:"bool"`
		} `json:"query"`
	}
	if err := json.Unmarshal(body, &qb); err != nil {
		writeFakeSearchJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_query"})
		return
	}
	tenant := ""
	for _, clause := range qb.Query.Bool.Filter {
		if term, ok := clause["term"]; ok {
			if v, ok := term["tenant_id"].(string); ok {
				tenant = v
			}
		}
	}
	if tenant == "" {
		writeFakeSearchJSON(w, http.StatusBadRequest, map[string]any{
			"error": "mandatory tenant_id term filter missing"})
		return
	}
	f.queries = append(f.queries, tenant)

	q := ""
	for _, clause := range qb.Query.Bool.Must {
		if sqs, ok := clause["simple_query_string"]; ok {
			q, _ = sqs["query"].(string)
		}
	}
	tokens := strings.Fields(strings.ToLower(q))
	hits := []map[string]any{}
	for _, id := range sortedIDs(f.docs["orvexa-conversations"]) {
		src := f.docs["orvexa-conversations"][id]
		if src["tenant_id"] != tenant {
			continue
		}
		if !fakeMatches(tokens, src) {
			continue
		}
		if len(hits) >= qb.Size {
			break
		}
		hits = append(hits, map[string]any{"_score": 1.0, "_source": src})
	}
	writeFakeSearchJSON(w, http.StatusOK, map[string]any{
		"hits": map[string]any{"total": map[string]any{"value": len(hits)}, "hits": hits},
	})
}

func sortedIDs(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func fakeMatches(tokens []string, src map[string]any) bool {
	hay := strings.ToLower(src["search_text"].(string) + " " + src["customer_id"].(string) + " " + src["channel"].(string))
	for _, tok := range tokens {
		if tok != "" && !strings.Contains(hay, tok) {
			return false
		}
	}
	return true
}

func writeFakeSearchJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// ---- harness ----

func searchRouter(svc *search.Service) http.Handler {
	r := chi.NewRouter()
	MountSearchRoutes(r, svc, func(r *http.Request) *tenancy.Principal {
		return &tenancy.Principal{APIKeyID: "key-1", TenantID: "tenant-a", Scopes: []string{"*"}}
	})
	return r
}

func decodeEnvelope(t *testing.T, resp *http.Response) httpx.Envelope {
	t.Helper()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	var env httpx.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope decode: %v (%s)", err, raw)
	}
	return env
}

// ---- tests ----

func TestSearchHandlerTenantIsolationAndEnvelope(t *testing.T) {
	f := newSearchFake(t)
	f.seed(t, "tenant-a", "conv-1", "cust-1", "voice")
	f.seed(t, "tenant-b", "conv-2", "cust-2", "voice")

	svc := search.NewService(search.Config{URL: f.URL, Timeout: 2 * time.Second})
	srv := httptest.NewServer(searchRouter(svc))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/search/conversations?q=" + url.QueryEscape("voice cust-1") + "&limit=10")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	env := decodeEnvelope(t, resp)
	if env.Error != nil {
		t.Fatalf("unexpected error envelope: %+v", env.Error)
	}
	data, _ := env.Data.(map[string]any)
	if data == nil || data["total"].(float64) != 1 {
		t.Fatalf("data = %+v, want exactly 1 hit", env.Data)
	}
	hits, _ := data["hits"].([]any)
	if len(hits) != 1 {
		t.Fatalf("hits = %+v", hits)
	}
	hit := hits[0].(map[string]any)
	if hit["conversation_id"] != "conv-1" {
		t.Fatalf("hit = %+v, want conv-1 (tenant-a's own document)", hit)
	}

	// cross-tenant query: the token only exists in tenant-b's document —
	// tenant-a's authenticated search must return NOTHING.
	respX, err := srv.Client().Get(srv.URL + "/search/conversations?q=cust-2")
	if err != nil {
		t.Fatal(err)
	}
	defer respX.Body.Close()
	envX := decodeEnvelope(t, respX)
	dataX, _ := envX.Data.(map[string]any)
	if dataX == nil || dataX["total"].(float64) != 0 {
		t.Fatalf("cross-tenant leak: %+v", envX.Data)
	}

	// SECURITY PROOF: every backend query carried the SERVER-SIDE tenant
	// filter derived from the principal (tenant-a), and the wire format has
	// no tenant parameter a client could abuse.
	if len(f.queries) == 0 {
		t.Fatal("no search queries reached the backend")
	}
	for _, tenant := range f.queries {
		if tenant != "tenant-a" {
			t.Fatalf("backend query filtered for %q, want tenant-a only", tenant)
		}
	}
}

func TestSearchHandlerLimitCapAndDefault(t *testing.T) {
	f := newSearchFake(t)
	svc := search.NewService(search.Config{URL: f.URL})
	srv := httptest.NewServer(searchRouter(svc))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/search/conversations?q=voice&limit=1000")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	env := decodeEnvelope(t, resp)
	data, _ := env.Data.(map[string]any)
	if data == nil || data["limit"].(float64) != float64(search.MaxLimit) {
		t.Fatalf("limit = %+v, want capped at %d", env.Data, search.MaxLimit)
	}

	resp2, err := srv.Client().Get(srv.URL + "/search/conversations?q=voice")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	env2 := decodeEnvelope(t, resp2)
	data2, _ := env2.Data.(map[string]any)
	if data2 == nil || data2["limit"].(float64) != float64(search.DefaultLimit) {
		t.Fatalf("limit = %+v, want default %d", env2.Data, search.DefaultLimit)
	}
}

func TestSearchHandlerValidation(t *testing.T) {
	f := newSearchFake(t)
	svc := search.NewService(search.Config{URL: f.URL})
	srv := httptest.NewServer(searchRouter(svc))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/search/conversations")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("missing q status = %d, want 422", resp.StatusCode)
	}
	env := decodeEnvelope(t, resp)
	if env.Error == nil || env.Error.Code != "search.query_required" {
		t.Fatalf("error envelope = %+v, want search.query_required", env.Error)
	}
}

func TestSearchHandlerBackendDown503(t *testing.T) {
	f := newSearchFake(t)
	svc := search.NewService(search.Config{URL: f.URL, Timeout: 2 * time.Second})

	// degraded backend → 503 with the typed code (degradation contract)
	f.down = true
	srv := httptest.NewServer(searchRouter(svc))
	resp, err := srv.Client().Get(srv.URL + "/search/conversations?q=voice")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (degradation principle)", resp.StatusCode)
	}
	env := decodeEnvelope(t, resp)
	if env.Error == nil || env.Error.Code != "search.unavailable" {
		t.Fatalf("error envelope = %+v, want search.unavailable", env.Error)
	}

	// unreachable backend → same typed 503
	dead := search.NewService(search.Config{URL: "http://127.0.0.1:1", Timeout: 200 * time.Millisecond})
	srv2 := httptest.NewServer(searchRouter(dead))
	resp2, err := srv2.Client().Get(srv2.URL + "/search/conversations?q=voice")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("dead backend status = %d, want 503", resp2.StatusCode)
	}
	env2 := decodeEnvelope(t, resp2)
	if env2.Error == nil || env2.Error.Code != "search.unavailable" {
		t.Fatalf("dead backend envelope = %+v, want search.unavailable", env2.Error)
	}
}

func TestSearchHandlerNotConfigured(t *testing.T) {
	// unconfigured service (no URL) → honest typed 503
	svc := search.NewService(search.Config{})
	srv := httptest.NewServer(searchRouter(svc))
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL + "/search/conversations?q=voice")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	env := decodeEnvelope(t, resp)
	if env.Error == nil || env.Error.Code != "search.not_configured" {
		t.Fatalf("error envelope = %+v, want search.not_configured", env.Error)
	}

	// nil service (orchestrator did not wire search) → same honest answer
	srv2 := httptest.NewServer(searchRouter(nil))
	defer srv2.Close()
	resp2, err := srv2.Client().Get(srv2.URL + "/search/conversations?q=voice")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("nil service status = %d, want 503", resp2.StatusCode)
	}
	env2 := decodeEnvelope(t, resp2)
	if env2.Error == nil || env2.Error.Code != "search.not_configured" {
		t.Fatalf("nil service envelope = %+v, want search.not_configured", env2.Error)
	}
}

func TestSearchHandlerRequiresPrincipal(t *testing.T) {
	f := newSearchFake(t)
	svc := search.NewService(search.Config{URL: f.URL})
	r := chi.NewRouter()
	MountSearchRoutes(r, svc, nil) // no principal source: fail closed
	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/search/conversations?q=voice")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (fail closed without principal)", resp.StatusCode)
	}
	if len(f.queries) != 0 {
		t.Fatal("a principal-less request must never reach the backend")
	}
}
