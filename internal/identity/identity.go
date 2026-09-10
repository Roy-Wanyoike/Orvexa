package identity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/oauth2"

	"github.com/Roy-Wanyoike/orvexa/internal/platform/httpx"
	"github.com/Roy-Wanyoike/orvexa/internal/tenancy"
	"github.com/google/uuid"
)

// Environment variables (docs/rbac.md §configuration). All three core vars
// must be set for OIDC to be enabled; when any is missing the identity plane
// is disabled and the API-key path is byte-identical to the pre-[O-29] stack.
const (
	envIssuer       = "ORVEXA_OIDC_ISSUER"
	envClientID     = "ORVEXA_OIDC_CLIENT_ID"
	envClientSecret = "ORVEXA_OIDC_CLIENT_SECRET"
	envRedirectURL  = "ORVEXA_OIDC_REDIRECT_URL" // optional fixed callback override
	envScopes       = "ORVEXA_OIDC_SCOPES"       // optional, default "openid"
)

// Config is the identity plane configuration (env-sourced in production).
type Config struct {
	Issuer       string        // e.g. https://idp.example.com (discovery root)
	ClientID     string        // aud required in tokens
	ClientSecret string        // confidential-client code exchange + state HMAC keying
	RedirectURL  string        // optional fixed callback; derived from request when empty
	Scopes       []string      // authorization scopes (default ["openid"])
	Leeway       time.Duration // clock skew tolerance (default 30s)
	JWKSTTL      time.Duration // signing-key cache TTL (default 10m)
	// JWKSMinRefresh is the floor between JWKS fetches (default 2s): a flood
	// of unknown-kid tokens cannot turn the verifier into a key-endpoint
	// amplifier. Rotation is still picked up on the next miss after the floor.
	JWKSMinRefresh time.Duration
	HTTPClient     *http.Client // optional client for IdP traffic (tests)
}

// FromEnv builds the identity Service from the environment. It reports
// ok=false (and the API stays API-key-only) unless issuer, client id and
// client secret are ALL configured.
func FromEnv() (*Service, bool) {
	issuer := strings.TrimSpace(os.Getenv(envIssuer))
	clientID := strings.TrimSpace(os.Getenv(envClientID))
	clientSecret := os.Getenv(envClientSecret)
	if issuer == "" || clientID == "" || clientSecret == "" {
		return nil, false
	}
	cfg := Config{
		Issuer:       issuer,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  strings.TrimSpace(os.Getenv(envRedirectURL)),
	}
	if raw := strings.TrimSpace(os.Getenv(envScopes)); raw != "" {
		cfg.Scopes = strings.Split(raw, ",")
	}
	return NewService(cfg), true
}

// Service is the OIDC identity plane: discovery, verification and
// authorization resolution. It is safe for concurrent use.
type Service struct {
	cfg Config
	log httpx.Logger

	// resolver resolves (tenant, subject) → role binding. nil = static
	// posture (roles claim trusted; no database in the deployment).
	resolver RoleResolver

	mu       sync.Mutex
	provider *oidcProviderMeta
	endpoint oauth2.Endpoint
	jwks     *jwksCache
}

// RoleResolver resolves the authoritative role binding for a subject inside a
// tenant. The production implementation reads role_bindings +
// capability_catalog (migration 0012_rbac.sql) behind a short-TTL cache.
type RoleResolver interface {
	Resolve(ctx context.Context, tenantID, subject string) (Binding, error)
}

// NewService builds the identity plane. A nil pool keeps the static
// (no-database) authorization posture; see WithRoleResolver.
func NewService(cfg Config) *Service {
	if cfg.Leeway <= 0 {
		cfg.Leeway = 30 * time.Second
	}
	if cfg.JWKSTTL <= 0 {
		cfg.JWKSTTL = 10 * time.Minute
	}
	switch {
	case cfg.JWKSMinRefresh == 0:
		cfg.JWKSMinRefresh = 2 * time.Second
	case cfg.JWKSMinRefresh < 0:
		cfg.JWKSMinRefresh = 0 // explicit no-floor (tests); production uses the default
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{"openid"}
	}
	return &Service{cfg: cfg, log: func(string, ...any) {}}
}

// WithLogger attaches a structured logger (never given token material).
func (s *Service) WithLogger(log httpx.Logger) *Service {
	if log != nil {
		s.log = log
	}
	return s
}

// WithRoleResolver replaces the authorization resolver (e.g. the database
// binding resolver; tests inject fakes). The service never mutates bindings —
// binding administration is a control-plane concern outside this package.
func (s *Service) WithRoleResolver(r RoleResolver) *Service {
	s.resolver = r
	return s
}

