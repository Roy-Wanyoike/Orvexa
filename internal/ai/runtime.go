// Package ai — agent runtime: resolves agent config, assembles grounded
// context from platform data, invokes the gateway, and executes suggested
// tools through the tool gateway when policy allows.
package ai

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Roy-Wanyoike/orvexa/internal/platform/outbox"
	"github.com/Roy-Wanyoike/orvexa/internal/tools"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// Agent is the runtime view of an AI agent configuration.
type Agent struct {
	ID           string         `json:"id"`
	TenantID     string         `json:"tenant_id"`
	Name         string         `json:"name"`
	ModelHint    string         `json:"model_hint"`
	Status       string         `json:"status"`
	SystemPrompt string         `json:"system_prompt"`
	Tools        map[string]int `json:"tools"` // tool → max calls per invocation
	Policies     map[string]any `json:"policies"`
}

// Runtime wires config resolution + gateway + tool gateway.
type Runtime struct {
	pool    *pgxpool.Pool
	gateway *Gateway
	tools   *tools.Gateway
	writer  *outbox.Writer
	source  string
}

func NewRuntime(pool *pgxpool.Pool, gateway *Gateway, toolGateway *tools.Gateway, writer *outbox.Writer, source string) *Runtime {
	return &Runtime{pool: pool, gateway: gateway, tools: toolGateway, writer: writer, source: source}
}

// ResolveAgent loads one tenant-scoped agent config.
func (r *Runtime) ResolveAgent(ctx context.Context, tenantID, agentID string) (*Agent, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT a.id, a.tenant_id, a.name, a.model_hint, a.status,
			coalesce(v.system_prompt,''),
			coalesce((SELECT jsonb_object_agg(t.tool_name, t.max_calls_per_invocation)
				FROM ai_agent_tools t WHERE t.agent_id = a.id AND t.allowed), '{}'::jsonb),
			coalesce((SELECT jsonb_object_agg(p.policy, p.config_json)
				FROM ai_agent_policies p WHERE p.agent_id = a.id), '{}'::jsonb)
		FROM ai_agents a
		LEFT JOIN LATERAL (
			SELECT system_prompt FROM ai_agent_versions
			WHERE agent_id = a.id ORDER BY version DESC LIMIT 1
		) v ON true
		WHERE a.id = $1 AND a.tenant_id = $2`, agentID, tenantID)

	ag := &Agent{Tools: map[string]int{}, Policies: map[string]any{}}
	var toolsJSON, policiesJSON map[string]any
	if err := row.Scan(&ag.ID, &ag.TenantID, &ag.Name, &ag.ModelHint, &ag.Status,
		&ag.SystemPrompt, &toolsJSON, &policiesJSON); err != nil {
		if err == pgx.ErrNoRows {
			return nil, apperrors.NotFound("ai.agent_not_found", "AI agent not found")
		}
		return nil, apperrors.Internal("db.read_failed", "read failed").WithCause(err)
	}
	for k, v := range toolsJSON {
		if n, ok := v.(float64); ok {
			ag.Tools[k] = int(n)
		}
	}
	ag.Policies = policiesJSON
	return ag, nil
}

// InvokeInput is the public invocation request.
type InvokeInput struct {
	AgentID       string         `json:"agent_id"`
	InteractionID string         `json:"interaction_id"`
	UserMessage   string         `json:"user_message"`
	Facts         map[string]any `json:"facts"`
}

// Invoke runs one governed AI turn: resolve config → assemble grounded
// context → gateway invocation → (policy-gated) tool suggestions surface as
// structured output, NOT auto-execution.
func (r *Runtime) Invoke(ctx context.Context, tenantID string, in InvokeInput) (*Result, error) {
	ag, err := r.ResolveAgent(ctx, tenantID, in.AgentID)
	if err != nil {
		return nil, err
	}
	if ag.Status != "active" {
		return nil, apperrors.Conflict("ai.agent_not_active", "AI agent is not active")
	}
	if in.Facts == nil {
		in.Facts = map[string]any{}
	}
	res, err := r.gateway.Invoke(ctx, &Invocation{
		TenantID: tenantID, AgentID: ag.ID, InteractionID: in.InteractionID,
		Context: Context{
			SystemPrompt: ag.SystemPrompt,
			Channel:      str(in.Facts["channel"]),
			Facts:        in.Facts,
			UserMessage:  in.UserMessage,
		},
	})
	if err != nil {
		return nil, err
	}
	// usage metering through the transactional outbox
	if env := EmitUsageEvent(r.source, res, &Invocation{
		TenantID: tenantID, AgentID: ag.ID, InteractionID: in.InteractionID,
	}); env != nil {
		if err := r.writer.PublishLater(ctx, env); err != nil {
			// metering loss is non-fatal but surfaced in logs by the writer
			return res, nil
		}
	}
	return res, nil
}

// InvokeTool executes a tool call on behalf of an agent through the gateway.
func (r *Runtime) InvokeTool(ctx context.Context, tenantID, agentID string, c *tools.Call) (*tools.Outcome, error) {
	c.TenantID = tenantID
	c.AgentID = agentID
	return r.tools.Invoke(ctx, c)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
