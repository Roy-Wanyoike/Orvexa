package identity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Shared fixtures for the identity plane tests: an in-process OpenID
// Provider (discovery + JWKS + token endpoint) whose key set, discovery
// document and token responses can be mutated per test to simulate
// rotation, misconfiguration and hostile responses.

const (
	testClientID = "orvexa-client"
	testSecret   = "test-client-secret"
	testSubject  = "user-1"
	testKid1     = "test-key-1"
	testKid2     = "test-key-2"

	// tenantsA/B are valid tenants.id-shaped UUIDs (bindings are keyed by
	// tenants.id; a non-uuid claim cannot match any binding).
	tenantA = "11111111-1111-1111-1111-111111111111"
	tenantB = "22222222-2222-2222-2222-222222222222"
)

type testIdP struct {
	t   *testing.T
	srv *httptest.Server

	mu          sync.Mutex
	keys        map[string]*rsa.PrivateKey // published JWKS (kid → key)
	discoBody   string                     // override discovery body when non-empty
	discoCode   int                        // override discovery status when non-zero
	tokenCode   int                        // override token endpoint status when non-zero
	tokenBody   string                     // override token body when non-empty
	jwksHits    int
	tokenHits   int
	tokenMinter func() string // default: mint a valid ID token for testSubject
}

func newTestIdP(t *testing.T) *testIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa keygen: %v", err)
	}
	idp := &testIdP{t: t, keys: map[string]*rsa.PrivateKey{testKid1: key}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", idp.serveDiscovery)
	mux.HandleFunc("/jwks.json", idp.serveJWKS)
	mux.HandleFunc("/token", idp.serveToken)
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	return idp
}

func (idp *testIdP) url() string { return idp.srv.URL }

func (idp *testIdP) rootKey() *rsa.PrivateKey {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	return idp.keys[testKid1]
}

// rotateTo replaces the published key set (rotation simulation).
func (idp *testIdP) rotateTo(kid string, key *rsa.PrivateKey) {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	idp.keys = map[string]*rsa.PrivateKey{kid: key}
}

func (idp *testIdP) publish(kid string, key *rsa.PrivateKey) {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	idp.keys[kid] = key
}

func (idp *testIdP) setDiscovery(code int, body string) {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	idp.discoCode, idp.discoBody = code, body
}

func (idp *testIdP) setToken(code int, body string) {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	idp.tokenCode, idp.tokenBody = code, body
}

func (idp *testIdP) hits() (jwks, token int) {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	return idp.jwksHits, idp.tokenHits
}

func (idp *testIdP) serveDiscovery(w http.ResponseWriter, _ *http.Request) {
	idp.mu.Lock()
	code, body := idp.discoCode, idp.discoBody
	idp.mu.Unlock()
	if code != 0 {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
		return
	}
	if body != "" {
		_, _ = w.Write([]byte(body))
		return
	}
	u := idp.srv.URL
	doc := map[string]any{
		"issuer":                                u,
		"authorization_endpoint":                u + "/authorize",
		"token_endpoint":                        u + "/token",
		"jwks_uri":                              u + "/jwks.json",
		"id_token_signing_alg_values_supported": []string{"RS256", "HS256"},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(doc)
}

func (idp *testIdP) serveJWKS(w http.ResponseWriter, _ *http.Request) {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	idp.jwksHits++
	keys := make([]map[string]any, 0, len(idp.keys))
	for kid, key := range idp.keys {
		keys = append(keys, jwkFor(kid, key))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
}

func (idp *testIdP) serveToken(w http.ResponseWriter, r *http.Request) {
	idp.mu.Lock()
	code, body, minter := idp.tokenCode, idp.tokenBody, idp.tokenMinter
	idp.tokenHits++
	idp.mu.Unlock()
	if code != 0 {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
		return
	}
	if body != "" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
		return
	}
	if minter != nil {
		body = minter()
	} else {
		tok := idp.defaultIDToken()
		body = `{"access_token":"test-access-token","token_type":"Bearer","id_token":"` + tok + `"}`
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

// defaultIDToken mints a valid, correctly-signed ID token for testSubject.
func (idp *testIdP) defaultIDToken() string {
	idp.mu.Lock()
	key := idp.keys[testKid1]
	idp.mu.Unlock()
	return mustMintRS(idp.t, key, testKid1, baseClaims(idp.srv.URL))
}

// service builds an identity Service bound to this provider. Rotation tests
// need the fetch floor disabled (JWKSMinRefresh < 0 → 0 in NewService);
// tests that exercise throttling override it explicitly.
func (idp *testIdP) service(mutate func(*Config)) *Service {
	cfg := Config{
		Issuer:       idp.srv.URL,
		ClientID:     testClientID,
		ClientSecret: testSecret,
		HTTPClient:   idp.srv.Client(),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return NewService(cfg)
}

func jwkFor(kid string, key *rsa.PrivateKey) map[string]any {
	return map[string]any{
		"kid": kid,
		"kty": "RSA",
		"alg": "RS256",
		"use": "sig",
		"n":   base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes()),
	}
}

// baseClaims is the valid claim set every forgery case mutates from.
func baseClaims(issuer string) Claims {
	now := time.Now()
	return Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Subject:   testSubject,
			Audience:  jwt.ClaimStrings{testClientID},
			ExpiresAt: jwt.NewNumericDate(now.Add(10 * time.Minute)),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now.Add(-time.Minute)),
			ID:        "jti-1",
		},
		Tenant: tenantA,
		Roles:  []string{"agent"},
	}
}

func mustMintRS(t *testing.T, key *rsa.PrivateKey, kid string, claims Claims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, &claims)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	raw, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("mint RS256: %v", err)
	}
	return raw
}

func mustMintHS(t *testing.T, secret []byte, kid string, claims Claims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, &claims)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	raw, err := tok.SignedString(secret)
	if err != nil {
		t.Fatalf("mint HS256: %v", err)
	}
	return raw
}

func mustMintNone(t *testing.T, claims Claims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, &claims)
	tok.Header["kid"] = testKid1
	raw, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("mint none: %v", err)
	}
	return raw
}

// staticResolver is a fake RoleResolver: bindings keyed "tenant|subject".
type staticResolver map[string]Binding

func (r staticResolver) Resolve(_ context.Context, tenantID, subject string) (Binding, error) {
	if b, ok := r[tenantID+"|"+subject]; ok {
		return b, nil
	}
	return Binding{}, nil
}

func adminBinding() Binding {
	return Binding{
		Roles: []string{"admin"},
		Capabilities: []string{
			string(CapInteractionRead), string(CapInteractionWrite),
			string(CapCaseRead), string(CapCaseWrite),
			string(CapWorkflowRun), string(CapSearchRead), string(CapAdminOrg),
		},
	}
}

func agentBinding() Binding {
	return Binding{
		Roles: []string{"agent"},
		Capabilities: []string{
			string(CapInteractionRead), string(CapInteractionWrite),
			string(CapCaseRead), string(CapCaseWrite), string(CapSearchRead),
		},
	}
}

func viewerBinding() Binding {
	return Binding{
		Roles:        []string{"viewer"},
		Capabilities: []string{string(CapInteractionRead), string(CapCaseRead), string(CapSearchRead)},
	}
}
