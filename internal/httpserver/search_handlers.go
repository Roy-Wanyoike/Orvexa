package httpserver

// Search plane transport (issue #36): HTTP surface for conversation search.
//
// Self-contained by contract: MountSearchRoutes takes the router, the search
// service and a principal extractor, so the orchestrator can mount it inside
// the authenticated /api/v1 group (tenancy.AuthMiddleware attached) without
// touching server.go. Nothing else in this package needs to know search
// exists.
//
// Degradation principle (why this file looks the way it does): search is a
// read-model luxury — a search outage must never block the hot path. The
// indexer (internal/search) sheds events under backpressure instead of
// slowing publishers; this handler degrades ANSWERS, not availability:
//
//   - backend down/hung      → 503 {"error":{"code":"search.unavailable"}}
//     (per-request deadline in the service; the handler never wedges)
//   - search not configured  → 503 {"error":{"code":"search.not_configured"}}
//     — honest: the deployment says "search is off", it does not pretend
//   - validation failures    → 422 typed (search.query_required, …)
//
// Security model: the tenant filter is derived ONLY from the authenticated
// principal supplied by principalFn (populated by tenancy.AuthMiddleware).
// There is no tenant parameter on the wire at all — a client cannot express a
// cross-tenant search even by lying in the query string. Query strings are
// never logged.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/Roy-Wanyoike/orvexa/internal/platform/httpx"
	"github.com/Roy-Wanyoike/orvexa/internal/search"
	"github.com/Roy-Wanyoike/orvexa/internal/tenancy"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// MountSearchRoutes wires GET /search/conversations onto r. Call it inside
// the authenticated group (auth middleware present, principal in context):
//
//	MountSearchRoutes(authed, deps.Search, principal)
//
// svc may be nil — the route still answers, with the honest typed
// search.not_configured 503, because a missing deployment must look like a
// degraded capability, not a vanished endpoint.
func MountSearchRoutes(r chi.Router, svc *search.Service, principalFn func(*http.Request) *tenancy.Principal) {
	if principalFn == nil {
		principalFn = func(*http.Request) *tenancy.Principal { return nil }
	}
	r.Route("/search", func(sr chi.Router) {
		sr.Get("/conversations", h_searchConversations(svc, principalFn))
	})
}

func h_searchConversations(svc *search.Service, principalFn func(*http.Request) *tenancy.Principal) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principalFn(r)
		if p == nil || p.TenantID == "" {
			// AuthMiddleware guarantees a principal inside mounted trees; an
			// empty tenant here means the route was mounted outside it — fail
			// closed (401), never fall through to an unscoped query.
			httpx.WriteError(w, apperrors.Unauth("auth.missing_key", "authenticated principal with tenant required"))
			return
		}
		if svc == nil {
			// Not wired by the orchestrator: honest degradation, not a panic.
			writeSearchError(w, search.ErrNotConfigured)
			return
		}

		q := r.URL.Query().Get("q")
		limit := atoi(r.URL.Query().Get("limit")) // 0 → service applies DefaultLimit

		res, err := svc.Search(r.Context(), p.TenantID, q, limit)
		if err != nil {
			writeSearchError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, res, nil)
	}
}

// writeSearchError maps search-plane errors onto the wire. The canonical
// error envelope is preserved; the only special case is the "unavailable"
// kind, which pkg/errors does not know (it is declared inside internal/search
// to keep the taxonomy file untouched) and which must surface as 503 — the
// degradation contract callers program against.
func writeSearchError(w http.ResponseWriter, err error) {
	var appErr *apperrors.Error
	if errors.As(err, &appErr) && appErr.Kind == search.KindUnavailable {
		body := httpx.Envelope{Error: &httpx.EInfo{Code: appErr.Code, Message: appErr.Message}}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(body)
		return
	}
	httpx.WriteError(w, err) // invalid → 422, anything else → canonical mapping
}
