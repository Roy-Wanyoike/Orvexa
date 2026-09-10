package identity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Roy-Wanyoike/orvexa/internal/platform/httpx"
	"github.com/Roy-Wanyoike/orvexa/internal/tenancy"
)

// probe captures what the enforcement chain delivered to the handler.
type probe struct {
	hit       bool
	principal *tenancy.Principal
	ref       tenancy.IdentityRef
	hasRef    bool
	oidc      *Auth
}

func probeHandler(p *probe) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.hit = true
		p.principal, _ = tenancy.PrincipalFrom(r.Context())
		p.ref, p.hasRef = tenancy.IdentityRefFrom(r.Context())
		p.oidc = oidcAuthFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})
}

func getReq(target string) *http.Request { return httptest.NewRequest(http.MethodGet, target, nil) }

// api-key stub: the stand-in for tenancy.AuthMiddleware that injects the
// principal an API-key authentication would produce.
func stubAPIKeyAuth(scopes *[]string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := &tenancy.Principal{APIKeyID: "key-1", TenantID: tenantA, Scopes: *scopes}
			next.ServeHTTP(w, r.WithContext(tenancy.WithPrincipal(r.Context(), p)))
		})
	}
}

// ---- DualAuth branch selection ----

func TestDualAuthNonJWTCredentialsTakeAPIKeyPath(t *testing.T) {
	idp := newTestIdP(t)
	svc := idp.service(nil)
	p := &probe{}
	scopes := []string{"api"}
	h := svc.DualAuth(stubAPIKeyAuth(&scopes), httpx.NewRateLimit(1000, 1000, 10))(probeHandler(p))

	for _, tc := range []struct {
		name    string
		headers map[string]string
	}{
		{"no credentials", nil},
		{"X-API-Key", map[string]string{"X-API-Key": "orvx_raw"}},
		{"bearer api key", map[string]string{"Authorization": "Bearer orvx_raw"}},
		{"opaque bearer", map[string]string{"Authorization": "Bearer opaque-opaque"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := getReq("/x")
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			p.hit = false
			h.ServeHTTP(httptest.NewRecorder(), r)
			if !p.hit {
				t.Fatal("API-key middleware must handle non-JWT credentials")
			}
			if p.principal == nil || p.principal.APIKeyID != "key-1" {
				t.Fatal("API-key principal must flow through unchanged")
			}
			if p.oidc != nil {
				t.Fatal("API-key path must not carry OIDC auth")
			}
		})
	}
}

func TestDualAuthJWTTakesOIDCPath(t *testing.T) {
	idp := newTestIdP(t)
	svc := idp.service(nil).WithRoleResolver(staticResolver{tenantA + "|" + testSubject: agentBinding()})
	p := &probe{}
	scopes := []string{"api"}
	h := svc.DualAuth(stubAPIKeyAuth(&scopes), httpx.NewRateLimit(1000, 1000, 10))(probeHandler(p))

	raw := mustMintRS(t, idp.rootKey(), testKid1, baseClaims(idp.url()))
	req := getReq("/x")
	req.Header.Set("Authorization", "Bearer "+raw)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !p.hit {
		t.Fatal("valid OIDC token must reach the handler")
	}
	if p.principal == nil || !p.principal.HasCapability(string(CapInteractionWrite)) {
		t.Fatalf("OIDC principal must carry binding capabilities, got %+v", p.principal)
	}
	if !p.hasRef || p.ref.Method != tenancy.AuthMethodOIDC || p.ref.Subject != testSubject || p.ref.TenantID != tenantA {
		t.Fatalf("identity provenance wrong: %+v (hasRef=%v)", p.ref, p.hasRef)
	}
	if p.oidc == nil || p.oidc.Binding.Roles[0] != "agent" {
		t.Fatalf("OIDC auth context wrong: %+v", p.oidc)
	}
}

