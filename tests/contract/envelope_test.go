package contract

// Envelope conformance (issue #41): the wire format is {data, meta} on
// success and {error:{code, message, details?}} on failure — exactly what
// internal/platform/httpx writes and what the spec's info block declares.
// These table tests pin the helper output, the pkg/errors kind→status
// mapping, one full-stack error sample, and the /search/conversations
// envelope (including the O-27 degradation 503 semantics) against the
// schema the spec declares for it.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Roy-Wanyoike/orvexa/internal/httpserver"
	"github.com/Roy-Wanyoike/orvexa/internal/platform/httpx"
	"github.com/Roy-Wanyoike/orvexa/internal/search"
	"github.com/Roy-Wanyoike/orvexa/internal/tenancy"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// decodeEnvelopeKeys parses an envelope body into generic maps (NOT the
// httpx.Envelope struct — decoding into the transport's own types would hide
// drift between the wire and the schema; a map shows exactly what was sent).
func decodeEnvelopeKeys(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("envelope is not a JSON object: %v (body=%q)", err, rec.Body.String())
	}
	return body
}

func keySet(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func sameMembers(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]bool{}
	for _, g := range got {
		seen[g] = true
	}
	for _, w := range want {
		if !seen[w] {
			return false
		}
	}
	return true
}

func TestEnvelopeShapeTable(t *testing.T) {
	cases := []struct {
		name        string
		write       func(w http.ResponseWriter)
		wantStatus  int
		wantTopKeys []string
		wantErrKeys []string
	}{
		{
			name: "success data+meta",
			write: func(w http.ResponseWriter) {
				httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": "c1"}, map[string]string{"next_cursor": "cur"})
			},
			wantStatus: http.StatusOK, wantTopKeys: []string{"data", "meta"},
		},
		{
			name: "success data only (meta omitted)",
			write: func(w http.ResponseWriter) {
				httpx.WriteJSON(w, http.StatusCreated, map[string]any{"id": "c1"}, nil)
			},
			wantStatus: http.StatusCreated, wantTopKeys: []string{"data"},
		},
		{
			name: "success meta only (data omitted)",
			write: func(w http.ResponseWriter) {
				httpx.WriteJSON(w, http.StatusOK, nil, map[string]string{"next_cursor": "cur"})
			},
			wantStatus: http.StatusOK, wantTopKeys: []string{"meta"},
		},
		{
			name: "error without details",
			write: func(w http.ResponseWriter) {
				httpx.WriteError(w, apperrors.Invalid("customer.invalid", "bad input"))
			},
			wantStatus: http.StatusUnprocessableEntity, wantTopKeys: []string{"error"},
			wantErrKeys: []string{"code", "message"},
		},
		{
			name: "error with details",
			write: func(w http.ResponseWriter) {
				httpx.WriteError(w, apperrors.Forbidden("identity.capability_missing", "nope").
					WithDetails(map[string]string{"required_capability": "case.write"}))
			},
			wantStatus: http.StatusForbidden, wantTopKeys: []string{"error"},
			wantErrKeys: []string{"code", "message", "details"},
		},
		{
			name: "unknown error → opaque 500 (no internals leak)",
			write: func(w http.ResponseWriter) {
				httpx.WriteError(w, errors.New("secret connection string leaked"))
			},
			wantStatus: http.StatusInternalServerError, wantTopKeys: []string{"error"},
			wantErrKeys: []string{"code", "message"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.write(rec)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("content-type = %q, want application/json", ct)
			}
			body := decodeEnvelopeKeys(t, rec)
			if got := keySet(body); !sameMembers(got, tc.wantTopKeys) {
				t.Fatalf("top-level keys = %v, want exactly %v", got, tc.wantTopKeys)
			}
			if tc.wantErrKeys != nil {
				errObj, _ := body["error"].(map[string]any)
				if errObj == nil {
					t.Fatalf("error is not an object: %v", body["error"])
				}
				if got := keySet(errObj); !sameMembers(got, tc.wantErrKeys) {
					t.Fatalf("error keys = %v, want exactly %v", got, tc.wantErrKeys)
				}
			}
			// no cross-contamination between the two envelope faces
			if tc.wantErrKeys != nil {
				if _, has := body["data"]; has {
					t.Fatal("error envelope must never carry data")
				}
				if _, has := body["meta"]; has {
					t.Fatal("error envelope must never carry meta")
				}
			} else if _, has := body["error"]; has {
				t.Fatal("success envelope must never carry error")
			}
		})
	}
}

