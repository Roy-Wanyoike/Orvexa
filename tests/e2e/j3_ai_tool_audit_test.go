//go:build e2e

package e2e

import (
	"testing"
	"time"
)

// J3 — intelligence-plane journey (issue #43):
//
//	AI suggest (metered) → tool call through the tool gateway (audited)
//	→ audit-trail query.
//
// Failure injection: a tool NOT on the agent's allowlist is refused (403,
// executed=false) and the refusal itself must be audited — "every decision is
// audited, including refusals" (internal/tools contract).
//
// Contract notes (handler drift kept visible; filed upstream):
//   - POST /api/v1/ai/agents/{id}/invoke ignores the {id} path parameter —
//     the runtime resolves the agent from the BODY agent_id, so the journey
//     sends both (the OpenAPI spec documents neither agent_id nor the
//     path-parameter semantics).
//   - POST /api/v1/ai/agents/{id}/tools decodes tools.Call (no JSON tags) with
//     DisallowUnknownFields, so the spec's optional interaction_id field is
//     rejected 422 — the journey omits it.
//   - The audit trail has no HTTP read endpoint yet; the journey queries the
//     audit_events read model directly (worker audit consumer), which is the
//     same trail an operator would query.
func TestJourney3_AI_Suggest_ToolCall_AuditTrail(t *testing.T) {
	tn := journeyTenantFor(t, "j3")
	t.Logf("J3 tenant %s agent %s (key %s…)", tn.TenantID, tn.AgentID, tn.RawKey[:12])

	// 0. stack readiness
	var ready struct {
		Status string `json:"status"`
	}
	decode(t, doJSON(t, "GET", "/readyz", "", nil), 200, &ready)
	if ready.Status != "ready" {
		t.Fatalf("stack not ready: %s", ready.Status)
	}

	// 1. AI agent configuration is resolvable and active with its allowlist
	var agent struct {
		ID       string         `json:"id"`
		Status   string         `json:"status"`
		Tools    map[string]int `json:"tools"`
		Provider string         `json:"model_hint"`
	}
	decode(t, doJSON(t, "GET", "/api/v1/ai/agents/"+tn.AgentID, tn.RawKey, nil), 200, &agent)
	if agent.Status != "active" || agent.Tools["get_customer"] <= 0 {
		t.Fatalf("agent config wrong: %+v", agent)
	}
	t.Logf("✓ AI agent %s active (model_hint=%s, tools=%v)", agent.ID, agent.Provider, agent.Tools)

	// 2. grounded customer to reason over
	var cust struct {
		ID string `json:"id"`
	}
	decode(t, doJSON(t, "POST", "/api/v1/customers", tn.RawKey, map[string]any{
		"display_name": "Zawadi Neema",
		"identifiers":  []map[string]any{{"type": "whatsapp", "value": "+254733222111", "is_primary": true}},
	}), 201, &cust)

	// 3. AI suggest — one governed, METERED turn. The deterministic rules
	//    provider derives a next-best-action from grounded facts.
	var result struct {
		Text       string `json:"text"`
		Suggestion *struct {
			Kind   string         `json:"kind"`
			Title  string         `json:"title"`
			Params map[string]any `json:"params"`
		} `json:"suggestion"`
		InputTokens  int    `json:"input_tokens"`
		OutputTokens int    `json:"output_tokens"`
		Outcome      string `json:"outcome"`
	}
	decode(t, doJSON(t, "POST", "/api/v1/ai/agents/"+tn.AgentID+"/invoke", tn.RawKey, map[string]any{
		"agent_id":       tn.AgentID, // handler resolves from the body (path param ignored — see contract note)
		"interaction_id": "",
		"user_message":   "Hello, my invoice looks doubled this month.",
		"facts": map[string]any{
			"channel":            "whatsapp",
			"interaction_status": "active",
			"unread_count":       3,
		},
	}), 200, &result)
	if result.Outcome != "completed" || result.Suggestion == nil || result.Suggestion.Kind == "" {
		t.Fatalf("AI invocation not completed with a suggestion: %+v", result)
	}
	if result.OutputTokens <= 0 || result.InputTokens <= 0 {
		t.Fatalf("metering missing: input=%d output=%d", result.InputTokens, result.OutputTokens)
	}
	t.Logf("✓ AI suggest (metered): outcome=%s kind=%s tokens=%d+%d — %q",
		result.Outcome, result.Suggestion.Kind, result.InputTokens, result.OutputTokens, result.Suggestion.Title)

	// 4. tool call — the ONLY path from AI to side-effects: allowlist → platform
	//    cap → rate window → schema validation → execution → audit.
	var outcome struct {
		Tool     string         `json:"tool"`
		Executed bool           `json:"executed"`
		Result   map[string]any `json:"result"`
		Reason   string         `json:"reason"`
	}
	decode(t, doJSON(t, "POST", "/api/v1/ai/agents/"+tn.AgentID+"/tools", tn.RawKey, map[string]any{
		"tool": "get_customer",
		"args": map[string]any{"customer_id": cust.ID},
	}), 200, &outcome)
	if !outcome.Executed || outcome.Tool != "get_customer" {
		t.Fatalf("tool call not executed: %+v", outcome)
	}
	custRes, _ := outcome.Result["customer"].(map[string]any)
	if custRes == nil || custRes["id"] != cust.ID {
		t.Fatalf("tool result missing the customer: %+v", outcome.Result)
	}
	t.Log("✓ tool call executed=true through the gateway (tenant-scoped, allowlisted)")

	// 5. FAILURE INJECTION — policy refusal: send_whatsapp is NOT on the agent
	//    allowlist. The gateway must refuse (403, executed=false) AND audit it.
	refused := doJSON(t, "POST", "/api/v1/ai/agents/"+tn.AgentID+"/tools", tn.RawKey, map[string]any{
		"tool": "send_whatsapp",
		"args": map[string]any{"to": "+254733222111", "body": "should never send"},
	})
	var refusal struct {
		Tool     string `json:"tool"`
		Executed bool   `json:"executed"`
		Reason   string `json:"reason"`
	}
	decode(t, refused, 403, &refusal)
	if refusal.Executed || refusal.Reason == "" {
		t.Fatalf("expected an audited refusal, got %+v", refusal)
	}
	if refusal.Tool != "send_whatsapp" {
		t.Fatalf("refusal envelope lost the tool name: %+v", refusal)
	}
	t.Logf("✓ FAILURE INJECTION (policy refusal): send_whatsapp → 403 executed=false (%s)", refusal.Reason)

	// 6. audit-trail query: the worker's audit consumer appended both decisions
	//    (executed + refused) to the append-only audit_events read model.
	waitFor(t, "tool execution audits to land", 30*time.Second, func() (bool, string) {
		n := countQuery(t, `SELECT count(*) FROM audit_events
			WHERE tenant_id = $1 AND action = 'tool.execution.audited'
			  AND resource_id = 'get_customer'
			  AND after_json->>'outcome' = 'executed'`, tn.TenantID)
		return n >= 1, "executed_audits=" + itoa(n)
	})
	waitFor(t, "tool refusal audits to land", 30*time.Second, func() (bool, string) {
		n := countQuery(t, `SELECT count(*) FROM audit_events
			WHERE tenant_id = $1 AND action = 'tool.execution.audited'
			  AND resource_id = 'send_whatsapp'
			  AND after_json->>'outcome' = 'refused'`, tn.TenantID)
		return n >= 1, "refused_audits=" + itoa(n)
	})
	executed := countQuery(t, `SELECT count(*) FROM audit_events
		WHERE tenant_id = $1 AND action = 'tool.execution.audited'
		  AND resource_id = 'get_customer' AND after_json->>'outcome' = 'executed'`, tn.TenantID)
	refusals := countQuery(t, `SELECT count(*) FROM audit_events
		WHERE tenant_id = $1 AND action = 'tool.execution.audited'
		  AND resource_id = 'send_whatsapp' AND after_json->>'outcome' = 'refused'`, tn.TenantID)
	t.Logf("✓ audit trail: %d executed + %d refused tool decision(s) recorded", executed, refusals)

	// 7. metering landed in the facts pipeline (usage.recorded → usage_fact).
	waitFor(t, "AI usage facts to land", 30*time.Second, func() (bool, string) {
		n := countQuery(t, `SELECT count(*) FROM usage_fact
			WHERE tenant_id = $1 AND metric = 'ai_tokens' AND amount > 0`, tn.TenantID)
		return n >= 1, "usage_facts=" + itoa(n)
	})
	t.Log("✓ metering: usage.recorded fact (ai_tokens) recorded for the invocation")
	t.Log("J3 GREEN")
}