func TestDualAuthForgedJWTIsFinal(t *testing.T) {
	idp := newTestIdP(t)
	svc := idp.service(nil)
	p := &probe{}
	called := false
	apiKeyHit := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true // tenancy.AuthMiddleware stand-in records delegation
			next.ServeHTTP(w, r)
		})
	}
	h := svc.DualAuth(apiKeyHit, httpx.NewRateLimit(1000, 1000, 10))(probeHandler(p))

	// alg=none with a junk signature segment (an EMPTY signature segment is
	// not JWT-shaped and flows to the API-key path, which rejects it as an
	// unknown key — still 401; see the branch-selection tests).
	noneForged := mustMintNone(t, baseClaims(idp.url())) + "Zm9yZ2Vk"
	hsForged := mustMintHS(t, []byte("attacker-secret"), testKid1, baseClaims(idp.url()))
	for _, forged := range []string{noneForged, hsForged} {
		p.hit, called = false, false
		req := getReq("/x")
		req.Header.Set("Authorization", "Bearer "+forged)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != 401 {
			t.Fatalf("forged JWT must be 401, got %d", rec.Code)
		}
		if called {
			t.Fatal("forged JWT must NEVER be retried as an API key")
		}
		if p.hit {
			t.Fatal("forged JWT must not reach the handler")
		}
		if body := rec.Body.String(); !strings.Contains(body, "identity.invalid_token") {
			t.Fatalf("error envelope must name identity.invalid_token, got %s", body)
		}
	}
}

func TestDualAuthRateLimitsPerSubject(t *testing.T) {
	idp := newTestIdP(t)
	svc := idp.service(nil)
	p := &probe{}
	scopes := []string{"api"}
	// burst 1: the second OIDC request from the same subject is limited
	h := svc.DualAuth(stubAPIKeyAuth(&scopes), httpx.NewRateLimit(60, 1, 10))(probeHandler(p))

	raw := mustMintRS(t, idp.rootKey(), testKid1, baseClaims(idp.url()))
	req := getReq("/x")
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("first request must pass, got %d", rec.Code)
	}

	req2 := getReq("/x")
	req2.Header.Set("Authorization", "Bearer "+raw)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request must be 429, got %d", rec2.Code)
	}
	if rec2.Header().Get("Retry-After") == "" {
		t.Fatal("429 must carry Retry-After")
	}
	if !strings.Contains(rec2.Body.String(), "identity.rate_limited") {
		t.Fatalf("429 envelope must name identity.rate_limited, got %s", rec2.Body.String())
	}
	if !p.hit {
		t.Fatal("the first (successful) request must have reached the handler")
	}
}

// ---- RequireCapability ----

func oidcRequest(p *tenancy.Principal, binding Binding, method, target string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	ctx := tenancy.WithPrincipal(r.Context(), p)
	ctx = context.WithValue(ctx, ctxOIDCAuth, &Auth{Principal: p, Binding: binding})
	return r.WithContext(ctx)
}

func apiKeyRequest(p *tenancy.Principal, method, target string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	return r.WithContext(tenancy.WithPrincipal(r.Context(), p))
}