func TestErrorKindStatusMapping(t *testing.T) {
	cases := []struct {
		kind apperrors.Kind
		want int
	}{
		{apperrors.KindInvalid, http.StatusUnprocessableEntity},
		{apperrors.KindNotFound, http.StatusNotFound},
		{apperrors.KindConflict, http.StatusConflict},
		{apperrors.KindRateLimited, http.StatusTooManyRequests},
		{apperrors.KindUnauth, http.StatusUnauthorized},
		{apperrors.KindForbidden, http.StatusForbidden},
		{apperrors.KindInternal, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		httpx.WriteError(rec, apperrors.New(tc.kind, "kind.probe", "status mapping probe"))
		if rec.Code != tc.want {
			t.Fatalf("kind %s → %d, want %d", tc.kind, rec.Code, tc.want)
		}
	}
}

// TestWireErrorEnvelopeOnAuthedRoutes: one full-stack sample — the assembled
// contract router answers an unauthenticated request with the canonical
// error envelope and nothing else (spec: security applies to every authed
// route; the shape matches the error face of the envelope contract).
func TestWireErrorEnvelopeOnAuthedRoutes(t *testing.T) {
	h := contractRouter()
	for _, path := range []string{"/api/v1/customers", "/api/v1/interactions/x", "/api/v1/cases/x/notes"} {
		rec := doRequest(h, http.MethodGet, path)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s = %d, want 401", path, rec.Code)
		}
		body := decodeEnvelopeKeys(t, rec)
		if got := keySet(body); !sameMembers(got, []string{"error"}) {
			t.Fatalf("%s: top-level keys = %v, want [error]", path, got)
		}
		errObj, _ := body["error"].(map[string]any)
		if got := keySet(errObj); !sameMembers(got, []string{"code", "message"}) {
			t.Fatalf("%s: error keys = %v, want [code message]", path, got)
		}
		if errObj["code"] != "auth.missing_key" {
			t.Fatalf("%s: error.code = %v, want auth.missing_key", path, errObj["code"])
		}
	}
}

// ---- minimal OpenSearch stand-in (same slice of the REST surface the
// service uses as internal/httpserver's transport tests) ----

type contractSearchFake struct {
	docs map[string][]map[string]any // tenant → sources
	down bool
}

