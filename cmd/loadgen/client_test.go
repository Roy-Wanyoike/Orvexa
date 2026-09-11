package main

// Unit tests for the client + signing logic (no external services; httptest
// servers stand in for the API where a round trip is required).

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestSignGoldenVector pins the wire scheme: hex(HMAC-SHA256(secret, body)) —
// vectors generated with `openssl dgst -sha256 -hmac`, matching the
// scripts/e2e-demo.sh signing path and internal/webhooks.ComputeSignature.
func TestSignGoldenVector(t *testing.T) {
	got := Sign("wseg-secret-42", []byte("loadgen-vector"))
	want := "7b6968053d6c1fb67ac569c1f18c0717b27f86f33476f67c3ebc73a64904a424"
	if got != want {
		t.Fatalf("Sign = %s, want %s", got, want)
	}
	if empty := Sign("wseg-secret-42", nil); empty != "6801d918fb59a84e8b3837f18477bebb1a55d31970523c9435c1aa8cf404241d" {
		t.Fatalf("Sign of empty body = %s (golden mismatch)", empty)
	}
}

// TestSignMatchesLibraryHMAC: same scheme the gateway's verifier consumes.
func TestSignMatchesLibraryHMAC(t *testing.T) {
	body := []byte(`{"event":"message.delivered","nonce":"abc"}`)
	mac := hmac.New(sha256.New, []byte("k"))
	mac.Write(body)
	if Sign("k", body) != hex.EncodeToString(mac.Sum(nil)) {
		t.Fatal("Sign diverges from crypto/hmac hex encoding")
	}
	if Sign("k1", body) == Sign("k2", body) {
		t.Fatal("different secrets produced identical signatures")
	}
	if Sign("k", []byte("a")) == Sign("k", []byte("b")) {
		t.Fatal("different bodies produced identical signatures")
	}
}

// TestNonceUnique: random nonces never repeat within a reasonable budget.
func TestNonceUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 10_000; i++ {
		n := nonce()
		if len(n) != 16 {
			t.Fatalf("nonce %q has unexpected length %d", n, len(n))
		}
		if seen[n] {
			t.Fatalf("nonce %q repeated", n)
		}
		seen[n] = true
	}
}

// TestNextKeyRotation: round-robin across the key set, concurrent-safe.
func TestNextKeyRotation(t *testing.T) {
	c := &Client{keys: []string{"k0", "k1", "k2"}}
	var mu sync.Mutex
	counts := map[string]int{}
	var wg sync.WaitGroup
	for i := 0; i < 300; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k := c.nextKey()
			mu.Lock()
			counts[k]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	for _, k := range c.keys {
		if counts[k] != 100 {
			t.Fatalf("key %s used %d times, want 100", k, counts[k])
		}
	}
}

// TestBuildSourcePoolFakeProbe: only probed-alive addresses become transports;
// a total failure degrades to a single plain client (rotation stays honest).
func TestBuildSourcePoolFakeProbe(t *testing.T) {
	var probed []string
	probe := func(network, addr, localIP string) error {
		probed = append(probed, localIP)
		if localIP == "127.0.0.2" || localIP == "127.0.0.3" {
			return nil // only these bind
		}
		return context.DeadlineExceeded
	}
	clients, srcs, degraded := buildSourcePool("127.0.0.1:1", 4, time.Second, probe)
	if degraded {
		t.Fatal("expected non-degraded pool when at least one source binds")
	}
	if len(clients) != 2 || len(srcs) != 2 || srcs[0] != "127.0.0.2" || srcs[1] != "127.0.0.3" {
		t.Fatalf("pool = %d clients, srcs = %v", len(clients), srcs)
	}
	if len(probed) != 4 {
		t.Fatalf("probed %d addresses, want 4", len(probed))
	}

	clients, srcs, degraded = buildSourcePool("127.0.0.1:1", 3, time.Second, func(string, string, string) error {
		return context.DeadlineExceeded
	})
	if !degraded || len(clients) != 1 || len(srcs) != 1 {
		t.Fatalf("expected single-client degraded fallback, got degraded=%v n=%d", degraded, len(clients))
	}
}

// TestBuildSourcePoolLiveProbe: on this platform the loopback /8 either
// accepts alternate source addresses (expected on Linux) or the probe drops
// them — both outcomes must leave a usable pool.
func TestBuildSourcePoolLiveProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	clients, srcs, degraded := buildSourcePool(strings.TrimPrefix(srv.URL, "http://"), 3, 2*time.Second, probeDial)
	if len(clients) < 1 || len(srcs) < 1 {
		t.Fatalf("no usable pool: clients=%d srcs=%v degraded=%v", len(clients), srcs, degraded)
	}
	if degraded && len(srcs) != 1 {
		t.Fatalf("degraded fallback must be exactly one source, got %v", srcs)
	}
}