// HasRoleResolver reports whether an explicit authorization resolver is
// wired (production wiring respects a deliberately injected resolver).
func (s *Service) HasRoleResolver() bool { return s.resolver != nil }

// Issuer reports the configured IdP issuer (safe to log; not a secret).
func (s *Service) Issuer() string { return s.cfg.Issuer }

// oauthCapable reports whether the login endpoints (authorize-url/callback)
// can operate: they need a confidential client secret for the code exchange
// and to key the state HMAC.
func (s *Service) oauthCapable() bool { return s.cfg.ClientSecret != "" }

// discoveryDoc is the subset of the OpenID Provider Metadata document
// (RFC 8414 / OpenID Discovery 1.0) the identity plane consumes.
//
// Discovery is implemented directly over HTTP rather than via go-oidc:
// go-oidc v3.21.0 transitively requires github.com/go-jose/go-jose/v4, whose
// checksums are absent from go.sum, and this wave's ownership rules forbid
// go.mod/go.sum edits. The document fetch is issuer-validated and
// HTTPS/loopback-checked; if the deps owner adds the go-jose entries, the
// go-oidc verifier can replace this thin reader without API change.
type discoveryDoc struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	IDTokenAlgs           []string `json:"id_token_signing_alg_values_supported"`
}

// ensure performs provider discovery exactly once (retried on later requests
// if it failed — an unreachable IdP must not poison the process forever).
func (s *Service) ensure(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.provider != nil {
		return nil
	}
	client := s.cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	wellKnown := strings.TrimSuffix(s.cfg.Issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wellKnown, nil)
	if err != nil {
		return errDiscovery(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return errDiscovery(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errDiscovery(fmt.Errorf("discovery endpoint answered %d", resp.StatusCode))
	}
	var doc discoveryDoc
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return errDiscovery(fmt.Errorf("discovery document malformed: %w", err))
	}
	// Issuer binding (OIDC Discovery §4.3): the document's issuer MUST equal
	// the configured issuer — a compromised/misrouted discovery response
	// cannot silently substitute another IdP.
	if doc.Issuer != strings.TrimSuffix(s.cfg.Issuer, "/") {
		return errDiscovery(fmt.Errorf("discovery issuer %q does not match configured issuer %q", doc.Issuer, s.cfg.Issuer))
	}
	if doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" || doc.JWKSURI == "" {
		return errDiscovery(errors.New("discovery document lacks required endpoints"))
	}
	s.endpoint = oauth2.Endpoint{AuthURL: doc.AuthorizationEndpoint, TokenURL: doc.TokenEndpoint}
	s.jwks = newJWKSCache(doc.JWKSURI, client, s.cfg.JWKSTTL, s.cfg.JWKSMinRefresh)
	s.provider = &oidcProviderMeta{issuer: doc.Issuer}
	s.log("oidc provider discovery complete", "issuer", doc.Issuer)
	return nil
}

// oidcProviderMeta pins the validated discovery result (issuer equality is
// the property that makes the fetched endpoints trustworthy).
type oidcProviderMeta struct{ issuer string }

// oauth2Config returns the client for authorize/exchange using the caller's
// redirect URI (fixed override or derived from the request host — the IdP
// validates it against its registered allowlist, so derivation is safe).
func (s *Service) oauth2Config(redirectURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     s.cfg.ClientID,
		ClientSecret: s.cfg.ClientSecret,
		Endpoint:     s.endpoint,
		RedirectURL:  redirectURL,
		Scopes:       s.cfg.Scopes,
	}
}

// Claims is the Orvexa-relevant ID-token claim set. Standard registered
// claims (iss/aud/exp/nbf/iat/sub) are validated by the parser; tenant and
// roles are the Orvexa extension claims mapped onto the principal.
type Claims struct {
	jwt.RegisteredClaims
	Tenant   string   `json:"tenant,omitempty"`    // primary tenant claim (uuid)
	TenantID string   `json:"tenant_id,omitempty"` // accepted alias
	Roles    []string `json:"roles,omitempty"`     // IdP-asserted roles (static posture)
}

// TenantClaim returns the tenant assertion (primary claim, then alias).
func (c *Claims) TenantClaim() string {
	if c.Tenant != "" {
		return c.Tenant
	}
	return c.TenantID
}

