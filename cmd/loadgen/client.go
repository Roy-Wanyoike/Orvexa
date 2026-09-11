package main

// HTTP client logic: base transport tuning, per-request API-key rotation, and
// the loopback source-address pool used for webhook ingress (the per-IP
// limiter — see the package doc). All of it is unit-tested without a network
// (fake prober + httptest servers).

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// Outcome classes (the JSON "outcome_counts" keys).
const (
	classOK           = "ok"
	classRateLimited  = "rate_limited" // 429
	classClientError  = "client_error" // other 4xx
	classServerError  = "server_error" // 5xx
	classTransportErr = "transport_error"
)

// SignatureHeader is the platform webhook signature header
// (internal/webhooks.SignatureHeader — duplicated to keep loadgen dependency-free).
const SignatureHeader = "X-Orvexa-Signature"

// Sign returns hex(HMAC-SHA256(secret, body)) — byte-identical to
// internal/webhooks.ComputeSignature, the scheme the gateway validates.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// nonce returns a random hex token for unique-per-request payloads.
func nonce() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is unrecoverable in practice; fall back to a
		// time-derived value so requests remain unique.
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// Result is the per-request outcome.
type Result struct {
	Scenario  string
	Status    int
	Class     string
	LatencyMS float64
}

// ok reports whether the result counts as success for SLO accounting.
func (r Result) ok() bool { return r.Status >= 200 && r.Status < 300 }

// Client is the loadgen HTTP client: one tuned transport for authenticated
// traffic plus a pool of transports bound to distinct loopback source
// addresses for webhook ingress.
type Client struct {
	base       *url.URL
	keys       []string
	keyIdx     atomic.Uint64
	ipIdx      atomic.Uint64
	timeout    time.Duration
	authed     *http.Client
	webhook    []*http.Client // parallel to webhookSrc
	webhookSrc []string
	probed     int  // requested source count
	degraded   bool // rotation requested but unavailable
}

// dialProber abstracts the source-binding probe for tests.
type dialProber func(network, addr, localIP string) error

// probeDial verifies that a connection to addr can be sourced from localIP.
func probeDial(network, addr, localIP string) error {
	local := &net.TCPAddr{IP: net.ParseIP(localIP)}
	d := &net.Dialer{LocalAddr: local, Timeout: 500 * time.Millisecond}
	conn, err := d.Dial(network, addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

// NewClient builds the client. sourceIPs loopback addresses are probed
// (500ms connect budget each); only addresses that successfully source-bind
// are kept. Keys rotate round-robin per request.
func NewClient(base string, keys []string, timeout time.Duration, sourceIPs int) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		return nil, fmt.Errorf("bad -target URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("bad -target URL scheme %q (want http/https)", u.Scheme)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no API keys")
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("timeout must be positive")
	}
	c := &Client{
		base:    u,
		keys:    keys,
		timeout: timeout,
		authed: &http.Client{
			Transport: newTransport(&net.Dialer{Timeout: 3 * time.Second}),
			Timeout:   timeout,
		},
		probed: sourceIPs,
	}
	c.webhook, c.webhookSrc, c.degraded = buildSourcePool(u.Host, sourceIPs, timeout, probeDial)
	return c, nil
}

// newTransport is the tuned loopback transport: connection reuse sized for
// hundreds of concurrent requests, compression off (the API is JSON-only and
// decompression would pollute latency measurement), HTTP/1.1 keep-alive.
func newTransport(dialer *net.Dialer) *http.Transport {
	return &http.Transport{
		DialContext:           dialer.DialContext,
		MaxIdleConns:          512,
		MaxIdleConnsPerHost:   512,
		MaxConnsPerHost:       0,
		IdleConnTimeout:       30 * time.Second,
		DisableCompression:    true,
		ForceAttemptHTTP2:     false,
		ResponseHeaderTimeout: 10 * time.Second,
	}
}

