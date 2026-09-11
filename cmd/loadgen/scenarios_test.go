package main

// Unit tests for scenario builders, seeding (against a fake Orvexa API) and
// the ramp runner (fixed-rate pacing, graceful cancellation, classification).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOrvexa answers the seed + scenario surface with the wire shapes the
// server actually produces: customers carry snake_case tags, interactions.Rec
// has none (Go-cased keys). It also enforces the webhook signature like the
// real gateway (fail-closed).
type fakeOrvexa struct {
	secret string

	mu            sync.Mutex
	custN         int
	intN          int
	webhookBodies []string
	badSigs       int
}

func (f *fakeOrvexa) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/customers" && r.Method == http.MethodPost:
			f.mu.Lock()
			f.custN++
			n := f.custN
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"data":{"id":"c-%d","tenant_id":"t-1","display_name":"x"}}`, n)
		case r.URL.Path == "/api/v1/customers" && r.Method == http.MethodGet:
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"data":[{"id":"c-1"}],"meta":{"next_cursor":""}}`)
		case r.URL.Path == "/api/v1/interactions" && r.Method == http.MethodPost:
			f.mu.Lock()
			f.intN++
			n := f.intN
			f.mu.Unlock()
			// interactions.Rec marshals without json tags: Go-cased keys.
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"data":{"ID":"i-%d","TenantID":"t-1","Status":"pending","ConversationID":"v-%d"}}`, n, n)
		case strings.HasPrefix(r.URL.Path, "/api/v1/interactions/") && strings.HasSuffix(r.URL.Path, "/transition"):
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"data":{"ID":"i-x","Status":"active"}}`)
		case strings.HasPrefix(r.URL.Path, "/api/v1/webhooks/") && r.Method == http.MethodPost:
			raw, _ := io.ReadAll(r.Body)
			if Sign(f.secret, raw) != r.Header.Get(SignatureHeader) {
				f.mu.Lock()
				f.badSigs++
				f.mu.Unlock()
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"error":{"code":"webhook.invalid_signature","message":"signature validation failed"}}`)
				return
			}
			f.mu.Lock()
			f.webhookBodies = append(f.webhookBodies, string(raw))
			f.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"data":{"event_id":"e-1","duplicate":false,"accepted":true,"processed":true}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"code":"not_found","message":"nope"}}`)
		}
	}
}