// TestClassify: SLO-relevant status partitioning.
func TestClassify(t *testing.T) {
	cases := map[int]string{
		200: classOK, 201: classOK, 202: classOK,
		400: classClientError, 401: classClientError, 404: classClientError, 422: classClientError,
		429: classRateLimited,
		500: classServerError, 503: classServerError,
	}
	for status, want := range cases {
		if got := classify(status); got != want {
			t.Fatalf("classify(%d) = %s, want %s", status, got, want)
		}
	}
}

// TestClientRoundTrips: Post/Get rotate keys and hit the right paths;
// PostWebhook delivers the exact signature header over the raw body and no
// API key at all (webhook ingress is key-less by contract).
func TestClientRoundTrips(t *testing.T) {
	var (
		mu       sync.Mutex
		seenKeys = map[string]int{}
		seenSig  string
		seenBody []byte
		seenPath string
	)
	body := []byte(`{"event":"message.delivered"}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		if key := r.Header.Get("X-API-Key"); key != "" {
			seenKeys[key]++
		}
		if sig := r.Header.Get(SignatureHeader); sig != "" {
			seenSig = sig
			seenBody = raw
			seenPath = r.URL.Path
		}
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, []string{"ka", "kb"}, 2*time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx := context.Background()
	if res := c.Post(ctx, "/api/v1/interactions", body); res.Status != 202 {
		t.Fatalf("Post = %d", res.Status)
	}
	if res := c.Get(ctx, "/api/v1/customers?limit=50"); res.Status != 202 {
		t.Fatalf("Get = %d", res.Status)
	}
	mu.Lock()
	if len(seenKeys) != 2 {
		t.Fatalf("expected both keys rotated across two requests, saw %v", seenKeys)
	}
	mu.Unlock()

	sig := Sign("s3cret", body)
	if res := c.PostWebhook(ctx, "/api/v1/webhooks/whatsapp_cloud", body, sig); res.Status != 202 {
		t.Fatalf("PostWebhook = %d", res.Status)
	}
	mu.Lock()
	defer mu.Unlock()
	if seenSig != sig {
		t.Fatalf("gateway saw signature %q, want %q", seenSig, sig)
	}
	if seenPath != "/api/v1/webhooks/whatsapp_cloud" {
		t.Fatalf("gateway saw path %q", seenPath)
	}
	if string(seenBody) != string(body) {
		t.Fatalf("gateway body = %q, want %q", seenBody, body)
	}
	if n := seenKeys["ka"] + seenKeys["kb"]; n != 2 {
		t.Fatalf("webhook request must not carry an API key (saw %d keyed requests)", n)
	}
}

// TestClientTransportError: connection refused classifies as transport_error.
func TestClientTransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // port released: nothing listens
	c, err := NewClient(srv.URL, []string{"k"}, time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	res := c.Post(context.Background(), "/x", []byte(`{}`))
	if res.Class != classTransportErr {
		t.Fatalf("class = %s, want transport_error", res.Class)
	}
}

// TestNewClientValidation: bad URLs and key sets fail fast.
func TestNewClientValidation(t *testing.T) {
	if _, err := NewClient("ftp://x", []string{"k"}, time.Second, 1); err == nil {
		t.Fatal("expected scheme error")
	}
	if _, err := NewClient("http://127.0.0.1:1", nil, time.Second, 1); err == nil {
		t.Fatal("expected key error")
	}
	if _, err := NewClient("http://127.0.0.1:1", []string{"k"}, 0, 1); err == nil {
		t.Fatal("expected timeout error")
	}
	if _, err := NewClient("://bad", []string{"k"}, time.Second, 1); err == nil {
		t.Fatal("expected URL parse error")
	}
}

// TestWebhookRotationAcrossSources: with a multi-source pool, webhook posts
// all flow (round-robin over transports) — the per-IP limiter contract the
// rotation implements.
func TestWebhookRotationAcrossSources(t *testing.T) {
	var served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		w.WriteHeader(202)
	}))
	defer srv.Close()

	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	clients := make([]*http.Client, 4)
	for i := range clients {
		clients[i] = srv.Client()
	}
	c := &Client{
		base:       base,
		keys:       []string{"k"},
		timeout:    time.Second,
		authed:     srv.Client(),
		webhook:    clients,
		webhookSrc: []string{"a", "b", "c", "d"},
	}
	if c.ActiveSourceIPs() != 4 || c.ProbedSourceIPs() != 0 || c.SourceIPRotationDegraded() {
		t.Fatal("unexpected client pool state")
	}
	ctx := context.Background()
	for i := 0; i < 8; i++ {
		body := []byte(`{}`)
		if res := c.PostWebhook(ctx, "/wh", body, Sign("s", body)); res.Status != 202 {
			t.Fatalf("post %d = %d", i, res.Status)
		}
	}
	if got := served.Load(); got != 8 {
		t.Fatalf("server saw %d requests, want 8", got)
	}
}
