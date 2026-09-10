package identity

import (
	"crypto/x509"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// asAppError asserts the error is the typed application error and returns it.
func asAppError(t *testing.T, err error) *apperrors.Error {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var appErr *apperrors.Error
	if !errors.As(err, &appErr) {
		t.Fatalf("error %v is not *apperrors.Error", err)
	}
	return appErr
}

func reasonOf(t *testing.T, err error) string {
	t.Helper()
	appErr := asAppError(t, err)
	details, ok := appErr.Details.(map[string]string)
	if !ok {
		t.Fatalf("error details missing: %#v", appErr.Details)
	}
	return details["reason"]
}

func verifiedClaims(t *testing.T, svc *Service, raw string) *Claims {
	t.Helper()
	claims, err := svc.Verify(t.Context(), raw)
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	return claims
}

// ---- discovery ----

func TestDiscoveryIssuerMismatchRejected(t *testing.T) {
	idp := newTestIdP(t)
	idp.setDiscovery(0, `{"issuer":"https://evil.example","authorization_endpoint":"https://evil.example/a","token_endpoint":"https://evil.example/t","jwks_uri":"https://evil.example/j"}`)
	svc := idp.service(nil)
	_, err := svc.Verify(t.Context(), mustMintRS(t, idp.rootKey(), testKid1, baseClaims(idp.url())))
	appErr := asAppError(t, err)
	if appErr.Code != "identity.discovery_failed" {
		t.Fatalf("want identity.discovery_failed, got %s", appErr.Code)
	}
}

func TestDiscoveryMissingEndpointsRejected(t *testing.T) {
	idp := newTestIdP(t)
	idp.setDiscovery(0, `{"issuer":"`+idp.url()+`"}`)
	svc := idp.service(nil)
	_, err := svc.Verify(t.Context(), mustMintRS(t, idp.rootKey(), testKid1, baseClaims(idp.url())))
	if got := asAppError(t, err).Code; got != "identity.discovery_failed" {
		t.Fatalf("want identity.discovery_failed, got %s", got)
	}
}

func TestDiscoveryNotPoisonedByTransientFailure(t *testing.T) {
	idp := newTestIdP(t)
	idp.setDiscovery(http.StatusBadGateway, "nope")
	svc := idp.service(nil)
	if _, err := svc.Verify(t.Context(), mustMintRS(t, idp.rootKey(), testKid1, baseClaims(idp.url()))); err == nil {
		t.Fatal("expected discovery failure")
	}
	// provider recovers → the very next request must succeed (no poisoning)
	idp.setDiscovery(0, "")
	verifiedClaims(t, svc, mustMintRS(t, idp.rootKey(), testKid1, baseClaims(idp.url())))
}

// ---- token forgery matrix (docs/rbac.md §forgery matrix) ----

func TestVerifyForgeryMatrix(t *testing.T) {
	idp := newTestIdP(t)
	svc := idp.service(nil)
	valid := mustMintRS(t, idp.rootKey(), testKid1, baseClaims(idp.url()))

	// tampered signature: flip the FIRST byte of the signature segment (the
	// last byte can encode unused padding bits — flipping it can decode to
	// the identical signature, which is not a forgery).
	sigParts := strings.Split(valid, ".")
	sigBytes := []byte(sigParts[2])
	if sigBytes[0] == 'A' {
		sigBytes[0] = 'B'
	} else {
		sigBytes[0] = 'A'
	}
	sigParts[2] = string(sigBytes)
	tampered := strings.Join(sigParts, ".")

	// HS256-confusion variant: HMAC secret = the RSA public key DER bytes
	// (the classic key-confusion attack against RS256 verifiers).
	pubDER, err := x509.MarshalPKIXPublicKey(&idp.rootKey().PublicKey)
	if err != nil {
		t.Fatalf("marshal pubkey: %v", err)
	}

	confusion := baseClaims(idp.url())

	cases := []struct {
		name   string
		token  string
		reason string // empty = only status/code asserted
	}{
		{"control_valid_token", valid, ""},
		{"alg_none", mustMintNone(t, baseClaims(idp.url())), "algorithm"},
		{"hs256_attacker_secret", mustMintHS(t, []byte("attacker-secret"), testKid1, baseClaims(idp.url())), "algorithm"},
		{"hs256_rsa_pubkey_confusion", mustMintHS(t, pubDER, testKid1, confusion), "algorithm"},
		{"wrong_issuer", mustMintRS(t, idp.rootKey(), testKid1, mutateClaims(baseClaims(idp.url()), func(c *Claims) { c.Issuer = "https://evil.example" })), "issuer"},
		{"wrong_audience", mustMintRS(t, idp.rootKey(), testKid1, mutateClaims(baseClaims(idp.url()), func(c *Claims) { c.Audience = jwt.ClaimStrings{"other-client"} })), "audience"},
		{"expired", mustMintRS(t, idp.rootKey(), testKid1, mutateClaims(baseClaims(idp.url()), func(c *Claims) { c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Hour)) })), "expired"},
		{"not_yet_valid", mustMintRS(t, idp.rootKey(), testKid1, mutateClaims(baseClaims(idp.url()), func(c *Claims) { c.NotBefore = jwt.NewNumericDate(time.Now().Add(time.Hour)) })), "not_yet_valid"},
		{"missing_exp", mustMintRS(t, idp.rootKey(), testKid1, mutateClaims(baseClaims(idp.url()), func(c *Claims) { c.ExpiresAt = nil })), "unverifiable"},
		{"missing_kid", mustMintRS(t, idp.rootKey(), "", baseClaims(idp.url())), "key"},
		{"unknown_kid", mustMintRS(t, idp.rootKey(), "ghost-kid", baseClaims(idp.url())), "unverifiable"},
		{"tampered_signature", tampered, "signature"},
		{"empty_subject", mustMintRS(t, idp.rootKey(), testKid1, mutateClaims(baseClaims(idp.url()), func(c *Claims) { c.Subject = "" })), "claims"},
		{"garbage_not_a_jwt", "not-a-token", "malformed"},
		{"empty_string", "", "malformed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claims, err := svc.Verify(t.Context(), tc.token)
			if tc.name == "control_valid_token" {
				if err != nil {
					t.Fatalf("control token must verify, got: %v", err)
				}
				if claims.Subject != testSubject || claims.TenantClaim() != tenantA {
					t.Fatalf("control claims wrong: %+v", claims)
				}
				return
			}
			appErr := asAppError(t, err)
			if appErr.HTTPStatus() != 401 {
				t.Fatalf("want 401, got %d", appErr.HTTPStatus())
			}
			if appErr.Code != "identity.invalid_token" {
				t.Fatalf("want identity.invalid_token, got %s", appErr.Code)
			}
			if tc.reason != "" {
				if got := reasonOf(t, err); got != tc.reason {
					t.Fatalf("want reason %q, got %q", tc.reason, got)
				}
			}
		})
	}
}

