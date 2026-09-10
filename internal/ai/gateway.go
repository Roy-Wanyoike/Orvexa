// Package ai owns the intelligence plane: the AI gateway (provider routing,
// metering, timeout/retry) and the agent runtime (context assembly over
// platform data). AI output is advisory; side-effects flow ONLY through the
// tool gateway (internal/tools).
package ai

import (
	"context"
	"errors"
	"time"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// Invocation is a single AI execution request.
type Invocation struct {
	TenantID      string
	AgentID       string
	InteractionID string
	Context       Context // assembled by the runtime from platform data
	MaxTokens     int     // hard output cap
	Timeout       time.Duration
}

// Context is the grounded input the provider may use. The runtime assembles
// it from the tenant's own data — providers never see other tenants.
type Context struct {
	SystemPrompt   string         `json:"system_prompt"`
	CustomerID     string         `json:"customer_id,omitempty"`
	ConversationID string         `json:"conversation_id,omitempty"`
	Channel        string         `json:"channel,omitempty"`
	Facts          map[string]any `json:"facts"` // grounded facts only
	UserMessage    string         `json:"user_message,omitempty"`
}

// Result is the provider output plus metering data.
type Result struct {
	Text         string        `json:"text"`
	Suggestion   *Suggestion   `json:"suggestion,omitempty"`
	InputTokens  int           `json:"input_tokens"`
	OutputTokens int           `json:"output_tokens"`
	Latency      time.Duration `json:"-"`
	Outcome      string        `json:"outcome"` // completed|refused|timeout|failed
}

// Suggestion is a structured recommendation the UI can act on (with or
// without human confirmation per agent policy).
type Suggestion struct {
	Kind   string         `json:"kind"` // e.g. next_best_action|summary|reply_draft
	Title  string         `json:"title"`
	Detail string         `json:"detail,omitempty"`
	Params map[string]any `json:"params,omitempty"`
}

// Provider is the LLM port. Implementations MUST be tenant-safe (context in,
// result out; no credential exposure, no side-effects).
type Provider interface {
	Invoke(ctx context.Context, inv *Invocation) (*Result, error)
	Name() string
}

// Gateway routes invocations to the configured provider with metering,
// timeout and retry discipline. It is the ONLY path to a provider.
type Gateway struct {
	provider    Provider
	maxTokens   int
	timeout     time.Duration
	maxAttempts int
	onUsage     func(ctx context.Context, r *Result, inv *Invocation)
}

// GatewayOption configures the gateway.
type GatewayOption func(*Gateway)

// WithUsageSink records usage events (usage.recorded) per invocation.
func WithUsageSink(fn func(ctx context.Context, r *Result, inv *Invocation)) GatewayOption {
	return func(g *Gateway) { g.onUsage = fn }
}

// NewGateway builds the gateway. Timeout/retry/max-token caps are enforced
// here so no provider adapter can bypass cost controls.
func NewGateway(provider Provider, maxTokens int, timeout time.Duration, opts ...GatewayOption) *Gateway {
	if maxTokens <= 0 {
		maxTokens = 1024
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	g := &Gateway{provider: provider, maxTokens: maxTokens, timeout: timeout, maxAttempts: 2}
	for _, o := range opts {
		o(g)
	}
	return g
}

// Invoke runs one governed AI invocation. Errors are app errors; timeout maps
// to a distinct outcome for metering.
func (g *Gateway) Invoke(ctx context.Context, inv *Invocation) (*Result, error) {
	if inv == nil || inv.TenantID == "" {
		return nil, apperrors.Invalid("ai.invalid_invocation", "tenant is required")
	}
	if inv.MaxTokens <= 0 || inv.MaxTokens > g.maxTokens {
		inv.MaxTokens = g.maxTokens
	}
	if inv.Timeout <= 0 || inv.Timeout > g.timeout {
		inv.Timeout = g.timeout
	}

	var lastErr error
	for attempt := 1; attempt <= g.maxAttempts; attempt++ {
		res, err := g.invokeOnce(ctx, inv)
		if err == nil {
			if g.onUsage != nil {
				g.onUsage(ctx, res, inv)
			}
			return res, nil
		}
		lastErr = err
		var appErr *apperrors.Error
		if errors.As(err, &appErr) && (appErr.Kind == apperrors.KindInvalid || appErr.Kind == apperrors.KindConflict) {
			break // invalid input and timeouts are non-retryable
		}
	}
	return nil, lastErr
}

func (g *Gateway) invokeOnce(ctx context.Context, inv *Invocation) (*Result, error) {
	start := time.Now()
	cctx, cancel := context.WithTimeout(ctx, inv.Timeout)
	defer cancel()
	res, err := g.provider.Invoke(cctx, inv)
	if err != nil {
		var appErr *apperrors.Error
		if errors.As(err, &appErr) && appErr.Code == "ai.timeout" {
			res = &Result{Outcome: "timeout", Latency: time.Since(start)}
			if g.onUsage != nil {
				g.onUsage(ctx, res, inv)
			}
			return nil, apperrors.Conflict("ai.timeout", "AI invocation timed out")
		}
		return nil, err
	}
	if res.OutputTokens > inv.MaxTokens {
		res.OutputTokens = inv.MaxTokens // metering cannot exceed the cap
	}
	res.Latency = time.Since(start)
	return res, nil
}
