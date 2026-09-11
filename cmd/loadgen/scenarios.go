package main

// Scenarios: seeding, per-request builders and the stepped-rate runner.
// Field-name notes are load-bearing:
//
//   - customers.CreateInput carries json tags — snake_case bodies bind.
//   - interactions.CreateInput carries NO json tags — encoding/json binds
//     case-insensitively on the Go field names, so the wire keys are
//     "CustomerID"/"Channel"/"Direction"/"Source"/"Destination"; spec-style
//     snake_case keys are rejected (DisallowUnknownFields). The loadgen
//     speaks the binding the server actually enforces and the drift is
//     reported in docs/slo.md + docs/perf/.
//   - interactions.Rec has no tags either — responses marshal as "ID" etc.;
//     the seed parser therefore matches keys case-insensitively.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// workload is the shared mutable state of a campaign (seeded ids + counters).
type workload struct {
	secret string

	mu           sync.Mutex
	customers    []string // ids, used for interaction-create rotation
	phones       []string // per-customer source endpoints
	tenantID     string
	interactions []string // active interaction ids, webhook targets

	custSeq  atomic.Uint64
	phoneSeq atomic.Uint64 // starts above the seed range so identifier uniques never collide
	whSeq    atomic.Uint64
}

// CustomerCount reports the seeded customer pool size.
func (w *workload) CustomerCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.customers)
}

// nextCustomer rotates the seeded customer pool (round-robin).
func (w *workload) nextCustomer() (id, phone string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.customers) == 0 {
		return "", ""
	}
	i := int(w.custSeq.Add(1)-1) % len(w.customers)
	return w.customers[i], w.phones[i]
}

// nextInteraction rotates the seeded interaction pool (round-robin).
func (w *workload) nextInteraction() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.interactions) == 0 {
		return ""
	}
	i := int(w.whSeq.Add(1)-1) % len(w.interactions)
	return w.interactions[i]
}

// buildWebhookBody mints one unique WhatsApp-shaped ProviderEvent: a
// message.delivered receipt for a seeded active interaction, with a fresh
// nonce per request so the gateway always takes the fresh (non-duplicate)
// ingest path. Extra keys (nonce) are ignored by the server's processor.
func buildWebhookBody(w *workload, interactionID string) []byte {
	body := map[string]string{
		"event":          "message.delivered",
		"interaction_id": interactionID,
		"tenant_id":      w.tenantID,
		"timestamp":      time.Now().UTC().Format(time.RFC3339Nano),
		"detail":         "loadgen",
		"nonce":          nonce(),
	}
	raw, _ := json.Marshal(body)
	return raw
}

// buildInteractionBody mints one interaction create for a seeded customer.
func buildInteractionBody(w *workload) []byte {
	id, phone := w.nextCustomer()
	body := map[string]string{
		"CustomerID":  id,
		"Channel":     "whatsapp",
		"Direction":   "inbound",
		"Source":      phone,
		"Destination": "+254700000000",
	}
	raw, _ := json.Marshal(body)
	return raw
}

// buildCustomerBody mints one customer create with a unique phone identifier
// (matches customers.CreateInput's snake_case tags). The counter starts at
// 1,000,000 — well above the seeded +254700xxxxxx range — so the tenant's
// identifier uniqueness never collides with seed data.
func buildCustomerBody(w *workload) []byte {
	n := w.phoneSeq.Add(1) + 999_999
	body := map[string]any{
		"display_name": "Load " + strconv.FormatUint(n, 10),
		"identifiers": []map[string]any{
			{"type": "phone", "value": fmt.Sprintf("+2547%08d", n%100_000_000), "is_primary": true},
		},
	}
	raw, _ := json.Marshal(body)
	return raw
}

// issueOne sends exactly one request for the named scenario.
func issueOne(ctx context.Context, c *Client, w *workload, name string) Result {
	var res Result
	switch name {
	case scnWebhook:
		id := w.nextInteraction()
		body := buildWebhookBody(w, id)
		res = c.PostWebhook(ctx, "/api/v1/webhooks/whatsapp_cloud", body, Sign(w.secret, body))
	case scnInteraction:
		res = c.Post(ctx, "/api/v1/interactions", buildInteractionBody(w))
	case scnCustomerCreate:
		res = c.Post(ctx, "/api/v1/customers", buildCustomerBody(w))
	case scnCustomerList:
		res = c.Get(ctx, "/api/v1/customers?limit=50")
	default:
		panic("unknown scenario " + name) // parseScenarios guarantees membership
	}
	res.Scenario = name
	return res
}