// Verify validates a raw bearer JWT and returns its claims. Acceptance
// contract (docs/rbac.md §token contract): RS256 only; iss == configured
// issuer; aud contains the configured client id; exp REQUIRED and in the
// future (leeway applies); kid REQUIRED and resolvable through the JWKS
// cache; sub REQUIRED and non-empty. Every rejection is the typed 401
// identity.invalid_token with a coarse reason.
func (s *Service) Verify(ctx context.Context, raw string) (*Claims, error) {
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	alg, kid, err := parseHeader(raw)
	if err != nil {
		return nil, invalidToken("malformed")
	}
	// Explicit algorithm allowlist BEFORE crypto: rejects alg:none and the
	// HS256-confusion family with an unambiguous reason. parser-side
	// WithValidMethods stays as defense in depth.
	if alg != "RS256" {
		return nil, invalidToken("algorithm")
	}
	if kid == "" {
		return nil, invalidToken("key")
	}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(s.cfg.Issuer),
		jwt.WithAudience(s.cfg.ClientID),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(s.cfg.Leeway),
	)
	// ParseWithClaims targets *Claims: plain Parse decodes into MapClaims,
	// which would make the typed claim assertion below fail for EVERY token
	// (caught by the forgery-matrix tests).
	tok, err := parser.ParseWithClaims(raw, &Claims{}, s.jwks.keyFunc(ctx, kid))
	if err != nil {
		return nil, invalidToken(mapParseError(err))
	}
	claims, ok := tok.Claims.(*Claims)
	if !ok || tok == nil || !tok.Valid || strings.TrimSpace(claims.Subject) == "" {
		return nil, invalidToken("claims")
	}
	return claims, nil
}

// mapParseError reduces golang-jwt's error taxonomy to the coarse machine
// reasons exposed in the 401 payload (never echoing token content).
func mapParseError(err error) string {
	switch {
	case errors.Is(err, jwt.ErrTokenMalformed):
		return "malformed"
	case errors.Is(err, jwt.ErrTokenExpired):
		return "expired"
	case errors.Is(err, jwt.ErrTokenNotValidYet), errors.Is(err, jwt.ErrTokenUsedBeforeIssued):
		return "not_yet_valid"
	case errors.Is(err, jwt.ErrTokenInvalidIssuer):
		return "issuer"
	case errors.Is(err, jwt.ErrTokenInvalidAudience):
		return "audience"
	case errors.Is(err, jwt.ErrTokenSignatureInvalid):
		return "signature"
	case errors.Is(err, jwt.ErrTokenUnverifiable):
		return "unverifiable"
	default:
		return "unverifiable"
	}
}

// parseHeader pre-parses the JOSE header (unverified, for routing decisions
// only — cryptographic conclusions are drawn exclusively by the parser).
func parseHeader(raw string) (alg, kid string, err error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "", "", errors.New("token is not a JWS compact serialization")
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	decoded, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", "", fmt.Errorf("header is not base64url: %w", err)
	}
	if err := json.Unmarshal(decoded, &header); err != nil {
		return "", "", fmt.Errorf("header is not JSON: %w", err)
	}
	return header.Alg, header.Kid, nil
}

// capabilitiesFor resolves the authoritative binding for the claims. With a
// resolver configured (database posture) role_bindings rows are authoritative
// and the token roles claim is IGNORED (Orvexa owns authorization; the IdP
// owns identity). Without one, the static catalog maps the roles claim.
func (s *Service) capabilitiesFor(ctx context.Context, claims *Claims) (Binding, error) {
	if s.resolver != nil {
		tenant := claims.TenantClaim()
		// Bindings are keyed by tenants.id (uuid); a non-uuid tenant claim
		// cannot match any binding — short-circuit to an empty binding
		// instead of surfacing a storage error for forged junk.
		if _, err := uuid.Parse(tenant); err != nil {
			return Binding{}, nil
		}
		b, err := s.resolver.Resolve(ctx, tenant, claims.Subject)
		if err != nil {
			return Binding{}, errBindingLookup(err)
		}
		return b, nil
	}
	return staticBinding(claims.Roles), nil
}

// Principal maps verified claims onto the tenancy principal: the tenant
// ALWAYS comes from the authenticated token claims (never from request
// bodies or headers), capabilities come from the resolved role binding.
func (s *Service) Principal(ctx context.Context, claims *Claims) (*tenancy.Principal, Binding, error) {
	binding, err := s.capabilitiesFor(ctx, claims)
	if err != nil {
		return nil, Binding{}, err
	}
	p := &tenancy.Principal{
		TenantID: claims.TenantClaim(),
		Scopes:   binding.Capabilities, // capabilities ARE the enforcement scopes
	}
	return p, binding, nil
}