func mutateClaims(base Claims, f func(*Claims)) Claims {
	f(&base)
	return base
}

func TestVerifyTenantClaimAlias(t *testing.T) {
	idp := newTestIdP(t)
	svc := idp.service(nil)
	claims := mutateClaims(baseClaims(idp.url()), func(c *Claims) {
		c.Tenant = ""
		c.TenantID = tenantB // tenant_id alias accepted
	})
	got := verifiedClaims(t, svc, mustMintRS(t, idp.rootKey(), testKid1, claims))
	if got.TenantClaim() != tenantB {
		t.Fatalf("tenant_id alias not honored: %q", got.TenantClaim())
	}
}

// ---- principal mapping / authorization resolution ----

func TestPrincipalStaticBindingRolesClaim(t *testing.T) {
	idp := newTestIdP(t)
	svc := idp.service(nil) // no resolver → static posture (roles claim)
	claims := mutateClaims(baseClaims(idp.url()), func(c *Claims) {
		c.Roles = []string{"agent", "supervisor", "stranger-role"}
	})
	p, binding, err := svc.Principal(t.Context(), &claims)
	if err != nil {
		t.Fatalf("principal: %v", err)
	}
	if p.TenantID != tenantA {
		t.Fatalf("tenant must come from the token claims, got %q", p.TenantID)
	}
	want := []string{string(CapInteractionRead), string(CapInteractionWrite), string(CapCaseRead), string(CapCaseWrite), string(CapWorkflowRun), string(CapSearchRead)}
	if strings.Join(p.Scopes, ",") != strings.Join(want, ",") {
		t.Fatalf("capability union wrong:\n got %v\nwant %v", p.Scopes, want)
	}
	if strings.Join(binding.Roles, ",") != "agent,supervisor" {
		t.Fatalf("roles wrong: %v (unknown roles must be dropped)", binding.Roles)
	}
}

