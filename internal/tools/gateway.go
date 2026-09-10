// Package tools implements the Tool Gateway: the ONLY path from AI to
// side-effects. Architecture doc §15 — the gateway enforces authorization,
// tenant validation, schema validation, rate limiting and audit before
// execution. AI holds no database, cloud, payment or CRM credentials; it
// holds tool allowlists.
package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
	"github.com/Roy-Wanyoike/orvexa/pkg/events"
)

// ToolName enumerates the first tool set.
const (
	ToolGetCustomer  = "get_customer"
	ToolCreateCase   = "create_case"
	ToolSendWhatsApp = "send_whatsapp"
)

// Call is one tool execution request emitted by an AI agent.
type Call struct {
	Tool          string
	TenantID      string
	AgentID       string
	InteractionID string
	Args          map[string]any
}

// Outcome reports what the gateway decided and what the tool returned.
type Outcome struct {
	Tool     string         `json:"tool"`
	Executed bool           `json:"executed"`
	Result   map[string]any `json:"result,omitempty"`
	Reason   string         `json:"reason,omitempty"`
	Latency  time.Duration  `json:"-"`
}

// Executor executes one validated tool within the tenant. Implementations are
// the platform's own services — the tool gateway is a façade over internal
// service methods, NEVER a raw credential surface.
type Executor interface {
	Execute(ctx context.Context, c *Call) (map[string]any, error)
}

// PolicySource resolves an agent's tool allowlist and per-call limits.
type PolicySource interface {
	Allowlist(ctx context.Context, tenantID, agentID string) (map[string]int, error)
}

// Gateway enforces the AI→side-effect boundary.
type Gateway struct {
	executor   Executor
	policies   PolicySource
	limiter    *sync.Map // agentID+tool → sliding window (bounded per entry count)
	audit      func(ctx context.Context, c *Call, o *Outcome, err error)
	maxPerCall map[string]int // platform-level caps per tool
	mu         sync.Mutex
	windows    map[string][]time.Time
}

// NewGateway builds the tool gateway with an audit sink.
func NewGateway(executor Executor, policies PolicySource,
	audit func(ctx context.Context, c *Call, o *Outcome, err error)) *Gateway {
	return &Gateway{
		executor: executor,
		policies: policies,
		audit:    audit,
		maxPerCall: map[string]int{
			ToolGetCustomer:  5,
			ToolCreateCase:   2,
			ToolSendWhatsApp: 1,
		},
		windows: map[string][]time.Time{},
	}
}

// CallResult is the exported result type for handlers.
type CallResult = Outcome

// Invoke runs the full gate chain: allowlist → platform cap → rate window →
// schema validation → execution → audit. Every decision is audited,
// including refusals.
func (g *Gateway) Invoke(ctx context.Context, c *Call) (*Outcome, error) {
	out := &Outcome{Tool: c.Tool}

	o, err := g.invoke(ctx, c, out)
	if g.audit != nil {
		g.audit(ctx, c, o, err)
	}
	return o, err
}

func (g *Gateway) invoke(ctx context.Context, c *Call, out *Outcome) (*Outcome, error) {
	if c.TenantID == "" || c.AgentID == "" {
		return out, apperrors.Invalid("tool.tenant_agent_required", "tenant and agent context are required")
	}
	// 1) authorization: allowlist per agent
	allow, err := g.policies.Allowlist(ctx, c.TenantID, c.AgentID)
	if err != nil {
		return out, err
	}
	limit, ok := allow[c.Tool]
	if !ok || limit <= 0 {
		out.Reason = "tool not in agent allowlist"
		return out, apperrors.Forbidden("tool.not_allowed", "agent is not allowed to call "+c.Tool)
	}

	// 2) platform-level per-invocation cap (min of policy and platform limit)
	if plat, ok := g.maxPerCall[c.Tool]; ok && limit > plat {
		limit = plat
	}
	if err := g.admit(c.AgentID, c.Tool, limit); err != nil {
		out.Reason = "rate limit"
		return out, err
	}

	// 3) schema validation per tool
	if err := validateArgs(c); err != nil {
		out.Reason = "schema validation failed"
		return out, err
	}

	// 4) execution through the platform service executor
	start := time.Now()
	res, err := g.executor.Execute(ctx, c)
	out.Latency = time.Since(start)
	if err != nil {
		out.Reason = "execution failed"
		return out, err
	}
	out.Executed = true
	out.Result = res
	return out, nil
}

// admit enforces a per-agent-per-tool call rate within the invocation window.
func (g *Gateway) admit(agentID, tool string, limit int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := agentID + ":" + tool
	now := time.Now()
	window := g.windows[key][:0]
	for _, t := range g.windows[key] {
		if now.Sub(t) < time.Minute {
			window = append(window, t)
		}
	}
	if len(window) >= limit {
		g.windows[key] = window
		return apperrors.RateLimited("tool.rate_limited", "tool call limit reached for "+tool)
	}
	g.windows[key] = append(window, now)
	if len(g.windows) > 10_000 { // bounded memory
		g.windows = map[string][]time.Time{key: g.windows[key]}
	}
	return nil
}

// validateArgs enforces the per-tool argument schema (deny-by-default).
func validateArgs(c *Call) error {
	switch c.Tool {
	case ToolGetCustomer:
		return requireString(c.Args, "customer_id")
	case ToolCreateCase:
		if err := requireString(c.Args, "customer_id"); err != nil {
			return err
		}
		return requireString(c.Args, "subject")
	case ToolSendWhatsApp:
		if err := requireString(c.Args, "to"); err != nil {
			return err
		}
		body, _ := c.Args["body"].(string)
		if strings.TrimSpace(body) == "" {
			return apperrors.Invalid("tool.body_required", "body is required")
		}
		if len(body) > 4096 {
			return apperrors.Invalid("tool.body_too_long", "body must be at most 4096 chars")
		}
		return nil
	default:
		return apperrors.Invalid("tool.unknown", "unknown tool "+c.Tool)
	}
}

func requireString(args map[string]any, field string) error {
	v, ok := args[field].(string)
	if !ok || strings.TrimSpace(v) == "" {
		return apperrors.Invalid("tool.field_required", fmt.Sprintf("%s is required", field))
	}
	if len(v) > 512 {
		return apperrors.Invalid("tool.field_too_long", fmt.Sprintf("%s must be at most 512 chars", field))
	}
	return nil
}

// AuditEnvelope builds the tool.execution.audited event for a gateway decision.
func AuditEnvelope(source string, c *Call, o *Outcome, err error) *events.Envelope {
	outcome := "executed"
	if err != nil {
		outcome = "refused"
	}
	env, e := events.New(events.TopicToolExecutionAudited, source, c.Tool, c.TenantID, "", map[string]any{
		"tool": c.Tool, "agent_id": c.AgentID, "interaction_id": c.InteractionID,
		"outcome": outcome, "executed": o != nil && o.Executed,
		"reason": reasonOf(o), "error": errString(err),
	})
	if e != nil {
		return nil
	}
	return env
}

func reasonOf(o *Outcome) string {
	if o == nil {
		return ""
	}
	return o.Reason
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