// buildSourcePool probes 127.0.0.1..127.0.0.(n-1) against host and returns a
// client per live source address. degraded=true when none could bind (the
// caller then gets one plain client — traffic still flows, rotation does not).
func buildSourcePool(host string, n int, timeout time.Duration, probe dialProber) ([]*http.Client, []string, bool) {
	var clients []*http.Client
	var srcs []string
	for i := 0; i < n; i++ {
		ip := fmt.Sprintf("127.0.0.%d", i+1)
		if probe("tcp", host, ip) != nil {
			continue
		}
		dialer := &net.Dialer{
			LocalAddr: &net.TCPAddr{IP: net.ParseIP(ip)},
			Timeout:   3 * time.Second,
		}
		clients = append(clients, &http.Client{Transport: newTransport(dialer), Timeout: timeout})
		srcs = append(srcs, ip)
	}
	if len(clients) == 0 {
		return []*http.Client{
			{Transport: newTransport(&net.Dialer{Timeout: 3 * time.Second}), Timeout: timeout},
		}, []string{"127.0.0.1"}, true
	}
	return clients, srcs, false
}

// nextKey rotates the API keys round-robin.
func (c *Client) nextKey() string {
	i := c.keyIdx.Add(1) - 1
	return c.keys[int(i%uint64(len(c.keys)))]
}

// nextWebhookClient rotates the source-bound transports round-robin.
func (c *Client) nextWebhookClient() *http.Client {
	i := c.ipIdx.Add(1) - 1
	return c.webhook[int(i%uint64(len(c.webhook)))]
}

// ProbedSourceIPs reports how many source addresses were requested.
func (c *Client) ProbedSourceIPs() int { return c.probed }

// ActiveSourceIPs reports how many distinct sources carry webhook traffic.
func (c *Client) ActiveSourceIPs() int { return len(c.webhookSrc) }

// SourceIPRotationDegraded reports whether rotation was requested but
// unavailable (single-source fallback in effect).
func (c *Client) SourceIPRotationDegraded() bool { return c.degraded }

// Close releases idle connections of every transport.
func (c *Client) Close() {
	c.authed.CloseIdleConnections()
	for _, hc := range c.webhook {
		hc.CloseIdleConnections()
	}
}

// Get issues an authenticated GET with key rotation.
func (c *Client) Get(ctx context.Context, path string) Result {
	res, _ := c.do(ctx, http.MethodGet, path, nil, false)
	return res
}

// Post issues an authenticated POST with the given JSON body.
func (c *Client) Post(ctx context.Context, path string, body []byte) Result {
	res, _ := c.do(ctx, http.MethodPost, path, body, false)
	return res
}

// PostRead issues an authenticated POST and also returns the response body
// (used by seeding, never in the measured loop — reading the body costs a
// copy the measured path should not pay).
func (c *Client) PostRead(ctx context.Context, path string, body []byte) (Result, []byte) {
	return c.do(ctx, http.MethodPost, path, body, true)
}

// PostWebhook issues a key-less POST signed with the platform HMAC header,
// round-robined over the source-bound transports.
func (c *Client) PostWebhook(ctx context.Context, path string, body []byte, signature string) Result {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base.String()+path, bytes.NewReader(body))
	if err != nil {
		return Result{Class: classTransportErr, LatencyMS: msSince(start)}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(SignatureHeader, signature)
	res, _ := c.roundTrip(c.nextWebhookClient(), req, false)
	return res
}

// do is the shared authenticated path. wantBody caps the read at 1 MiB.
func (c *Client) do(ctx context.Context, method, path string, body []byte, wantBody bool) (Result, []byte) {
	req, err := http.NewRequestWithContext(ctx, method, c.base.String()+path, bytes.NewReader(body))
	if err != nil {
		return Result{Class: classTransportErr}, nil
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-API-Key", c.nextKey())
	return c.roundTrip(c.authed, req, wantBody)
}

// roundTrip performs the request and classifies the outcome.
func (c *Client) roundTrip(hc *http.Client, req *http.Request, wantBody bool) (Result, []byte) {
	start := time.Now()
	resp, err := hc.Do(req)
	lat := msSince(start)
	if err != nil {
		return Result{Class: classTransportErr, LatencyMS: lat}, nil
	}
	var payload []byte
	if wantBody {
		payload, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	} else {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	}
	resp.Body.Close()
	return Result{Status: resp.StatusCode, Class: classify(resp.StatusCode), LatencyMS: lat}, payload
}

// classify maps a status code to an outcome class.
func classify(status int) string {
	switch {
	case status >= 200 && status < 300:
		return classOK
	case status == http.StatusTooManyRequests:
		return classRateLimited
	case status >= 400 && status < 500:
		return classClientError
	default:
		return classServerError
	}
}
