// Package search owns conversation search (issue #36 [O-27]): an event-driven
// OpenSearch indexer (indexer.go) and a tenant-scoped query service (this
// file). It talks to OpenSearch over plain REST with net/http only — no
// client dependency.
//
// Degradation principle (the reason this package exists in this shape):
// search is a read-model luxury. A search outage must never block the hot
// path (ingest → persist → route). Therefore:
//
//   - The indexer consumes the bus through a bounded queue and SHEDS events
//     (drop + structured log) when OpenSearch is slow or down — publishers
//     are never blocked, memory is bounded by the queue capacity.
//   - The query path answers 503 with a typed error when the backend is
//     unreachable; callers (agent desktop) render "search unavailable" and
//     the platform keeps taking calls.
//   - Without ORVEXA_OPENSEARCH_URL the service is still constructible and
//     reports the honest typed error search.not_configured — search is off,
//     not silently broken.
//
// Security model: every query is filtered SERVER-SIDE by an exact term on
// tenant_id, and that tenant always comes from the authenticated principal
// (never a query parameter — see the HTTP handler). Query strings are never
// logged and never echoed into logs or errors.
package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// KindUnavailable extends the application error taxonomy with 503 semantics
// for the search plane. pkg/errors is outside this package's exclusive file
// scope, so the kind is declared here and mapped to 503 by the transport
// layer (internal/httpserver/search_handlers.go).
const KindUnavailable = apperrors.Kind("unavailable")

// ErrNotConfigured is returned (and surfaced as 503 search.not_configured)
// when ORVEXA_OPENSEARCH_URL is unset. Honest degradation: the deployment
// says "search is off" instead of pretending.
var ErrNotConfigured = apperrors.New(KindUnavailable, "search.not_configured",
	"search is not configured on this deployment")

// unavailable wraps a backend failure into the canonical 503 surface
// (search.unavailable) while keeping the cause private for server logs.
func unavailable(cause error) *apperrors.Error {
	return apperrors.New(KindUnavailable, "search.unavailable",
		"search is temporarily unavailable").WithCause(cause)
}

// IndexConversations is the projection index for conversation search. Every
// document carries a tenant_id keyword field — the mandatory filter term.
const IndexConversations = "orvexa-conversations"

// Query/size caps (defense in depth; the handler enforces them too).
const (
	DefaultLimit = 25
	MaxLimit     = 50
	MaxQueryLen  = 256
)

// EnvURL is the environment variable that gates the search plane.
const EnvURL = "ORVEXA_OPENSEARCH_URL"

// Logger is the structured logging surface (same shape as httpx.Logger and
// slog sugar): msg plus key/value pairs. Call sites in this package never
// pass query strings or payload bodies.
type Logger func(msg string, args ...any)

func noopLogger(string, ...any) {}

// Config configures the OpenSearch-backed service.
type Config struct {
	URL     string       // e.g. http://localhost:9200 — from EnvURL
	Index   string       // defaults to IndexConversations
	Client  *http.Client // optional; defaults to a 3s-timeout client
	Timeout time.Duration
	Logger  Logger // optional structured logger
}

// NewServiceFromEnv builds a Config from the environment (EnvURL, optional
// ORVEXA_OPENSEARCH_INDEX). Unset URL ⇒ Configured()==false and the service
// returns ErrNotConfigured — constructible but honest.
func NewServiceFromEnv() Config {
	return Config{URL: os.Getenv(EnvURL), Index: os.Getenv("ORVEXA_OPENSEARCH_INDEX")}
}

// Service serves tenant-scoped conversation search over OpenSearch REST.
type Service struct {
	cfg    Config
	client *http.Client
	log    Logger
}

// NewService constructs the service. It never fails (issue #36: the service
// must be constructible even when search is not configured).
func NewService(cfg Config) *Service {
	if cfg.Index == "" {
		cfg.Index = IndexConversations
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 3 * time.Second
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout}
	}
	log := cfg.Logger
	if log == nil {
		log = noopLogger
	}
	return &Service{cfg: cfg, client: client, log: log}
}

