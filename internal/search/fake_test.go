package search

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOpenSearch is an in-memory OpenSearch speaking enough of the REST API
// for this package: index create, doc put, scripted update (the two painless
// scripts above), _search (with MANDATORY tenant_id term filter — a missing
// filter is a 400, so a service regression that drops the filter fails
// loudly), and _refresh. Deliberately simple relevance: every token of the
// simple_query_string must appear (case-insensitive) in at least one of
// search_text/customer_id/channel — deterministic for tests.
//
// It also doubles as a degradation simulator: setDown makes every endpoint
// answer 503, and holdOpen gates responses until release (a wedged backend).
type fakeOpenSearch struct {
	mu   sync.Mutex
	docs map[string]map[string]doc // index → doc id → document
	reqs []recordedReq

	down atomicBool
	gate chan struct{}

	srv *httptest.Server
	URL string
}

type recordedReq struct {
	Method string
	Path   string
	Body   string
}

type atomicBool struct {
	mu sync.Mutex
	v  bool
}

func (b *atomicBool) set(v bool) { b.mu.Lock(); b.v = v; b.mu.Unlock() }
func (b *atomicBool) get() bool  { b.mu.Lock(); defer b.mu.Unlock(); return b.v }

func newFakeOpenSearch(t *testing.T) *fakeOpenSearch {
	t.Helper()
	f := &fakeOpenSearch{docs: map[string]map[string]doc{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	f.URL = f.srv.URL
	return f
}

func (f *fakeOpenSearch) setDown(v bool) { f.down.set(v) }

// holdOpen wedges every data-path request (not index create/read) until
// release is called — simulates a hung backend without timing flakiness.
func (f *fakeOpenSearch) holdOpen() {
	f.mu.Lock()
	f.gate = make(chan struct{})
	f.mu.Unlock()
}

func (f *fakeOpenSearch) release() {
	f.mu.Lock()
	if f.gate != nil {
		close(f.gate)
		f.gate = nil
	}
	f.mu.Unlock()
}

func (f *fakeOpenSearch) serve(w http.ResponseWriter, r *http.Request) {
	if f.down.get() {
		writeFakeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "down"})
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	f.mu.Lock()
	f.reqs = append(f.reqs, recordedReq{Method: r.Method, Path: r.URL.Path, Body: string(body)})
	gate := f.gate
	f.mu.Unlock()
	if gate != nil && (strings.Contains(r.URL.Path, "/_doc/") ||
		strings.Contains(r.URL.Path, "/_update/") ||
		strings.Contains(r.URL.Path, "/_search")) {
		<-gate
	}

	rest := strings.TrimPrefix(r.URL.Path, "/")
	// split "<index>/<verb...>" or bare "<index>"
	index, verb := rest, ""
	if i := strings.Index(rest, "/"); i >= 0 {
		index, verb = rest[:i], rest[i+1:]
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.docs[index] == nil {
		f.docs[index] = map[string]doc{}
	}

	switch {
	case verb == "" && r.Method == http.MethodGet: // index exists?
		writeFakeJSON(w, http.StatusNotFound, map[string]any{"error": "index_not_found"})
	case verb == "" && r.Method == http.MethodPut: // create index
		writeFakeJSON(w, http.StatusOK, map[string]any{"acknowledged": true})
	case verb == "_refresh":
		writeFakeJSON(w, http.StatusOK, map[string]any{"_shards": map[string]int{"successful": 1}})
	case strings.HasPrefix(verb, "_doc/"):
		if r.Method != http.MethodPut {
			writeFakeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method"})
			return
		}
		id := strings.TrimPrefix(verb, "_doc/")
		var d doc
		if err := json.Unmarshal(body, &d); err != nil {
			writeFakeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_doc"})
			return
		}
		f.docs[index][id] = d
		writeFakeJSON(w, http.StatusOK, map[string]any{"_id": id})
	case strings.HasPrefix(verb, "_update/"):
		if r.Method != http.MethodPost {
			writeFakeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method"})
			return
		}
		f.update(index, strings.TrimPrefix(verb, "_update/"), body, w)
	case verb == "_search":
		if r.Method != http.MethodPost {
			writeFakeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method"})
			return
		}
		f.search(index, body, w)
	default:
		writeFakeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown_route"})
	}
}

func (f *fakeOpenSearch) update(index, id string, body []byte, w http.ResponseWriter) {
	var ub struct {
		Script *struct {
			Source string `json:"source"`
			Lang   string `json:"lang"`
			Params struct {
				LastInteraction *LastInteraction `json:"last_interaction"`
				SearchText      string           `json:"search_text"`
				At              time.Time        `json:"at"`
			} `json:"params"`
		} `json:"script"`
		Doc         map[string]any `json:"doc"`
		DocAsUpsert bool           `json:"doc_as_upsert"`
		Upsert      *doc           `json:"upsert"`
	}
	if err := json.Unmarshal(body, &ub); err != nil {
		writeFakeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_update"})
		return
	}
	existing, ok := f.docs[index][id]
	switch {
	case ub.Script != nil:
		if ok {
			p := ub.Script.Params
			if strings.Contains(ub.Script.Source, "interaction_count") {
				existing.InteractionCount++
			}
			existing.LastInteraction = p.LastInteraction
			existing.LastInteractionAt = &p.At
			existing.UpdatedAt = &p.At
			if p.SearchText != "" && !strings.Contains(existing.SearchText, p.SearchText) {
				existing.SearchText = strings.TrimSpace(existing.SearchText + " " + p.SearchText)
			}
			f.docs[index][id] = existing
		} else if ub.Upsert != nil {
			f.docs[index][id] = *ub.Upsert
		}
	case ub.Doc != nil:
		if ok {
			raw, _ := json.Marshal(ub.Doc)
			var patch doc
			_ = json.Unmarshal(raw, &patch)
			if patch.Status != "" {
				existing.Status = patch.Status
			}
			if patch.ClosedAt != nil {
				existing.ClosedAt = patch.ClosedAt
			}
			if patch.TenantID != "" {
				existing.TenantID = patch.TenantID
			}
			f.docs[index][id] = existing
		} else if ub.DocAsUpsert {
			raw, _ := json.Marshal(ub.Doc)
			var d doc
			_ = json.Unmarshal(raw, &d)
			f.docs[index][id] = d
		}
	default:
		writeFakeJSON(w, http.StatusBadRequest, map[string]any{"error": "empty_update"})
		return
	}
	writeFakeJSON(w, http.StatusOK, map[string]any{"_id": id})
}

func (f *fakeOpenSearch) search(index string, body []byte, w http.ResponseWriter) {
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
		writeFakeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad_query"})
		return
	}
	// Enforce the mandatory server-side tenant filter: no term on tenant_id
	// ⇒ 400. A service that stops sending the filter cannot silently leak
	// cross-tenant rows — it gets nothing back but an error.
	tenant := ""
	for _, clause := range qb.Query.Bool.Filter {
		if term, ok := clause["term"]; ok {
			if v, ok := term["tenant_id"].(string); ok {
				tenant = v
			}
		}
	}
	if tenant == "" {
		writeFakeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "mandatory tenant_id term filter missing"})
		return
	}
	q := ""
	for _, clause := range qb.Query.Bool.Must {
		if sqs, ok := clause["simple_query_string"]; ok {
			q, _ = sqs["query"].(string)
		}
	}

	size := qb.Size
	if size <= 0 {
		size = 10
	}
	tokens := strings.Fields(strings.ToLower(q))
	hits := []map[string]any{}
	ids := make([]string, 0, len(f.docs[index]))
	for id := range f.docs[index] {
		ids = append(ids, id)
	}
	sort.Strings(ids) // deterministic order (relevance is intentionally naive)
	for _, id := range ids {
		d := f.docs[index][id]
		if d.TenantID != tenant {
			continue // tenant isolation is enforced HERE, server-side
		}
		if !matchesAll(tokens, d) {
			continue
		}
		if len(hits) >= size {
			break
		}
		raw, _ := json.Marshal(d)
		var src map[string]any
		_ = json.Unmarshal(raw, &src)
		hits = append(hits, map[string]any{"_score": 1.0, "_source": src})
	}
	writeFakeJSON(w, http.StatusOK, map[string]any{
		"hits": map[string]any{
			"total": map[string]any{"value": len(hits)},
			"hits":  hits,
		},
	})
}

func matchesAll(tokens []string, d doc) bool {
	fields := []string{strings.ToLower(d.SearchText), strings.ToLower(d.CustomerID), strings.ToLower(d.Channel)}
	for _, tok := range tokens {
		found := false
		for _, f := range fields {
			if tok != "" && strings.Contains(f, tok) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (f *fakeOpenSearch) doc(index, id string) (doc, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.docs[index][id]
	return d, ok
}

func (f *fakeOpenSearch) docCount(index string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.docs[index])
}

// requests returns recorded requests (method+path) matching substr.
func (f *fakeOpenSearch) requests(substr string) []recordedReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recordedReq
	for _, r := range f.reqs {
		if strings.Contains(r.Path, substr) {
			out = append(out, r)
		}
	}
	return out
}

func writeFakeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
