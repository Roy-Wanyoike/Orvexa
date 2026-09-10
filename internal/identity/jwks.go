package identity

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// jwksCache is the RS256 public-key store fed by the provider's jwks_uri.
//
// Properties (docs/rbac.md §JWKS):
//   - kid-keyed lookup; unknown kid triggers ONE throttled re-fetch so key
//     rotation is picked up within milliseconds without polling;
//   - TTL refresh: at TTL age the cache re-fetches lazily on the next lookup
//     (a rotated-out kid fails closed instead of lingering);
//   - minRefresh floor between fetches: a flood of unknown-kid tokens cannot
//     turn the verifier into a key-endpoint DDoS amplifier;
//   - bounded memory: one *rsa.PublicKey per kid, replaced wholesale on each
//     successful fetch.
type jwksCache struct {
	mu         sync.Mutex
	url        string
	client     *http.Client
	keys       map[string]*rsaPublicKey
	fetchedAt  time.Time // last SUCCESSFUL fetch
	lastFetch  time.Time // last attempt (success or failure)
	ttl        time.Duration
	minRefresh time.Duration
	now        func() time.Time
}

// rsaPublicKey is the validated JWK → Go key mapping for one kid.
type rsaPublicKey struct {
	kid string
	key *rsa.PublicKey
}

func newJWKSCache(url string, client *http.Client, ttl, minRefresh time.Duration) *jwksCache {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	if minRefresh < 0 {
		minRefresh = 0
	}
	return &jwksCache{
		url:        url,
		client:     client,
		keys:       map[string]*rsaPublicKey{},
		ttl:        ttl,
		minRefresh: minRefresh,
		now:        time.Now,
	}
}

// keyFunc returns the golang-jwt Keyfunc resolving the header kid through the
// cache (with rotation refresh on miss).
func (c *jwksCache) keyFunc(ctx context.Context, kid string) jwt.Keyfunc {
	return func(*jwt.Token) (any, error) {
		k, err := c.publicKey(ctx, kid)
		if err != nil {
			return nil, err
		}
		return k.key, nil
	}
}

// publicKey resolves kid, refreshing (rotation/TTL) as needed. Called under
// the request path; the mutex serializes fetches so a burst of first requests
// performs at most one JWKS fetch.
func (c *jwksCache) publicKey(ctx context.Context, kid string) (*rsaPublicKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	if k, ok := c.keys[kid]; ok && now.Sub(c.fetchedAt) <= c.ttl {
		return k, nil
	}
	// kid unknown OR cache stale: re-fetch unless we just tried (throttle).
	if now.Sub(c.lastFetch) >= c.minRefresh {
		if err := c.refreshLocked(ctx, now); err != nil {
			return nil, err
		}
	}
	if k, ok := c.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("jwks: no signing key for kid %q", kid)
}

func (c *jwksCache) refreshLocked(ctx context.Context, now time.Time) error {
	c.lastFetch = now
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return fmt.Errorf("jwks: bad request: %w", err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("jwks: fetch failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks: fetch status %d", resp.StatusCode)
	}
	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return fmt.Errorf("jwks: malformed document: %w", err)
	}
	next := make(map[string]*rsaPublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		pk, err := k.rsaPublicKey()
		if err != nil {
			continue // skip unusable entries; a usable one may remain
		}
		next[pk.kid] = pk
	}
	if len(next) == 0 {
		return fmt.Errorf("jwks: no usable RSA signing keys")
	}
	c.keys = next
	c.fetchedAt = now
	return nil
}

// jwk is the subset of RFC 7517 we accept for RS256 verification.
type jwk struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (j jwk) rsaPublicKey() (*rsaPublicKey, error) {
	if j.Kid == "" || j.Kty != "RSA" || (j.Alg != "" && j.Alg != "RS256") {
		return nil, fmt.Errorf("jwks: unsupported key (kty=%q alg=%q)", j.Kty, j.Alg)
	}
	if j.Use != "" && j.Use != "sig" {
		return nil, fmt.Errorf("jwks: key use %q not accepted", j.Use)
	}
	n, err := decodeBase64URLUint(j.N)
	if err != nil {
		return nil, fmt.Errorf("jwks: bad modulus: %w", err)
	}
	e, err := decodeBase64URLUint(j.E)
	if err != nil || e.Sign() <= 0 || !e.IsInt64() || e.Int64() > 1<<31 {
		return nil, fmt.Errorf("jwks: bad exponent")
	}
	return &rsaPublicKey{
		kid: j.Kid,
		key: &rsa.PublicKey{N: n, E: int(e.Int64())},
	}, nil
}

// decodeBase64URLUint decodes a base64url (RFC 7515, unpadded) big-endian
// unsigned integer as used for JWK n/e parameters.
func decodeBase64URLUint(s string) (*big.Int, error) {
	if s == "" {
		return nil, fmt.Errorf("empty value")
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(b), nil
}