// ---- seeding ----

// seed prepares the pools the scenarios draw from. Seed traffic is paced so a
// single-key setup stays under the shipped 600/min auth limiter; it is never
// part of the measured window.
func seed(ctx context.Context, c *Client, secret string, names []string, pool int, budget time.Duration) (*workload, error) {
	w := &workload{secret: secret}
	wantCustomers := false
	wantInteractions := false
	for _, n := range names {
		switch n {
		case scnWebhook:
			wantInteractions = true
			wantCustomers = true // webhook interactions need a customer too
		case scnInteraction, scnCustomerCreate, scnCustomerList:
			wantCustomers = true
		}
	}
	if !wantCustomers && !wantInteractions {
		return w, nil
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	// Spread seed traffic over the key set so even a single-key setup stays
	// under the shipped 600/min burst-120 auth limiter.
	interval := time.Duration(100/maxInt(1, len(c.keys))) * time.Millisecond
	if interval < 2*time.Millisecond {
		interval = 2 * time.Millisecond
	}

	if wantCustomers {
		// The first customer's response yields the tenant id the webhook
		// scenario must stamp into ProviderEvent payloads.
		body, _ := json.Marshal(map[string]any{
			"display_name": "Seed 0",
			"identifiers":  []map[string]any{{"type": "phone", "value": "+254700000001", "is_primary": true}},
		})
		res, payload := c.PostRead(ctx, "/api/v1/customers", body)
		data, err := respData(res, payload, http.StatusCreated)
		if err != nil {
			return nil, fmt.Errorf("seed customer 0: %w", err)
		}
		w.tenantID = fieldCI(data, "tenant_id", "TenantID")
		w.mu.Lock()
		w.customers = append(w.customers, fieldCI(data, "id", "ID"))
		w.phones = append(w.phones, "+254700000001")
		w.mu.Unlock()

		for i := 1; i < pool; i++ {
			body, _ := json.Marshal(map[string]any{
				"display_name": "Seed " + strconv.Itoa(i),
				"identifiers":  []map[string]any{{"type": "phone", "value": fmt.Sprintf("+254700%06d", i), "is_primary": true}},
			})
			res, payload := c.PostRead(ctx, "/api/v1/customers", body)
			data, err := respData(res, payload, http.StatusCreated)
			if err != nil {
				return nil, fmt.Errorf("seed customer %d: %w", i, err)
			}
			w.mu.Lock()
			w.customers = append(w.customers, fieldCI(data, "id", "ID"))
			w.phones = append(w.phones, fmt.Sprintf("+254700%06d", i))
			w.mu.Unlock()
			sleepCtx(ctx, interval)
		}
	}

	if wantInteractions {
		for i := 0; i < pool; i++ {
			body := buildInteractionBody(w)
			res, payload := c.PostRead(ctx, "/api/v1/interactions", body)
			data, err := respData(res, payload, http.StatusCreated)
			if err != nil {
				return nil, fmt.Errorf("seed interaction %d: %w", i, err)
			}
			id := fieldCI(data, "id", "ID")
			// Move pending → active so measured webhook deliveries take the
			// documented same-state no-op path (the legal transition happens
			// here, unmeasured).
			tr, _ := json.Marshal(map[string]string{"to": "active"})
			if tres := c.Post(ctx, "/api/v1/interactions/"+id+"/transition", tr); tres.Status != http.StatusOK {
				return nil, fmt.Errorf("seed interaction %d: transition %s", i, tres.Class)
			}
			w.mu.Lock()
			w.interactions = append(w.interactions, id)
			w.mu.Unlock()
			sleepCtx(ctx, interval)
		}
	}
	return w, nil
}

// respData decodes the success envelope's data object.
func respData(res Result, payload []byte, wantStatus int) (map[string]any, error) {
	if res.Status != wantStatus {
		return nil, fmt.Errorf("status %d (%s)", res.Status, res.Class)
	}
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, fmt.Errorf("response decode: %w", err)
	}
	if env.Data == nil {
		return nil, fmt.Errorf("response envelope carries no data")
	}
	return env.Data, nil
}

// fieldCI reads a string field case-insensitively (interactions.Rec has no
// json tags and marshals Go-cased keys).
func fieldCI(m map[string]any, names ...string) string {
	for _, n := range names {
		if v, ok := m[n].(string); ok {
			return v
		}
	}
	return ""
}

// sleepCtx sleeps or returns early when the context is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// maxInt is the int max (kept explicit to avoid shadowing the builtin).
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