func (f *contractSearchFake) serve(w http.ResponseWriter, r *http.Request) {
	if f.down {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if !strings.HasSuffix(r.URL.Path, "/_search") || r.Method != http.MethodPost {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var qb struct {
		Size  int `json:"size"`
		Query struct {
			Bool struct {
				Filter []map[string]map[string]any `json:"filter"`
			} `json:"bool"`
		} `json:"query"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&qb)
	tenant := ""
	for _, clause := range qb.Query.Bool.Filter {
		if term, ok := clause["term"]; ok {
			tenant, _ = term["tenant_id"].(string)
		}
	}
	hits := []map[string]any{}
	total := 0
	for _, src := range f.docs[tenant] {
		total++
		if len(hits) < qb.Size {
			hits = append(hits, map[string]any{"_score": 1.5, "_source": src})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"hits": map[string]any{"total": map[string]any{"value": total}, "hits": hits},
	})
}

// searchHarnessRouter mounts the search plane exactly the way MountV1 mounts
// it inside the authenticated group (internal/httpserver test pattern): the
// principal comes from the context-equivalent source, so the handler's
// envelope — success AND the typed degradation 503s — is what's under test.
func searchHarnessRouter(svc *search.Service) http.Handler {
	r := chi.NewRouter()
	httpserver.MountSearchRoutes(r, svc, func(*http.Request) *tenancy.Principal {
		return &tenancy.Principal{APIKeyID: "key-1", TenantID: "tenant-a", Scopes: []string{"*"}}
	})
	return r
}

// TestSearchEnvelopeConformance: the O-27 wire sample — success shape against
// the spec's declared data properties, and the degradation contract (typed
// 503s) that the spec's 503 line promises.
func TestSearchEnvelopeConformance(t *testing.T) {
	spec := MustLoadSpec(t)

	if got := spec.OKDataProperties("/search/conversations", "GET"); !got["total"] || !got["limit"] || !got["hits"] {
		t.Fatalf("spec search 200 data properties = %v, want total+limit+hits", got)
	}
	hitProps := spec.HitProperties("/search/conversations", "GET")
	if !hitProps["conversation_id"] || !hitProps["score"] || !hitProps["last_interaction"] {
		t.Fatalf("spec search hit properties = %v, missing core fields", hitProps)
	}

	// seeded backend → 200 envelope
	fake := &contractSearchFake{docs: map[string][]map[string]any{
		"tenant-a": {{
			"tenant_id": "tenant-a", "conversation_id": "conv-1", "customer_id": "cust-1",
			"channel": "voice", "status": "open", "search_text": "angry billing customer",
			"interaction_count": 3,
			"opened_at":         time.Now().UTC().Format(time.RFC3339Nano),
		}},
	}}
	bd := httptest.NewServer(http.HandlerFunc(fake.serve))
	defer bd.Close()

	svc := search.NewService(search.Config{URL: bd.URL, Timeout: 2 * time.Second})
	handler := searchHarnessRouter(svc)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/search/conversations?q=billing&limit=10")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, raw)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("envelope not JSON: %v", err)
	}
	if got := keySet(body); !sameMembers(got, []string{"data"}) {
		t.Fatalf("top-level keys = %v, want exactly [data] (meta omitted when nil)", got)
	}
	data, _ := body["data"].(map[string]any)
	declared := spec.OKDataProperties("/search/conversations", "GET")
	for k := range data {
		if !declared[k] {
			t.Fatalf("wire data key %q is NOT declared in the spec 200 schema (drift)", k)
		}
	}
	for _, want := range []string{"total", "limit", "hits"} {
		if _, ok := data[want]; !ok {
			t.Fatalf("wire data missing declared key %q", want)
		}
	}
	if limit, _ := data["limit"].(float64); limit != 10 {
		t.Fatalf("data.limit = %v, want the requested 10 echoed back", data["limit"])
	}
	hits, _ := data["hits"].([]any)
	if len(hits) != 1 {
		t.Fatalf("hits = %v, want 1", hits)
	}
	hit := hits[0].(map[string]any)
	if hit["conversation_id"] != "conv-1" {
		t.Fatalf("hit = %v, want conv-1 (tenant-a's own document)", hit)
	}
	for k := range hit {
		if !hitProps[k] {
			t.Fatalf("wire hit key %q is NOT declared in the spec hit schema (drift)", k)
		}
	}

	// missing q → documented 422 (Error422 ref), typed code search.query_required
	rec422 := doRequest(handler, http.MethodGet, "/search/conversations")
	if rec422.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing q = %d, want 422 (spec documents '422')", rec422.Code)
	}
	e422, _ := decodeEnvelopeKeys(t, rec422)["error"].(map[string]any)
	if e422 == nil || e422["code"] != "search.query_required" {
		t.Fatalf("422 envelope = %v, want error.code=search.query_required", e422)
	}

	// unconfigured backend → honest typed 503 (spec names search.not_configured)
	unconfigured := search.NewService(search.Config{})
	unHandler := searchHarnessRouter(unconfigured)
	rec503 := doRequest(unHandler, http.MethodGet, "/search/conversations?q=x")
	if rec503.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured = %d, want 503 (degradation principle)", rec503.Code)
	}
	e503, _ := decodeEnvelopeKeys(t, rec503)["error"].(map[string]any)
	if e503 == nil || e503["code"] != "search.not_configured" {
		t.Fatalf("unconfigured envelope = %v, want search.not_configured", e503)
	}

	// dead backend → typed 503 search.unavailable (spec names it too)
	dead := search.NewService(search.Config{URL: "http://127.0.0.1:1", Timeout: 200 * time.Millisecond})
	deadHandler := searchHarnessRouter(dead)
	recDead := doRequest(deadHandler, http.MethodGet, "/search/conversations?q=x")
	if recDead.Code != http.StatusServiceUnavailable {
		t.Fatalf("dead backend = %d, want 503", recDead.Code)
	}
	eDead, _ := decodeEnvelopeKeys(t, recDead)["error"].(map[string]any)
	if eDead == nil || eDead["code"] != "search.unavailable" {
		t.Fatalf("dead backend envelope = %v, want search.unavailable", eDead)
	}

	// and the spec's 503 line must keep naming BOTH degradation codes
	desc, ok := spec.ResponseDescription("/search/conversations", "GET", "503")
	if !ok {
		t.Fatal("spec lost the 503 response on /search/conversations")
	}
	for _, code := range []string{"search.unavailable", "search.not_configured"} {
		if !strings.Contains(desc, code) {
			t.Fatalf("spec 503 description %q does not name %q", desc, code)
		}
	}
}