// TestSeedAndIssueOne: seeding populates the pools (tenant, customers,
// active interactions) and every scenario issues against the right route.
func TestSeedAndIssueOne(t *testing.T) {
	fake := &fakeOrvexa{secret: "sekrit"}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	c, err := NewClient(srv.URL, []string{"k1", "k2"}, 2*time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	names := []string{scnWebhook, scnInteraction, scnCustomerCreate, scnCustomerList}
	work, err := seed(context.Background(), c, "sekrit", names, 8, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if work.tenantID != "t-1" {
		t.Fatalf("tenant = %q", work.tenantID)
	}
	if got := work.CustomerCount(); got != 8 {
		t.Fatalf("customers = %d, want 8", got)
	}
	if len(work.interactions) != 8 {
		t.Fatalf("interactions = %d, want 8", len(work.interactions))
	}

	// webhook: signed body over a seeded interaction; the fake verifies HMAC.
	res := issueOne(context.Background(), c, work, scnWebhook)
	if res.Status != http.StatusAccepted || res.Class != classOK {
		t.Fatalf("webhook result = %+v", res)
	}
	fake.mu.Lock()
	bodies := append([]string(nil), fake.webhookBodies...)
	bad := fake.badSigs
	fake.mu.Unlock()
	if bad != 0 {
		t.Fatalf("fake gateway saw %d bad signatures", bad)
	}
	if len(bodies) != 1 {
		t.Fatalf("expected 1 webhook delivery, got %d", len(bodies))
	}
	var ev map[string]any
	if err := json.Unmarshal([]byte(bodies[0]), &ev); err != nil {
		t.Fatal(err)
	}
	if ev["event"] != "message.delivered" || ev["tenant_id"] != "t-1" || ev["interaction_id"] == "" || ev["nonce"] == "" {
		t.Fatalf("webhook body shape wrong: %v", ev)
	}
	if id, _ := ev["interaction_id"].(string); !strings.HasPrefix(id, "i-") {
		t.Fatalf("interaction_id %v not from the seeded pool", ev["interaction_id"])
	}

	// Each remaining scenario reaches its route with the expected outcome.
	if res := issueOne(context.Background(), c, work, scnInteraction); res.Status != http.StatusCreated {
		t.Fatalf("interaction = %+v", res)
	}
	if res := issueOne(context.Background(), c, work, scnCustomerCreate); res.Status != http.StatusCreated {
		t.Fatalf("customer-create = %+v", res)
	}
	if res := issueOne(context.Background(), c, work, scnCustomerList); res.Status != http.StatusOK {
		t.Fatalf("customer-list = %+v", res)
	}
}

// TestBodyBuildersUniqueness: webhook nonces and customer phones are unique
// per call; the interaction body binds on Go-cased keys (server contract).
func TestBodyBuildersUniqueness(t *testing.T) {
	w := &workload{secret: "s"}
	w.tenantID = "t-9"
	w.mu.Lock()
	w.customers = []string{"c-1"}
	w.phones = []string{"+254700000001"}
	w.mu.Unlock()

	phones := map[string]bool{}
	for i := 0; i < 200; i++ {
		var body map[string]any
		if err := json.Unmarshal(buildCustomerBody(w), &body); err != nil {
			t.Fatal(err)
		}
		ids := body["identifiers"].([]any)[0].(map[string]any)
		phone := ids["value"].(string)
		if phones[phone] {
			t.Fatalf("duplicate phone %s", phone)
		}
		phones[phone] = true
		if !strings.HasPrefix(phone, "+2547") {
			t.Fatalf("phone %s not E.164-shaped", phone)
		}
	}
	// The measured stream must never collide with the seed range.
	for p := range phones {
		if p <= "+254700000064" && p >= "+254700000000" {
			t.Fatalf("measured phone %s collides with seed range", p)
		}
	}

	w.interactions = []string{"i-1", "i-2"}
	nonces := map[string]bool{}
	for i := 0; i < 100; i++ {
		var ev map[string]any
		if err := json.Unmarshal(buildWebhookBody(w, w.nextInteraction()), &ev); err != nil {
			t.Fatal(err)
		}
		n := ev["nonce"].(string)
		if nonces[n] {
			t.Fatalf("duplicate webhook nonce %s", n)
		}
		nonces[n] = true
	}

	intBody := buildInteractionBody(w)
	var m map[string]any
	if err := json.Unmarshal(intBody, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"CustomerID", "Channel", "Direction", "Source", "Destination"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("interaction body missing Go-cased key %q: %v", key, m)
		}
	}
	if m["Channel"] != "whatsapp" || m["Direction"] != "inbound" {
		t.Fatalf("interaction body values wrong: %v", m)
	}
}

// TestParseScenariosAndRamp: flag parsing surface.
func TestParseScenariosAndRamp(t *testing.T) {
	all, err := parseScenarios("all")
	if err != nil || len(all) != 4 {
		t.Fatalf("all = %v, %v", all, err)
	}
	sub, err := parseScenarios("webhook, customer-list")
	if err != nil || len(sub) != 2 || sub[0] != scnWebhook || sub[1] != scnCustomerList {
		t.Fatalf("subset = %v, %v", sub, err)
	}
	if _, err := parseScenarios("bogus"); err == nil {
		t.Fatal("expected scenario error")
	}
	if _, err := parseScenarios(""); err == nil {
		t.Fatal("expected empty scenario error")
	}
	ramp, err := parseRamp("50, 100,200")
	if err != nil || len(ramp) != 3 || ramp[0] != 50 || ramp[2] != 200 {
		t.Fatalf("ramp = %v, %v", ramp, err)
	}
	if _, err := parseRamp(""); err == nil {
		t.Fatal("expected empty ramp error")
	}
	if _, err := parseRamp("x"); err == nil {
		t.Fatal("expected ramp parse error")
	}
	if _, err := parseRamp("-5"); err == nil {
		t.Fatal("expected ramp range error")
	}
}

// TestRunnerStepFixedRate: a healthy fake sees ~rps*dur issued requests, all
// classified, with sane latency stats.
func TestRunnerStepFixedRate(t *testing.T) {
	fake := &fakeOrvexa{secret: "sekrit"}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	c, err := NewClient(srv.URL, []string{"k"}, time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	work := &workload{secret: "sekrit", tenantID: "t-1"}
	work.mu.Lock()
	work.customers = []string{"c-1"}
	work.phones = []string{"+254700000001"}
	work.interactions = []string{"i-1"}
	work.mu.Unlock()

	r := newRunner(c, work, []string{scnWebhook, scnCustomerList}, 1024)
	step, err := r.step(context.Background(), 1, 100, 300*time.Millisecond, false)
	if err != nil {
		t.Fatal(err)
	}
	// ~100 rps x 0.3s = ~30 (ticker granularity tolerated)
	if step.Total.Issued < 25 || step.Total.Issued > 35 {
		t.Fatalf("issued = %d, want ~30", step.Total.Issued)
	}
	if step.Total.OK != step.Total.Issued || step.Total.Errors != 0 {
		t.Fatalf("totals = %+v", step.Total)
	}
	if step.Total.ErrorRate != 0 {
		t.Fatalf("error rate = %v", step.Total.ErrorRate)
	}
	wh := step.Scenarios[scnWebhook]
	cl := step.Scenarios[scnCustomerList]
	if wh.Issued == 0 || cl.Issued == 0 || wh.Issued+cl.Issued != step.Total.Issued {
		t.Fatalf("scenario split wrong: webhook=%d list=%d total=%d", wh.Issued, cl.Issued, step.Total.Issued)
	}
	if wh.LatencyMS.P50 <= 0 || wh.LatencyMS.N != wh.Issued {
		t.Fatalf("webhook hist = %+v (issued %d)", wh.LatencyMS, wh.Issued)
	}
	if step.Total.AchievedRPS < 80 || step.Total.AchievedRPS > 120 {
		t.Fatalf("achieved rps = %v", step.Total.AchievedRPS)
	}
	if step.Index != 1 || step.TargetRPS != 100 || step.Warmup {
		t.Fatalf("step header wrong: %+v", step)
	}
}

// TestRunnerGracefulStop: cancelling mid-step returns promptly with partial
// counts and no hanging — the contract behind the CLI's SIGINT handling.
func TestRunnerGracefulStop(t *testing.T) {
	fake := &fakeOrvexa{secret: "sekrit"}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	c, err := NewClient(srv.URL, []string{"k"}, time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	work := &workload{secret: "sekrit", tenantID: "t-1"}
	work.mu.Lock()
	work.customers = []string{"c-1"}
	work.phones = []string{"+254700000001"}
	work.interactions = []string{"i-1"}
	work.mu.Unlock()

	ctx, stop := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		stop() // stands in for the SIGINT handler in main
	}()
	defer stop()

	r := newRunner(c, work, []string{scnWebhook}, 1024)
	start := time.Now()
	step, err := r.step(ctx, 1, 200, 10*time.Second, false)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("graceful stop took %v — not graceful", elapsed)
	}
	if step.Total.Issued >= 2000 {
		t.Fatalf("issued %d — cancellation did not stop issuance", step.Total.Issued)
	}
	if step.Total.OK == 0 {
		t.Fatalf("no completed requests recorded: %+v", step.Total)
	}
}
