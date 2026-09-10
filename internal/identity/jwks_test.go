package identity

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"math/big"
	"testing"
	"time"
)

// The JWKS cache tests exercise kid rotation, TTL staleness, fetch
// throttling and hostile key documents against the in-process provider.

func testServiceWithJWKS(t *testing.T, idp *testIdP, minRefresh time.Duration) *Service {
	t.Helper()
	svc := idp.service(func(c *Config) {
		c.JWKSTTL = time.Hour
		c.JWKSMinRefresh = minRefresh
	})
	if err := svc.ensure(t.Context()); err != nil {
		t.Fatalf("discovery: %v", err)
	}
	return svc
}

func generateKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa keygen: %v", err)
	}
	return key
}

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func TestJWKSRotationPickedUpOnUnknownKid(t *testing.T) {
	idp := newTestIdP(t)
	svc := testServiceWithJWKS(t, idp, -1) // no fetch floor

	// old key verifies
	oldKey := idp.rootKey()
	verifiedClaims(t, svc, mustMintRS(t, oldKey, testKid1, baseClaims(idp.url())))

	// IdP rotates: publishes ONLY the new key
	newKey := generateKey(t)
	idp.rotateTo(testKid2, newKey)

	// token under the new kid verifies (rotation fetched on miss)
	verifiedClaims(t, svc, mustMintRS(t, newKey, testKid2, baseClaims(idp.url())))

	// token under the retired kid fails closed
	_, err := svc.Verify(t.Context(), mustMintRS(t, oldKey, testKid1, baseClaims(idp.url())))
	asAppError(t, err)
}

func TestJWKSTTLRefreshesCache(t *testing.T) {
	idp := newTestIdP(t)
	svc := testServiceWithJWKS(t, idp, -1)
	raw := mustMintRS(t, idp.rootKey(), testKid1, baseClaims(idp.url()))
	verifiedClaims(t, svc, raw)

	before, _ := idp.hits()

	// advance the clock past the TTL: the next lookup must re-fetch even for
	// a CACHED kid (rotated-out keys must not linger past the TTL)
	svc.jwks.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	verifiedClaims(t, svc, raw)
	after, _ := idp.hits()
	if after <= before {
		t.Fatalf("TTL expiry must trigger a re-fetch (before=%d after=%d)", before, after)
	}
}

func TestJWKSUnknownKidFloodIsThrottled(t *testing.T) {
	idp := newTestIdP(t)
	svc := testServiceWithJWKS(t, idp, time.Hour) // 1h floor between fetches

	raw := mustMintRS(t, idp.rootKey(), "flood-kid", baseClaims(idp.url()))
	for i := 0; i < 5; i++ {
		if _, err := svc.Verify(t.Context(), raw); err == nil {
			t.Fatal("unknown kid must be rejected")
		}
	}
	hits, _ := idp.hits()
	if hits != 1 {
		t.Fatalf("unknown-kid flood must trigger exactly ONE fetch, got %d", hits)
	}
}

func TestJWKParserRejectsUnusableEntries(t *testing.T) {
	good := generateKey(t)
	cases := []struct {
		name string
		jwk  jwk
	}{
		{"missing kid", jwk{Kid: "", Kty: "RSA", N: b64u(good.PublicKey.N.Bytes()), E: "AQAB"}},
		{"non-RSA kty", jwk{Kid: "k", Kty: "EC", N: b64u(good.PublicKey.N.Bytes()), E: "AQAB"}},
		{"non-RS256 alg", jwk{Kid: "k", Kty: "RSA", Alg: "RS512", N: b64u(good.PublicKey.N.Bytes()), E: "AQAB"}},
		{"enc use", jwk{Kid: "k", Kty: "RSA", Alg: "RS256", Use: "enc", N: b64u(good.PublicKey.N.Bytes()), E: "AQAB"}},
		{"malformed modulus", jwk{Kid: "k", Kty: "RSA", Alg: "RS256", N: "!!!not-base64!!!", E: "AQAB"}},
		{"empty modulus", jwk{Kid: "k", Kty: "RSA", Alg: "RS256", N: "", E: "AQAB"}},
		{"zero exponent", jwk{Kid: "k", Kty: "RSA", Alg: "RS256", N: b64u(good.PublicKey.N.Bytes()), E: "AA"}},
		{"missing exponent", jwk{Kid: "k", Kty: "RSA", Alg: "RS256", N: b64u(good.PublicKey.N.Bytes()), E: ""}},
		{"oversize exponent", jwk{Kid: "k", Kty: "RSA", Alg: "RS256", N: b64u(good.PublicKey.N.Bytes()), E: b64u([]byte{0x7f, 0xff, 0xff, 0xff, 0xff})}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.jwk.rsaPublicKey(); err == nil {
				t.Fatal("unusable JWK must be rejected")
			}
		})
	}

	valid := jwk{Kid: "k", Kty: "RSA", Alg: "RS256", Use: "sig", N: b64u(good.PublicKey.N.Bytes()), E: b64u(big.NewInt(int64(good.PublicKey.E)).Bytes())}
	pk, err := valid.rsaPublicKey()
	if err != nil || pk == nil || pk.key.N.Cmp(good.PublicKey.N) != 0 || pk.key.E != good.PublicKey.E {
		t.Fatalf("valid JWK must parse to the same key: %v", err)
	}
}

func TestJWKSFetchFailureFailsClosed(t *testing.T) {
	idp := newTestIdP(t)
	svc := testServiceWithJWKS(t, idp, -1)

	// simulate a JWKS endpoint outage: dead URL + evicted cache
	svc.jwks.url = idp.url() + "/dead-jwks"
	svc.jwks.keys = map[string]*rsaPublicKey{}
	svc.jwks.fetchedAt = time.Time{}

	_, err := svc.Verify(t.Context(), mustMintRS(t, idp.rootKey(), testKid1, baseClaims(idp.url())))
	appErr := asAppError(t, err)
	if appErr.HTTPStatus() != 401 {
		t.Fatalf("unfetchable keys must fail closed with 401, got %d", appErr.HTTPStatus())
	}
	if !errors.Is(err, appErr) {
		t.Fatal("typed error expected")
	}
}