func TestPrincipalDBBindingAuthoritativeOverRolesClaim(t *testing.T) {
	idp := newTestIdP(t)
	svc := idp.service(nil).WithRoleResolver(staticResolver{
		tenantA + "|" + testSubject: viewerBinding(), // DB says viewer
	})
	claims := mutateClaims(baseClaims(idp.url()), func(c *Claims) {
		c.Roles = []string{"admin"} // token claims admin
	})
	p, binding, err := svc.Principal(t.Context(), &claims)
	if err != nil {
		t.Fatalf("principal: %v", err)
	}
	if strings.Join(binding.Roles, ",") != "viewer" {
		t.Fatalf("role_bindings must be authoritative, got %v", binding.Roles)
	}
	if !p.HasCapability(string(CapInteractionRead)) || p.HasCapability(string(CapAdminOrg)) {
		t.Fatalf("capabilities must follow the binding, got %v", p.Scopes)
	}
}

func TestCrossTenantClaimYieldsNoCapabilities(t *testing.T) {
	idp := newTestIdP(t)
	// bindings exist ONLY for tenant A; the token asserts tenant B
	svc := idp.service(nil).WithRoleResolver(staticResolver{
		tenantA + "|" + testSubject: adminBinding(),
	})
	claims := mutateClaims(baseClaims(idp.url()), func(c *Claims) { c.Tenant = tenantB })
	p, binding, err := svc.Principal(t.Context(), &claims)
	if err != nil {
		t.Fatalf("principal: %v", err)
	}
	if len(binding.Capabilities) != 0 || len(p.Scopes) != 0 {
		t.Fatalf("cross-tenant claim must resolve to an EMPTY binding, got %v", p.Scopes)
	}
	// and RequireCapability therefore denies everything downstream:
	if p.HasCapability(string(CapInteractionRead)) {
		t.Fatal("cross-tenant principal must hold no capabilities")
	}
}

func TestNonUUIDTenantClaimShortCircuitsBinding(t *testing.T) {
	idp := newTestIdP(t)
	svc := idp.service(nil).WithRoleResolver(staticResolver{
		"not-a-uuid|" + testSubject: adminBinding(), // must never be consulted
	})
	claims := mutateClaims(baseClaims(idp.url()), func(c *Claims) { c.Tenant = "not-a-uuid" })
	p, _, err := svc.Principal(t.Context(), &claims)
	if err != nil {
		t.Fatalf("principal: %v", err)
	}
	if len(p.Scopes) != 0 {
		t.Fatalf("non-uuid tenant claim must yield no capabilities, got %v", p.Scopes)
	}
}

// ---- concurrency (race detector target) ----

func TestVerifyConcurrentMixedTraffic(t *testing.T) {
	idp := newTestIdP(t)
	svc := idp.service(nil)
	valid := mustMintRS(t, idp.rootKey(), testKid1, baseClaims(idp.url()))
	forged := mustMintNone(t, baseClaims(idp.url()))

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if _, err := svc.Verify(t.Context(), valid); err != nil {
					errs <- err
					return
				}
				if _, err := svc.Verify(t.Context(), forged); err == nil {
					errs <- errors.New("forged token unexpectedly accepted")
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent verify failed: %v", err)
	}
}