// Configured reports whether a backend URL is present.
func (s *Service) Configured() bool { return strings.TrimSpace(s.cfg.URL) != "" }

// Hit is one projected conversation document, shaped for the search UI.
type Hit struct {
	ConversationID   string           `json:"conversation_id"`
	CustomerID       string           `json:"customer_id,omitempty"`
	Channel          string           `json:"channel,omitempty"`
	Status           string           `json:"status,omitempty"`
	InteractionCount int              `json:"interaction_count,omitempty"`
	LastInteraction  *LastInteraction `json:"last_interaction,omitempty"`
	OpenedAt         *time.Time       `json:"opened_at,omitempty"`
	UpdatedAt        *time.Time       `json:"updated_at,omitempty"`
	Score            float64          `json:"score"`
}

// LastInteraction is the projected latest interaction on a conversation.
type LastInteraction struct {
	ID        string `json:"id,omitempty"`
	Channel   string `json:"channel,omitempty"`
	Direction string `json:"direction,omitempty"`
	Status    string `json:"status,omitempty"`
}

// Result is the query outcome.
type Result struct {
	Total int64 `json:"total"`
	Limit int   `json:"limit"`
	Hits  []Hit `json:"hits"`
}

// Search runs a tenant-scoped conversation query.
//
// The tenant filter is MANDATORY and SERVER-SIDE: tenantID is baked into the
// OpenSearch bool query as an exact term filter before anything else. It is
// never taken from user-controlled input at this layer (the HTTP handler
// passes the authenticated principal's tenant; the API has no tenant
// parameter at all).
func (s *Service) Search(ctx context.Context, tenantID, q string, limit int) (*Result, error) {
	if !s.Configured() {
		return nil, ErrNotConfigured
	}
	if strings.TrimSpace(tenantID) == "" {
		return nil, apperrors.Invalid("search.tenant_required", "authenticated tenant is required")
	}
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, apperrors.Invalid("search.query_required", "query parameter q is required")
	}
	if len(q) > MaxQueryLen {
		return nil, apperrors.Invalid("search.query_too_long", "query must be at most 256 characters")
	}
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit // size cap: a query can never ask for unbounded rows
	}

	body := map[string]any{
		"size": limit,
		"query": map[string]any{
			"bool": map[string]any{
				// tenant isolation: exact keyword term derived from the
				// authenticated principal — never from request input
				"filter": []any{map[string]any{"term": map[string]any{"tenant_id": tenantID}}},
				"must": []any{map[string]any{"simple_query_string": map[string]any{
					"query":            q,
					"fields":           []string{"search_text", "customer_id", "channel"},
					"default_operator": "and",
				}}},
			},
		},
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, unavailable(err)
	}

	// Per-request deadline: a hung backend degrades to 503, never to a stuck
	// handler (hot-path principle).
	reqCtx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	endpoint := strings.TrimSuffix(s.cfg.URL, "/") + "/" + s.cfg.Index + "/_search"
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, unavailable(err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, unavailable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, unavailable(fmt.Errorf("opensearch returned status %d", resp.StatusCode))
	}

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, unavailable(err)
	}
	var parsed struct {
		Hits struct {
			Total struct {
				Value int64 `json:"value"`
			} `json:"total"`
			Hits []struct {
				Score  float64 `json:"_score"`
				Source doc     `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return nil, unavailable(err)
	}

	res := &Result{Total: parsed.Hits.Total.Value, Limit: limit, Hits: make([]Hit, 0, len(parsed.Hits.Hits))}
	for _, h := range parsed.Hits.Hits {
		res.Hits = append(res.Hits, Hit{
			ConversationID:   h.Source.ConversationID,
			CustomerID:       h.Source.CustomerID,
			Channel:          h.Source.Channel,
			Status:           h.Source.Status,
			InteractionCount: h.Source.InteractionCount,
			LastInteraction:  h.Source.LastInteraction,
			OpenedAt:         h.Source.OpenedAt,
			UpdatedAt:        h.Source.UpdatedAt,
			Score:            h.Score,
		})
	}
	return res, nil
}
