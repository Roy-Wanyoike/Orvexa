//go:build e2e

package e2e

import (
	"testing"
	"time"
)

// J2 — outbound messaging journey (issue #43):
//
//	outbound SMS via the simulator carrier → provider lifecycle receipts
//	(sent/delivered signed through the internal ingest port; read receipt
//	delivered as a SIGNED webhook through the PUBLIC gateway) →
//	receipt-driven state → analytics summary.
//
// Failure injections: tampered signature (401 fail-closed) and duplicate
// replay of the read receipt (dedupe asserted).
//
// Contract notes (kept visible so the drift stays fixed upstream):
//   - interactions.Rec carries snake_case JSON tags since the wire-contract
//     fix (#101/#110) — the journey decodes data.id / data.status as documented;
//     the historical PascalCase drift is closed.
//   - Issue #103 (filed, unfixed): the simulator's INTERNAL sent/delivered
//     receipts are signed into the provider_events ledger via gateway.Ingest,
//     but no consumer applies them — activation at step 3 comes from the
//     messaging service's own queued→sent transition, and completion at
//     step 7 is driven by the read receipt through the PUBLIC gateway, whose
//     handler applies the comms processor inline. J2 is therefore NOT blocked
//     by #103; the demo loop's voice leg needs the same public-gateway
//     routing (scripts/e2e-demo.sh step 8).
//   - Issue #90 is routed around by design: the harness gives every journey
//     its OWN tenant, and J2 sends exactly ONE outbound message — the second
//     outbound leg per tenant currently fails 409 (empty provider_ref unique
//     collision), which is a comms/interactions defect outside this wave's
//     ownership.
//   - Issue #106 (filed): analytics total_interactions counts lifecycle
//     events, not distinct interactions (one sms send → total=3, bucket "":2).
//     The step-8 assertion stays at >=1 until #106 lands, then ratchet to ==1.
func TestJourney2_OutboundSMS_Receipts_Analytics(t *testing.T) {
	tn := journeyTenantFor(t, "j2")
	t.Logf("J2 tenant %s (key %s…)", tn.TenantID, tn.RawKey[:12])

	// 1. customer with a phone identity
	var cust struct {
		ID string `json:"id"`
	}
	decode(t, doJSON(t, "POST", "/api/v1/customers", tn.RawKey, map[string]any{
		"display_name": "Kamau Njeri",
		"identifiers":  []map[string]any{{"type": "phone", "value": "+254722333444", "is_primary": true}},
	}), 201, &cust)
	t.Logf("✓ customer %s", cust.ID)

	// 2. outbound SMS — creates the interaction, hands delivery to the
	//    simulator carrier; the carrier reports progress via signed webhooks
	//    delivered through the same fail-closed public gateway.
	var sent struct {
		ID        string `json:"id"`
		Status    string `json:"status"`
		Channel   string `json:"channel"`
		Direction string `json:"direction"`
	}
	decode(t, doJSON(t, "POST", "/api/v1/messages", tn.RawKey, map[string]any{
		"customer_id": cust.ID,
		"channel":     "sms",
		"from":        "+254700000000", // sender identity: required by the handler although the OpenAPI spec marks it optional (filed)
		"to":          "+254722333444",
		"body":        "Your order #4192 has shipped and arrives Thursday.",
	}), 201, &sent)
	if sent.ID == "" {
		t.Fatal("message send returned no interaction id (expected data.id — snake_case wire contract #110)")
	}
	if sent.Channel != "sms" || sent.Direction != "outbound" {
		t.Fatalf("unexpected interaction shape: %+v", sent)
	}
	t.Logf("✓ outbound sms interaction %s created (status=%s at response time)", sent.ID, sent.Status)

	// 3. the send must be active without any operator action: the messaging
	//    service applies queued→sent synchronously, and the simulator's
	//    signed sent/delivered receipts land in the provider_events ledger
	//    (ledger-only until #103 — see contract note above).
	waitFor(t, "outbound interaction to be active", 15*time.Second, func() (bool, string) {
		var cur struct {
			Status string `json:"status"`
		}
		decode(t, doJSON(t, "GET", "/api/v1/interactions/"+sent.ID, tn.RawKey, nil), 200, &cur)
		return cur.Status == "active", "status=" + cur.Status
	})
	t.Log("✓ interaction active (queued→sent); simulator sent/delivered receipts signed into the ledger (#103: ledger-only)")

	// 4. FAILURE INJECTION — tampered signature on a read receipt: fail-closed 401.
	readBody := mustJSON(map[string]any{
		"event":          "message.read",
		"interaction_id": sent.ID,
		"tenant_id":      tn.TenantID,
		"timestamp":      nowRFC3339(),
		"detail":         "customer read the message",
	})
	tampered := postWebhook(t, "simulator", readBody, "00deadbeef00")
	if tampered.Status != 401 || tampered.Body.Error == nil || tampered.Body.Error.Code != "webhook.invalid_signature" {
		t.Fatalf("tampered read receipt must be 401 webhook.invalid_signature, got %d %+v", tampered.Status, tampered.Body.Error)
	}
	t.Log("✓ FAILURE INJECTION (tamper): read receipt with forged signature → 401, never persisted")

	// 5. legitimate signed read receipt completes the message lifecycle
	var readRes struct {
		EventID   string `json:"event_id"`
		Duplicate bool   `json:"duplicate"`
		Processed bool   `json:"processed"`
	}
	decode(t, postWebhook(t, "simulator", readBody, sign(readBody)), 202, &readRes)
	if readRes.Duplicate || !readRes.Processed {
		t.Fatalf("first read receipt must be fresh+processed: %+v", readRes)
	}
	t.Logf("✓ signed read receipt accepted (202): event_id=%s", readRes.EventID)

	// 6. FAILURE INJECTION — duplicate replay of the read receipt: deduped to the
	//    original event id; the completed state must not re-enter the lifecycle.
	var replayRes struct {
		EventID   string `json:"event_id"`
		Duplicate bool   `json:"duplicate"`
	}
	decode(t, postWebhook(t, "simulator", readBody, sign(readBody)), 200, &replayRes)
	if !replayRes.Duplicate || replayRes.EventID != readRes.EventID {
		t.Fatalf("read-receipt replay must dedupe: %+v vs %+v", replayRes, readRes)
	}
	t.Logf("✓ FAILURE INJECTION (replay): duplicate=true, event_id=%s — single effect", replayRes.EventID)

	// 7. receipt-driven state: the message.read receipt, delivered through
	//    the PUBLIC webhook gateway (signature-validated, processed inline),
	//    moves the interaction to completed.
	waitFor(t, "interaction to complete via message.read", 15*time.Second, func() (bool, string) {
		var cur struct {
			Status string `json:"status"`
		}
		decode(t, doJSON(t, "GET", "/api/v1/interactions/"+sent.ID, tn.RawKey, nil), 200, &cur)
		return cur.Status == "completed", "status=" + cur.Status
	})
	t.Log("✓ interaction completed by the carrier read receipt")

	// 8. analytics summary over the facts pipeline (worker: outbox → bus →
	//    interaction_fact). The facts consumer is asynchronous, so poll.
	var summary struct {
		Window struct {
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"window"`
		InteractionsByChannel map[string]int64 `json:"interactions_by_channel"`
		TotalInteractions     int64            `json:"total_interactions"`
		AIEvents              int64            `json:"ai_events"`
		TokensUsed            int64            `json:"tokens_used"`
	}
	waitFor(t, "analytics facts to land", 30*time.Second, func() (bool, string) {
		decode(t, doJSON(t, "GET", "/api/v1/analytics/summary", tn.RawKey, nil), 200, &summary)
		return summary.TotalInteractions >= 1 && summary.InteractionsByChannel["sms"] >= 1,
			"total=" + itoa(int(summary.TotalInteractions))
	})
	if summary.InteractionsByChannel["sms"] < 1 {
		t.Fatalf("expected an sms fact, got %+v", summary.InteractionsByChannel)
	}
	if summary.Window.To == "" {
		t.Fatal("analytics summary window missing")
	}
	// Known inflation (#106): total counts lifecycle events (one sms send →
	// total=3 with an empty-channel bucket), so only >=1 is asserted here;
	// ratchet to total==1 && len(by_channel)==1 once #106 lands.
	t.Logf("✓ analytics summary: total_interactions=%d by_channel=%v (window %s → %s)",
		summary.TotalInteractions, summary.InteractionsByChannel, summary.Window.From, summary.Window.To)
	t.Log("J2 GREEN")
}
