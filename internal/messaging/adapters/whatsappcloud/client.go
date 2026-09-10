package whatsappcloud

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Roy-Wanyoike/orvexa/internal/comms/registry"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// maxResponseBodyBytes bounds how much of a Graph response is read into
// memory. Send responses and error envelopes are small; a runaway body is a
// provider-side incident, not something to buffer.
const maxResponseBodyBytes = 64 * 1024

// RetryPolicy bounds the adapter's in-process retry budget. Retries apply
// ONLY to provider-signaled transient failures — HTTP 429, HTTP 5xx, and the
// Graph rate-limit codes (130429, 80007) — never to the 24h-window error or
// any other 4xx classification, and never to transport errors (transport
// timeouts are the core's retry domain: messaging.Service re-invokes
// provider.Send after transport failures).
type RetryPolicy struct {
	// MaxAttempts is the total attempt count including the first.
	// Default 3.
	MaxAttempts int
	// BaseDelay is the backoff after the first failed attempt. Delay doubles
	// per attempt, capped at MaxDelay. Default 200ms.
	BaseDelay time.Duration
	// MaxDelay caps exponential growth and honored Retry-After values.
	// Default 2s.
	MaxDelay time.Duration
	// Sleep suspends execution between attempts. Default: context-aware
	// timer. Injectable for deterministic tests.
	Sleep func(ctx context.Context, d time.Duration) error
}

// withDefaults applies the documented defaults to zero fields.
func (r RetryPolicy) withDefaults() RetryPolicy {
	if r.MaxAttempts <= 0 {
		r.MaxAttempts = 3
	}
	if r.BaseDelay <= 0 {
		r.BaseDelay = 200 * time.Millisecond
	}
	if r.MaxDelay <= 0 {
		r.MaxDelay = 2 * time.Second
	}
	if r.Sleep == nil {
		r.Sleep = sleepCtx
	}
	return r
}

// sleepCtx is the default context-aware backoff sleep: an expired context
// interrupts the wait (and therefore the retry loop) immediately.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// graphClient is the minimal Graph API HTTP client: one endpoint
// (POST /{phone-number-id}/messages), Bearer auth, bounded response reads,
// bounded retries. Deliberately unglamorous — no SDK dependency (go.mod is
// frozen for this wave).
type graphClient struct {
	baseURL       string
	phoneNumberID string
	token         registry.Secret
	http          *http.Client
	retry         RetryPolicy
}

// sendResponse is the success envelope of POST /{phone-number-id}/messages:
//
//	{"messaging_product":"whatsapp","contacts":[...],
//	 "messages":[{"id":"wamid.<...>"}]}
type sendResponse struct {
	MessagingProduct string `json:"messaging_product"`
	Messages         []struct {
		ID string `json:"id"`
	} `json:"messages"`
}

// messageID extracts the wamid. A 2xx without one is a contract violation
// and fails loudly: status receipts resolve through this id, so accepting a
// send we cannot correlate would orphan the interaction's lifecycle.
func (r *sendResponse) messageID() (string, error) {
	if len(r.Messages) == 0 || r.Messages[0].ID == "" {
		return "", apperrors.Internal("whatsapp.malformed_send_response",
			"provider accepted the message but returned no message id")
	}
	return r.Messages[0].ID, nil
}

// endpoint renders the messages URL: {base}/{phone-number-id}/messages.
func (c *graphClient) endpoint() string {
	return strings.TrimSuffix(c.baseURL, "/") + "/" + c.phoneNumberID + "/messages"
}

// graphAttempt is the classified outcome of one HTTP attempt: nil when the
// request succeeded, otherwise the mapped error plus its retry verdict and
// the provider's Retry-After hint (0 = no hint).
type graphAttempt struct {
	err        error
	retryable  bool
	retryAfter time.Duration
}

// post sends one payload with the bounded retry budget and returns the
// parsed success envelope.
func (c *graphClient) post(ctx context.Context, payload any) (*sendResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, apperrors.Internal("whatsapp.payload_marshal_failed",
			"request could not be encoded").WithCause(err)
	}

	var last *graphAttempt
	for attempt := 0; attempt < c.retry.MaxAttempts; attempt++ {
		if attempt > 0 {
			if err := c.retry.Sleep(ctx, c.backoff(attempt, last.retryAfter)); err != nil {
				return nil, err // ctx canceled mid-backoff
			}
		}
		resp, res := c.postOnce(ctx, body)
		if res == nil {
			return resp, nil
		}
		last = res
		if !last.retryable {
			return nil, last.err
		}
	}
	return nil, last.err
}

// postOnce performs exactly one HTTP attempt and classifies the outcome.
// Transport errors surface raw (no retry, no wrapping): the core's retry
// path owns transport semantics, and retrying both layers would multiply the
// effective budget beyond the documented bound.
func (c *graphClient) postOnce(ctx context.Context, body []byte) (*sendResponse, *graphAttempt) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(), bytes.NewReader(body))
	if err != nil {
		return nil, &graphAttempt{err: apperrors.Internal("whatsapp.request_build_failed",
			"request could not be built").WithCause(err)}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+string(c.token))
	req.Header.Set("User-Agent", "orvexa-whatsappcloud/1.0")

	resp, err := c.http.Do(req)
	if err != nil {
		// Transport error: never retried here (core's retry domain), never
		// wrapped into a client-safe classification — it is not one.
		return nil, &graphAttempt{err: err}
	}
	defer resp.Body.Close() //nolint:errcheck — read side is fully consumed below
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes))
	if readErr != nil {
		return nil, &graphAttempt{err: apperrors.Internal("whatsapp.response_read_failed",
			"provider response could not be read").WithCause(readErr)}
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var out sendResponse
		if err := json.Unmarshal(respBody, &out); err != nil {
			return nil, &graphAttempt{err: apperrors.Internal("whatsapp.malformed_send_response",
				"provider returned an unparsable success envelope").WithCause(err)}
		}
		return &out, nil
	}
	return nil, mapGraphError(resp.StatusCode, respBody, resp.Header)
}

// backoff computes the wait before the next attempt: Retry-After (bounded by
// MaxDelay) when the provider sent one, else exponential (BaseDelay <<
// attempt-1), also capped. Deliberately jitter-free so tests stay
// deterministic.
func (c *graphClient) backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter > c.retry.MaxDelay {
			return c.retry.MaxDelay
		}
		return retryAfter
	}
	d := c.retry.BaseDelay << (attempt - 1)
	if d <= 0 || d > c.retry.MaxDelay {
		return c.retry.MaxDelay
	}
	return d
}

// retryAfterSeconds parses a Retry-After header value (integer seconds; a
// non-parsable or non-positive value yields 0 = "no hint"). Absurdly large
// hints saturate at the maximum duration instead of overflowing negative —
// the caller caps the honored value at RetryPolicy.MaxDelay regardless.
func retryAfterSeconds(v string) time.Duration {
	v = strings.TrimSpace(v)
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		return 0
	}
	const maxSeconds = int64(math.MaxInt64) / int64(time.Second)
	if int64(secs) > maxSeconds {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(secs) * time.Second
}
