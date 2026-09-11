//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// J1 — inbound WhatsApp journey (issue #43):
//
//	customer identity → inbound WhatsApp interaction (conversation auto-opened)
//	→ signed provider lifecycle webhook → routing decision → agent assignment
//	→ wrap-up → case open/link/close → conversation close.
//
// Failure injections: duplicate webhook replay (dedupe asserted, single
// effect) and a tampered signature (fail-closed 401).
//
// Contract notes (issue #89 findings, kept visible so the drift stays fixed):
//   - POST /api/v1/interactions is documented with snake_case body fields in
//     api/openapi/orvexa-v1.yaml. The handler drift (CreateInput without JSON
//     tags) was fixed by the snake_case wire contract (#101/#110): the journey
//     now sends the documented snake_case body and decodes the snake_case
//     response, so OpenAPI ⇄ wire ⇄ journey all agree.
//   - The comms processor vocabulary has no "inbound.whatsapp" topic: inbound
//     provider events must reference an existing interaction id and use the
//     lifecycle topics (message.delivered/message.read/…), so the journey
//     creates the inbound interaction first and then delivers the signed
//     provider event for it — exactly what the whatsapp_cloud adapter does.
func TestJourney1_InboundWhatsApp_Routing_Assignment_Wrapup_CaseClose(t *testing.T) {
	tn := journeyTenantFor(t, "j1")
	t.Logf("J1 tenant %s (key %s…)", tn.TenantID, tn.RawKey[:12])

	// 0. stack truth: readiness reports component state
	var ready struct {
		Status string `json:"status"`
	}
	decode(t, doJSON(t, "GET", "/readyz", "", nil), 200, &ready)
	if ready.Status != "ready" {
		t.Fatalf("stack not ready: %s", ready.Status)
	}
	t.Log("✓ stack ready (db up)")

	// 1. customer identity with channel identifiers (whatsapp primary + phone)
	var cust struct {
		ID string `json:"id"`
	}
	decode(t, doJSON(t, "POST", "/api/v1/customers", tn.RawKey, map[string]any{
		"display_name": "Jane Wanjiku",
		"identifiers": []map[string]any{
			{"type": "whatsapp", "value": "+254712345678", "is_primary": true},
			{"type": "phone", "value": "+254712345678"},
		},
	}), 201, &cust)
	if cust.ID == "" {
		t.Fatal("customer id empty")
	}
	t.Logf("✓ customer %s created", cust.ID)

	// 2. identity resolution: the phone identifier normalizes to the same customer
	var resolved struct {
		ID string `json:"id"`
	}
	decode(t, doJSON(t, "POST", "/api/v1/customers/resolve", tn.RawKey, map[string]any{
		"type": "phone", "value": "254712345678", // no plus, no country formatting
	}), 200, &resolved)
	if resolved.ID != cust.ID {
		t.Fatalf("resolve returned %s, want %s (E.164 normalization broken)", resolved.ID, cust.ID)
	}
	t.Log("✓ identifier resolution: 254712345678 → +254712345678 → same customer")

	// 3. inbound WhatsApp interaction — conversation auto-opened (continuous context)
	//    Documented snake_case body (#101/#110); no workaround keys.
	inter := doJSON(t, "POST", "/api/v1/interactions", tn.RawKey, map[string]any{
		"customer_id": cust.ID,
		"channel":     "whatsapp",
		"direction":   "inbound",
		"source":      "+254712345678",
		"destination": "+254700000000",
	})
	var rec struct {
		ID             string `json:"id"`
		ConversationID string `json:"conversation_id"`
		Status         string `json:"status"`
		Channel        string `json:"channel"`
		Direction      string `json:"direction"`
	}
	decode(t, inter, 201, &rec)
	if rec.ID == "" || rec.ConversationID == "" {
		t.Fatalf("interaction not created: %+v", rec)
	}
	if rec.Status != "pending" || rec.Channel != "whatsapp" || rec.Direction != "inbound" {
		t.Fatalf("unexpected interaction state: %+v", rec)
	}
	t.Logf("✓ inbound whatsapp interaction %s (conversation %s, status %s)", rec.ID, rec.ConversationID, rec.Status)

	// 4. signed provider webhook: message.delivered drives pending→active through
	//    the processor — the exact event a real whatsapp_cloud adapter translates.
	eventBody := mustJSON(map[string]any{
		"event":          "message.delivered",
		"interaction_id": rec.ID,
		"tenant_id":      tn.TenantID,
		"timestamp":      nowRFC3339(),
		"detail":         "journey j1 inbound delivery",
	})
	sig := sign(eventBody)
	var hookRes struct {
		EventID   string `json:"event_id"`
		Duplicate bool   `json:"duplicate"`
		Accepted  bool   `json:"accepted"`
		Processed bool   `json:"processed"`
	}
	decode(t, postWebhook(t, "whatsapp_cloud", eventBody, sig), 202, &hookRes)
	if !hookRes.Accepted || hookRes.Duplicate || !hookRes.Processed || hookRes.EventID == "" {
		t.Fatalf("first delivery must be accepted+processed, got %+v", hookRes)
	}
	t.Logf("✓ signed webhook accepted (202): event_id=%s processed=true", hookRes.EventID)

	// 5. FAILURE INJECTION — duplicate replay: same bytes, same signature. The
	//    gateway must dedupe (single effect): duplicate=true, SAME event id, 200.
	var replayRes struct {
		EventID   string `json:"event_id"`
		Duplicate bool   `json:"duplicate"`
	}
	decode(t, postWebhook(t, "whatsapp_cloud", eventBody, sig), 200, &replayRes)
	if !replayRes.Duplicate || replayRes.EventID != hookRes.EventID {
		t.Fatalf("replay must dedupe to the original event id: %+v vs %+v", replayRes, hookRes)
	}
	t.Logf("✓ FAILURE INJECTION (replay): duplicate=true, event_id=%s (single effect)", replayRes.EventID)

	// 6. FAILURE INJECTION — tampered signature: fail-closed 401, no state change.
	tampered := postWebhook(t, "whatsapp_cloud", eventBody, "deadbeef")
	if tampered.Status != 401 || tampered.Body.Error == nil || tampered.Body.Error.Code != "webhook.invalid_signature" {
		t.Fatalf("tampered signature must be 401 webhook.invalid_signature, got %d %+v", tampered.Status, tampered.Body.Error)
	}
	t.Log("✓ FAILURE INJECTION (tamper): 401 webhook.invalid_signature — ingress is fail-closed")

	// 7. the provider event landed: interaction is now active
	var active struct {
		Status string `json:"status"`
	}
	waitFor(t, "interaction to become active", 15*time.Second, func() (bool, string) {
		decode(t, doJSON(t, "GET", "/api/v1/interactions/"+rec.ID, tn.RawKey, nil), 200, &active)
		return active.Status == "active", "status=" + active.Status
	})
	t.Log("✓ interaction active after signed provider event")

	// 8. workforce: agent with billing skill, made available
	var agent struct {
		ID string `json:"id"`
	}
	decode(t, doJSON(t, "POST", "/api/v1/agents", tn.RawKey, map[string]any{
		"external_identity": "amara@acme.test",
		"display_name":      "Amara",
		"language":          "sw",
		"skills":            []map[string]any{{"skill": "billing", "level": 4}},
	}), 201, &agent)
	var presence struct {
		Status string `json:"status"`
	}
	decode(t, doJSON(t, "PUT", "/api/v1/agents/"+agent.ID+"/presence", tn.RawKey, map[string]any{"status": "available"}), 200, &presence)
	if presence.Status != "available" {
		t.Fatalf("presence = %s, want available", presence.Status)
	}
	t.Logf("✓ agent %s available (skills: billing, language: sw)", agent.ID)

	// 9. routing decision — deterministic engine, assignment recorded atomically
	var decision struct {
		ID              string `json:"id"`
		Outcome         string `json:"outcome"`
		AssignedAgentID string `json:"assigned_agent_id"`
		Candidates      []struct {
			AgentID   string  `json:"agent_id"`
			Score     float64 `json:"score"`
			Available bool    `json:"available"`
		} `json:"candidates"`
	}
	decode(t, doJSON(t, "POST", "/api/v1/routing/interactions/"+rec.ID, tn.RawKey, map[string]any{
		"required_skills": []string{"billing"},
		"language":        "sw",
		"priority":        5,
	}), 200, &decision)
	if decision.Outcome != "assigned_agent" || decision.AssignedAgentID != agent.ID {
		t.Fatalf("routing outcome=%s agent=%s, want assigned_agent/%s (candidates=%+v)",
			decision.Outcome, decision.AssignedAgentID, agent.ID, decision.Candidates)
	}
	t.Logf("✓ routing decision %s: outcome=assigned_agent → agent %s", decision.ID, decision.AssignedAgentID)

	// 10. the decision is an immutable record visible to supervisors
	var decisions []struct {
		ID              string `json:"id"`
		InteractionID   string `json:"interaction_id"`
		Outcome         string `json:"outcome"`
		AssignedAgentID string `json:"assigned_agent_id"`
	}
	decode(t, doJSON(t, "GET", "/api/v1/routing/decisions?interaction_id="+rec.ID, tn.RawKey, nil), 200, &decisions)
	if len(decisions) == 0 || decisions[0].ID != decision.ID {
		t.Fatalf("routing decision not recorded: %+v", decisions)
	}
	t.Log("✓ decision record queryable via /routing/decisions")

	// 11. assignment applied to the interaction
	var assigned struct {
		AssignedAgentID string `json:"assigned_agent_id"`
		Status          string `json:"status"`
	}
	decode(t, doJSON(t, "GET", "/api/v1/interactions/"+rec.ID, tn.RawKey, nil), 200, &assigned)
	if assigned.AssignedAgentID != agent.ID {
		t.Fatalf("interaction assigned_agent=%q, want %q", assigned.AssignedAgentID, agent.ID)
	}
	if assigned.Status != "active" {
		t.Fatalf("routing must not move the interaction out of active, got %s", assigned.Status)
	}
	t.Log("✓ interaction carries AssignedAgentID after routing")

	// 12. wrap-up: customer left, after-call work begins
	var wrapped struct {
		Status string `json:"status"`
	}
	decode(t, doJSON(t, "POST", "/api/v1/interactions/"+rec.ID+"/transition", tn.RawKey, map[string]any{"to": "wrapup"}), 200, &wrapped)
	if wrapped.Status != "wrapup" {
		t.Fatalf("transition to wrapup returned %s", wrapped.Status)
	}
	t.Log("✓ interaction wrapped up (active → wrapup)")

	// 13. case: open → link the interaction → resolve → close
	var kase struct {
		ID       string `json:"id"`
		Ref      string `json:"ref"`
		Status   string `json:"status"`
		Subject  string `json:"subject"`
		Customer string `json:"customer_id"`
	}
	decode(t, doJSON(t, "POST", "/api/v1/cases", tn.RawKey, map[string]any{
		"customer_id": cust.ID, "subject": "Billing dispute", "priority": "high",
	}), 201, &kase)
	if kase.ID == "" || kase.Ref == "" {
		t.Fatalf("case not created: %+v", kase)
	}
	decode(t, doJSON(t, "POST", "/api/v1/cases/"+kase.ID+"/interactions/"+rec.ID+"/link", tn.RawKey, nil), 204, nil)
	t.Logf("✓ case %s (%s) opened and interaction linked", kase.Ref, kase.ID)

	for _, step := range []struct{ to, reason string }{
		{"in_progress", "agent picked up"},
		{"resolved", "billing corrected"},
		{"closed", "customer confirmed"},
	} {
		var moved struct {
			Status string `json:"status"`
		}
		decode(t, doJSON(t, "POST", "/api/v1/cases/"+kase.ID+"/transition", tn.RawKey,
			map[string]any{"to": step.to, "reason": step.reason}), 200, &moved)
		if moved.Status != step.to {
			t.Fatalf("case transition to %s returned %s", step.to, moved.Status)
		}
	}
	t.Log("✓ case closed (open → in_progress → resolved → closed)")

	// 14. interaction completed after the work is done
	var completed struct {
		Status string `json:"status"`
	}
	decode(t, doJSON(t, "POST", "/api/v1/interactions/"+rec.ID+"/transition", tn.RawKey, map[string]any{
		"to": "completed", "end_reason": "resolved",
	}), 200, &completed)
	if completed.Status != "completed" {
		t.Fatalf("transition to completed returned %s", completed.Status)
	}

	// 15. conversation closed — the continuous context ends resolved
	var conv struct {
		Status string `json:"status"`
	}
	decode(t, doJSON(t, "POST", "/api/v1/conversations/"+rec.ConversationID+"/close", tn.RawKey,
		map[string]any{"reason": "resolved"}), 200, &conv)
	if conv.Status != "closed" {
		t.Fatalf("conversation close returned %s", conv.Status)
	}
	t.Log("✓ interaction completed + conversation closed — J1 GREEN")
}

// mustJSON marshals v or fails the test.
func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("marshal webhook body: %v", err))
	}
	return raw
}