func TestRequireCapabilityDenialNamesCapability(t *testing.T) {
	p := &tenancy.Principal{TenantID: tenantA, Scopes: []string{string(CapInteractionRead)}}
	h := RequireCapability(string(CapCaseWrite))(probeHandler(&probe{}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, oidcRequest(p, staticBinding([]string{"viewer"}), http.MethodPost, "/x"))
	if rec.Code != 403 {
		t.Fatalf("want 403, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "identity.capability_missing") {
		t.Fatalf("envelope must name identity.capability_missing, got %s", body)
	}
	if !strings.Contains(body, `"required_capability":"case.write"`) {
		t.Fatalf("403 payload must NAME the required capability, got %s", body)
	}
}

func TestRequireCapabilityOIDCGrantedPasses(t *testing.T) {
	p := &tenancy.Principal{TenantID: tenantA, Scopes: agentBinding().Capabilities}
	pr := &probe{}
	h := RequireCapability(string(CapInteractionWrite))(probeHandler(pr))
	h.ServeHTTP(httptest.NewRecorder(), oidcRequest(p, agentBinding(), http.MethodPost, "/x"))
	if !pr.hit {
		t.Fatal("capability holder must pass")
	}
}

func TestRequireCapabilityAPIKeyCompatMatrix(t *testing.T) {
	cases := []struct {
		name   string
		scopes []string
		deny   bool
	}{
		{"legacy blanket api scope", []string{"api"}, false},
		{"wildcard scope", []string{"*"}, false},
		{"capability minted as scope", []string{"interaction.read"}, false},
		{"mixed", []string{"api", "cases:write"}, false},
		{"unrelated scope only", []string{"interactions:read"}, true},
		{"no scopes", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &tenancy.Principal{APIKeyID: "k", TenantID: tenantA, Scopes: tc.scopes}
			pr := &probe{}
			h := RequireCapability(string(CapInteractionRead))(probeHandler(pr))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, apiKeyRequest(p, http.MethodGet, "/x"))
			if tc.deny && rec.Code != 403 {
				t.Fatalf("want 403, got %d", rec.Code)
			}
			if !tc.deny {
				if rec.Code != 200 || !pr.hit {
					t.Fatalf("want pass, got %d", rec.Code)
				}
			}
		})
	}
}

func TestRequireCapabilityWithoutPrincipalIs401(t *testing.T) {
	h := RequireCapability(string(CapInteractionRead))(probeHandler(&probe{}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != 401 {
		t.Fatalf("missing principal (defense in depth) must be 401, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "auth.missing_key") {
		t.Fatalf("401 must carry the auth error code, got %s", rec.Body.String())
	}
}

// ---- RequireMethodCapability ----

func TestRequireMethodCapabilityVerbSplit(t *testing.T) {
	viewer := &tenancy.Principal{TenantID: tenantA, Scopes: viewerBinding().Capabilities}
	h := RequireMethodCapability(CapInteractionRead, CapInteractionWrite)(probeHandler(&probe{}))

	cases := []struct {
		method string
		deny   bool
	}{
		{http.MethodGet, false},
		{http.MethodHead, false},
		{http.MethodOptions, false},
		{http.MethodPost, true},
		{http.MethodPut, true},
		{http.MethodDelete, true},
	}
	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, oidcRequest(viewer, viewerBinding(), tc.method, "/x"))
			if tc.deny && rec.Code != 403 {
				t.Fatalf("%s for a viewer must be 403, got %d", tc.method, rec.Code)
			}
			if !tc.deny && rec.Code != 200 {
				t.Fatalf("%s for a viewer must pass, got %d", tc.method, rec.Code)
			}
			if tc.deny && !strings.Contains(rec.Body.String(), `"required_capability":"interaction.write"`) {
				t.Fatalf("%s denial must name interaction.write, got %s", tc.method, rec.Body.String())
			}
		})
	}
}

func TestRequireMethodCapabilityAdminWrites(t *testing.T) {
	admin := &tenancy.Principal{TenantID: tenantA, Scopes: adminBinding().Capabilities}
	pr := &probe{}
	h := RequireMethodCapability(CapCaseRead, CapCaseWrite)(probeHandler(pr))
	h.ServeHTTP(httptest.NewRecorder(), oidcRequest(admin, adminBinding(), http.MethodPost, "/x"))
	if !pr.hit {
		t.Fatal("admin must write")
	}
}

// retryAfterSeconds parity with tenancy (ceil, min 1)
func TestRetryAfterSecondsFloor(t *testing.T) {
	if got := retryAfterSeconds(time.Millisecond); got != "1" {
		t.Fatalf("retry ceil floor: got %q", got)
	}
	if got := retryAfterSeconds(2500 * time.Millisecond); got != "3" {
		t.Fatalf("retry ceil: got %q", got)
	}
}
