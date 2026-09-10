package ai

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Roy-Wanyoike/orvexa/pkg/events"
)

// RulesProvider is the deterministic, dependency-free provider. It produces
// grounded, auditable suggestions from the invocation context — no external
// calls, no nondeterminism. Production LLM providers (OpenAI-compatible
// endpoints, etc.) implement the same Provider port behind configuration.
type RulesProvider struct {
	source string
}

func NewRulesProvider(source string) *RulesProvider { return &RulesProvider{source: source} }

func (p *RulesProvider) Name() string { return "rules-v1" }

// Invoke derives a next-best-action suggestion from grounded facts.
// Deterministic contract (tested): channel-aware action choice, refusal on
// insufficient grounding, stable token accounting.
func (p *RulesProvider) Invoke(ctx context.Context, inv *Invocation) (*Result, error) {
	if inv == nil {
		return nil, fmt.Errorf("nil invocation")
	}
	// Refuse when there is nothing grounded to reason over — never invent.
	if inv.Context.Facts == nil || len(inv.Context.Facts) == 0 {
		return &Result{
			Text:        "Insufficient grounding to suggest an action.",
			Outcome:     "refused",
			InputTokens: countTokens(inv.Context.UserMessage),
		}, nil
	}

	channel, _ := inv.Context.Facts["channel"].(string)
	status, _ := inv.Context.Facts["interaction_status"].(string)
	unread := toInt(inv.Context.Facts["unread_count"])

	var s Suggestion
	switch {
	case status == "active" && channel == "voice":
		s = Suggestion{Kind: "next_best_action", Title: "Verify identity, then summarize the issue",
			Detail: "Call is live. Confirm the customer, restate their problem, and log a case note."}
	case unread > 0 && channel != "":
		s = Suggestion{Kind: "next_best_action", Title: "Reply to the customer's latest message",
			Detail: fmt.Sprintf("The conversation has %d unread inbound message(s) on %s.", unread, channel),
			Params: map[string]any{"suggested_channel": channel}}
	case status == "wrapup":
		s = Suggestion{Kind: "next_best_action", Title: "Complete after-call work",
			Detail: "Wrap the interaction: disposition, notes, and follow-up case if needed."}
	default:
		s = Suggestion{Kind: "summary", Title: "Monitor conversation",
			Detail: "No action is required right now; the conversation is progressing normally."}
	}

	out := fmt.Sprintf("%s — %s", s.Title, s.Detail)
	return &Result{
		Text:         out,
		Suggestion:   &s,
		InputTokens:  countTokens(inv.Context.SystemPrompt + inv.Context.UserMessage),
		OutputTokens: countTokens(out),
		Outcome:      "completed",
	}, nil
}

// EmitUsageEvent writes the usage.recorded envelope for an invocation result.
func EmitUsageEvent(source string, r *Result, inv *Invocation) *events.Envelope {
	env, err := events.New(events.TopicUsageRecorded, source, inv.InteractionID, inv.TenantID, "", map[string]any{
		"metric":        "ai_tokens",
		"input_tokens":  r.InputTokens,
		"output_tokens": r.OutputTokens,
		"outcome":       r.Outcome,
		"provider":      source,
		"agent_id":      inv.AgentID,
	})
	if err != nil {
		return nil
	}
	return env
}

func countTokens(s string) int {
	return len(strings.Fields(s))
}

func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}

var _ = time.Second
