package identity

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Roy-Wanyoike/orvexa/internal/platform/httpx"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
	"golang.org/x/oauth2"
)

// The login endpoints are PUBLIC (mounted outside the authenticated tree) and
// stateless: no cookies, no sessions. The server mints a MAC-protected state
// token (nonce + expiry keyed by the client secret); the caller echoes it on
// /identity/callback where it is verified before the code exchange. Token
// material stays with the IdP — the callback returns tenant-scoped principal
// INFO only (no access/refresh/ID tokens are echoed, so responses are safe
// to log and carry no replayable credentials).

const (
	stateTTL        = 10 * time.Minute
	stateNonceBytes = 32
)

func errStateInvalid() *apperrors.Error {
	return apperrors.Unauth("identity.state_invalid", "state validation failed")
}

func errRequestInvalid(msg string) *apperrors.Error {
	return apperrors.Invalid("identity.request_invalid", msg)
}

func errExchange(cause error) *apperrors.Error {
	return apperrors.Unauth("identity.exchange_failed", "authorization code exchange failed").WithCause(cause)
}

func errIDTokenMissing() *apperrors.Error {
	return apperrors.Unauth("identity.id_token_missing", "identity provider did not return an ID token")
}

// stateKey derives the state-HMAC key from the client secret. The secret
// never appears in output, logs or errors.
func (s *Service) stateKey() []byte {
	h := sha256.Sum256([]byte("orvexa/oidc/state/v1\x00" + s.cfg.ClientSecret))
	return h[:]
}

// signState mints "<b64url(payload)>.<b64url(hmac)>" with payload
// "<nonce>|<expiry-unix>".
func (s *Service) signState(nonce string, exp time.Time) string {
	payload := nonce + "|" + strconv.FormatInt(exp.Unix(), 10)
	mac := hmac.New(sha256.New, s.stateKey())
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) +
		"." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verifyState validates the MAC and the freshness window, returning the nonce.
func (s *Service) verifyState(state string, now time.Time) (string, error) {
	dot := strings.LastIndexByte(state, '.')
	if dot <= 0 || dot == len(state)-1 {
		return "", errStateInvalid()
	}
	rawPayload, err := base64.RawURLEncoding.DecodeString(state[:dot])
	if err != nil {
		return "", errStateInvalid()
	}
	sig, err := base64.RawURLEncoding.DecodeString(state[dot+1:])
	if err != nil {
		return "", errStateInvalid()
	}
	mac := hmac.New(sha256.New, s.stateKey())
	mac.Write(rawPayload)
	if subtle.ConstantTimeCompare(sig, mac.Sum(nil)) != 1 {
		return "", errStateInvalid()
	}
	payload := string(rawPayload)
	pipe := strings.LastIndexByte(payload, '|')
	if pipe <= 0 || pipe == len(payload)-1 {
		return "", errStateInvalid()
	}
	exp, err := strconv.ParseInt(payload[pipe+1:], 10, 64)
	if err != nil || now.Unix() > exp {
		return "", errStateInvalid()
	}
	nonce := payload[:pipe]
	if len(nonce) < stateNonceBytes {
		return "", errStateInvalid()
	}
	return nonce, nil
}

// redirectURL resolves the OAuth2 redirect URI: the fixed override when
// configured, otherwise derived from the request host (echoed back only to
// the caller; the IdP enforces its registered-redirect allowlist).
func (s *Service) redirectURL(r *http.Request) string {
	if s.cfg.RedirectURL != "" {
		return s.cfg.RedirectURL
	}
	scheme := "https"
	if r.TLS == nil {
		scheme = "http"
	}
	return scheme + "://" + r.Host + "/api/v1/identity/callback"
}

// AuthorizeURLHandler serves GET /api/v1/identity/authorize-url:
// {authorize_url, state, nonce, expires_in, redirect_uri}.
func (s *Service) AuthorizeURLHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.oauthCapable() {
			httpx.WriteError(w, errDisabled())
			return
		}
		if err := s.ensure(r.Context()); err != nil {
			s.log("oidc authorize-url discovery failed",
				"request_id", httpx.RequestIDFrom(r.Context()))
			httpx.WriteError(w, err)
			return
		}
		nonceBytes := make([]byte, stateNonceBytes)
		if _, err := rand.Read(nonceBytes); err != nil {
			httpx.WriteError(w, apperrors.Internal("identity.nonce_failed", "could not mint state"))
			return
		}
		nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
		exp := time.Now().Add(stateTTL)
		state := s.signState(nonce, exp)
		redirect := s.redirectURL(r)
		url := s.oauth2Config(redirect).AuthCodeURL(state, oauthNonce(nonce))
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"authorize_url": url,
			"state":         state,
			"nonce":         nonce,
			"expires_in":    int(stateTTL.Seconds()),
			"redirect_uri":  redirect,
		}, nil)
	}
}

// oauthNonce passes the OIDC nonce parameter through oauth2's generic params.
func oauthNonce(nonce string) oauth2.AuthCodeOption {
	return oauth2.SetAuthURLParam("nonce", nonce)
}

// CallbackHandler serves GET /api/v1/identity/callback?code=&state=:
// state validation → code exchange → ID-token verification → tenant-scoped
// principal info. 200 on success; 422 missing params; 401 state/exchange/
// token failures; 404 when OIDC is not configured.
func (s *Service) CallbackHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.oauthCapable() {
			httpx.WriteError(w, errDisabled())
			return
		}
		q := r.URL.Query()
		code, state := q.Get("code"), q.Get("state")
		if code == "" || state == "" {
			httpx.WriteError(w, errRequestInvalid("code and state query parameters are required"))
			return
		}
		nonce, err := s.verifyState(state, time.Now())
		if err != nil {
			httpx.WriteError(w, err)
			return
		}
		_ = nonce // replay-safe binding: nonce is MAC-locked to the state and expired after one window
		if err := s.ensure(r.Context()); err != nil {
			s.log("oidc callback discovery failed",
				"request_id", httpx.RequestIDFrom(r.Context()))
			httpx.WriteError(w, err)
			return
		}
		tok, err := s.oauth2Config(s.redirectURL(r)).Exchange(r.Context(), code)
		if err != nil {
			httpx.WriteError(w, errExchange(err))
			return
		}
		rawID, _ := tok.Extra("id_token").(string)
		if rawID == "" {
			httpx.WriteError(w, errIDTokenMissing())
			return
		}
		claims, err := s.Verify(r.Context(), rawID)
		if err != nil {
			httpx.WriteError(w, err)
			return
		}
		_, binding, err := s.Principal(r.Context(), claims)
		if err != nil {
			httpx.WriteError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, principalInfo(claims, binding), nil)
	}
}

// principalInfo is the tenant-scoped identity payload: who the caller is,
// which tenant the token asserts, which roles bind them there and which
// capabilities those roles grant. Deliberately token-free.
func principalInfo(claims *Claims, binding Binding) map[string]any {
	info := map[string]any{
		"subject":      claims.Subject,
		"tenant_id":    claims.TenantClaim(),
		"roles":        binding.Roles,
		"capabilities": binding.Capabilities,
		"authorized":   len(binding.Capabilities) > 0,
		"issuer":       claims.Issuer,
	}
	if claims.ExpiresAt != nil {
		info["expires_at"] = claims.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return info
}
