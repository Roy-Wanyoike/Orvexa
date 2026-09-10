package ai

import (
	"context"
	"strings"
	"testing"
	"time"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

type slowProvider struct{ delay time.Duration }

func (s *slowProvider) Invoke(ctx context.Context, inv *Invocation) (*Result, error) {
	select {
	case <-time.After(s.delay):
		return &Result{Text: "done", Outcome: "completed", OutputTokens: 10}, nil
	case <-ctx.Done():
		return nil, apperrors.Conflict("ai.timeout", "AI invocation timed out")
	}
}
func (s *slowProvider) Name() string { return "slow" }

func TestGatewayTimeoutProducesMeteredTimeoutOutcome(t *testing.T) {
	var sink []*Result
	g := NewGateway(&slowProvider{delay: 200 * time.Millisecond}, 100, 50*time.Millisecond,
		WithUsageSink(func(_ context.Context, r *Result, _ *Invocation) { sink = append(sink, r) }))
	_, err := g.Invoke(context.Background(), &Invocation{TenantID: "t"})
	if err == nil {
		t.Fatal("timeout must surface as error")
	}
	var appErr *apperrors.Error
	for e := err; e != nil; e = e.(interface{ Unwrap() error }).Unwrap() {
		if ae, ok := e.(*apperrors.Error); ok && ae.Code == "ai.timeout" {
			appErr = ae
		}
	}
	if appErr == nil {
		t.Fatalf("timeout code expected, got %v", err)
	}
	if len(sink) == 0 || sink[0].Outcome != "timeout" {
		t.Fatal("timeout outcome must be metered via usage sink")
	}
}

type countingProvider struct{ calls int }

func (c *countingProvider) Invoke(_ context.Context, _ *Invocation) (*Result, error) {
	c.calls++
	return nil, apperrors.Internal("ai.provider_down", "provider down")
}
func (c *countingProvider) Name() string { return "counting" }

func TestGatewayRetriesThenFails(t *testing.T) {
	p := &countingProvider{}
	g := NewGateway(p, 100, time.Second)
	if _, err := g.Invoke(context.Background(), &Invocation{TenantID: "t"}); err == nil {
		t.Fatal("persistent failure must surface")
	}
	if p.calls != 2 {
		t.Fatalf("default maxAttempts is 2, got %d", p.calls)
	}
}

type okProvider struct{}

func (okProvider) Invoke(_ context.Context, inv *Invocation) (*Result, error) {
	return &Result{Text: "ok", Outcome: "completed", OutputTokens: 999999}, nil
}
func (okProvider) Name() string { return "ok" }

func TestGatewayEnforcesTokenCap(t *testing.T) {
	var sink []*Result
	g := NewGateway(okProvider{}, 128, time.Second,
		WithUsageSink(func(_ context.Context, r *Result, _ *Invocation) { sink = append(sink, r) }))
	res, err := g.Invoke(context.Background(), &Invocation{TenantID: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if res.OutputTokens != 128 {
		t.Fatalf("metering must clamp to cap, got %d", res.OutputTokens)
	}
	if len(sink) != 1 || sink[0].OutputTokens != 128 {
		t.Fatal("usage sink must record clamped tokens")
	}
}

func TestRulesProviderRefusesWithoutFacts(t *testing.T) {
	p := NewRulesProvider("test")
	res, err := p.Invoke(context.Background(), &Invocation{TenantID: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != "refused" || !strings.Contains(res.Text, "Insufficient grounding") {
		t.Fatalf("no facts must refuse, got %+v", res)
	}
}

func TestRulesProviderDeterministicSuggestions(t *testing.T) {
	p := NewRulesProvider("test")
	inv := &Invocation{TenantID: "t", Context: Context{
		Facts: map[string]any{"channel": "whatsapp", "unread_count": 2, "interaction_status": "active"},
	}}
	a, _ := p.Invoke(context.Background(), inv)
	b, _ := p.Invoke(context.Background(), inv)
	if a.Text != b.Text || a.Suggestion == nil {
		t.Fatal("rules provider must be deterministic and structured")
	}
	if a.Suggestion.Title == "" {
		t.Fatal("suggestion must carry a title")
	}
}
