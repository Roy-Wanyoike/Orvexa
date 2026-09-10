package identity

import (
        "context"
        "net/http"
        "strconv"
        "strings"
        "time"

        "github.com/Roy-Wanyoike/orvexa/internal/platform/httpx"
        "github.com/Roy-Wanyoike/orvexa/internal/tenancy"
        apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

type ctxKey int

const ctxOIDCAuth ctxKey = 300

// Auth is the authenticated OIDC identity attached to the request context.
type Auth struct {
        Claims    *Claims
        Principal *tenancy.Principal
        Binding   Binding
}

// oidcAuthFrom extracts the OIDC identity (nil when the request authenticated
// via the API-key path or is unauthenticated).
func oidcAuthFrom(ctx context.Context) *Auth {
        a, _ := ctx.Value(ctxOIDCAuth).(*Auth)
        return a
}

// bearerToken returns the raw token from "Authorization: Bearer <token>".
// The X-API-Key header is NEVER an OIDC token — it belongs to the API-key
// path, which is delegated to untouched.
func bearerToken(r *http.Request) string {
        auth := r.Header.Get("Authorization")
        if len(auth) > 7 && strings.EqualFold(auth[:7], "bearer ") {
                return strings.TrimSpace(auth[7:])
        }
        return ""
}

// looksLikeJWT reports whether the token is shaped like a JWS compact
// serialization (three non-empty base64url segments). Only JWT-shaped
// credentials take the OIDC branch; anything else (orvx_ keys, opaque
// strings) flows to the API-key middleware exactly as before this wave.
func looksLikeJWT(token string) bool {
        parts := strings.Split(token, ".")
        if len(parts) != 3 {
                return false
        }
        for _, p := range parts {
                if p == "" {
                        return false
                }
        }
        return true
}

// DualAuth is the dual-authentication middleware: OIDC Bearer tokens
// (JWT-shaped) are verified by the identity plane; every other credential
// shape is delegated to the existing API-key middleware UNCHANGED (same
// handler, same errors, same rate limiting). Only when OIDC is enabled is
// this wrapper present in the chain at all.
//
// Branch selection:
//   - no Authorization: Bearer header → API-key path (X-API-Key or missing).
//   - Bearer, not JWT-shaped        → API-key path (legacy orvx_ keys).
//   - Bearer, JWT-shaped            → OIDC verification; failure is FINAL
//     (401 identity.invalid_token) — a forged JWT is never retried as an
//     API key.
func (s *Service) DualAuth(apiKeyAuth func(http.Handler) http.Handler, limiter *httpx.RateLimit) func(http.Handler) http.Handler {
        return func(next http.Handler) http.Handler {
                // Pre-built API-key chain: byte-identical delegation for the
                // legacy path (constructed once, per mount, as chi does today).
                apiNext := apiKeyAuth(next)
                return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                        token := bearerToken(r)
                        if token == "" || !looksLikeJWT(token) {
                                apiNext.ServeHTTP(w, r)
                                return
                        }
                        claims, err := s.Verify(r.Context(), token)
                        if err != nil {
                                httpx.WriteError(w, err)
                                return
                        }
                        principal, binding, err := s.Principal(r.Context(), claims)
                        if err != nil {
                                httpx.WriteError(w, err)
                                return
                        }
                        // per-principal rate limiting on the OIDC path mirrors the
                        // per-key limiter on the API-key path (bounded buckets).
                        if limiter != nil {
                                if ok, retry := limiter.Allow("oidc:"+claims.Subject, time.Now()); !ok {
                                        w.Header().Set("Retry-After", retryAfterSeconds(retry))
                                        httpx.WriteError(w, apperrors.RateLimited("identity.rate_limited",
                                                "rate limit exceeded; retry later"))
                                        return
                                }
                        }
                        ctx := tenancy.WithPrincipal(r.Context(), principal)
                        ctx = tenancy.WithIdentityRef(ctx, tenancy.IdentityRef{
                                Method:   tenancy.AuthMethodOIDC,
                                Subject:  claims.Subject,
                                TenantID: principal.TenantID,
                        })
                        ctx = context.WithValue(ctx, ctxOIDCAuth, &Auth{Claims: claims, Principal: principal, Binding: binding})
                        next.ServeHTTP(w, r.WithContext(ctx))
                })
        }
}

// RequireCapability enforces one capability (`interaction.read`, …) for
// OIDC principals. API-key principals remain governed by their minted scope
// set: capability-style scopes pass, and the legacy blanket scopes ("api",
// "*") keep historical keys working byte-identically (they already passed
// the requiredScope gate upstream). The tenant always derives from the
// authenticated principal, so a cross-tenant claim cannot buy capabilities:
// bindings are resolved per (tenant, subject) and an unmatched pair yields
// an empty set → 403.
func RequireCapability(capability string) func(http.Handler) http.Handler {
        return func(next http.Handler) http.Handler {
                return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                        p, ok := tenancy.PrincipalFrom(r.Context())
                        if !ok {
                                // Defense in depth: this middleware only mounts inside the
                                // authenticated tree; a missing principal is a 401.
                                httpx.WriteError(w, apperrors.Unauth("auth.missing_key", "missing API key"))
                                return
                        }
                        if oidcAuthFrom(r.Context()) != nil {
                                if !p.HasCapability(capability) {
                                        httpx.WriteError(w, capabilityMissing(capability))
                                        return
                                }
                        } else if !p.HasCapability(capability) &&
                                !p.HasScope("api") && !p.HasScope("*") {
                                httpx.WriteError(w, capabilityMissing(capability))
                                return
                        }
                        next.ServeHTTP(w, r)
                })
        }
}

// RequireMethodCapability enforces the read/write capability split by HTTP
// verb at mount granularity: GET/HEAD/OPTIONS demand the read capability,
// every other method the write capability. This lets the v1 wiring gate a
// whole resource additively (one .With(...) around the existing mount call)
// without editing per-route registrations inside the mount helpers.
func RequireMethodCapability(read, write Capability) func(http.Handler) http.Handler {
        readNext := RequireCapability(string(read))
        writeNext := RequireCapability(string(write))
        return func(next http.Handler) http.Handler {
                readH, writeH := readNext(next), writeNext(next)
                return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                        switch r.Method {
                        case http.MethodGet, http.MethodHead, http.MethodOptions:
                                readH.ServeHTTP(w, r)
                        default:
                                writeH.ServeHTTP(w, r)
                        }
                })
        }
}

// retryAfterSeconds renders the Retry-After header. Rounded UP: a client
// retrying exactly at the advertised second must find a token waiting —
// understating the wait would let hammering clients retry early forever.
func retryAfterSeconds(d time.Duration) string {
        s := int(math.Ceil(d.Seconds()))
        if s < 1 {
                s = 1
        }
        return strconv.Itoa(s)
}
