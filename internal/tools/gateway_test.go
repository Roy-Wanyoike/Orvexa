package tools

import (
	"context"
	"strings"
	"testing"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

type fakeExec struct{ calls int }

func (f *fakeExec) Execute(_ context.Context, c *Call) (map[string]any, error) {
	f.calls++
	return map[string]any{"ok": true, "tool": c.Tool}, nil
}

type fakePolicy struct{ allow map[string]int }

func (f *fakePolicy) Allowlist(_ context.Context, _, _ string) (map[string]int, error) {
	return f.allow, nil
}

func harness(allow map[string]int) (*Gateway, *fakeExec, *[]toolAudit) {
	exec := &fakeExec{}
	var audits []toolAudit
	g := NewGateway(exec, &fakePolicy{allow: allow},
		func(_ context.Context, c *Call, o *Outcome, err error) {
			audits = append(audits, toolAudit{tool: c.Tool, err: err})
		})
	return g, exec, &audits
}

type toolAudit struct {
	tool string
	err  error
}

func TestToolGatewayAllowlistEnforced(t *testing.T) {
	g, exec, audits := harness(map[string]int{ToolGetCustomer: 3})
	_, err := g.Invoke(context.Background(), &Call{
		Tool: ToolCreateCase, TenantID: "t", AgentID: "a",
		Args: map[string]any{"customer_id": "c1", "subject": "s"},
	})
	if err == nil {
		t.Fatal("tool outside allowlist must be refused")
	}
	if exec.calls != 0 {
		t.Fatal("refused tool must not execute")
	}
	if len(*audits) != 1 || (*audits)[0].err == nil {
		t.Fatal("refusal must be audited")
	}
}

func TestToolGatewaySchemaValidation(t *testing.T) {
	g, exec, _ := harness(map[string]int{ToolGetCustomer: 3})
	if _, err := g.Invoke(context.Background(), &Call{
		Tool: ToolGetCustomer, TenantID: "t", AgentID: "a", Args: map[string]any{},
	}); err == nil {
		t.Fatal("missing customer_id must be rejected")
	}
	if exec.calls != 0 {
		t.Fatal("invalid args must not reach the executor")
	}
	// valid call executes
	if _, err := g.Invoke(context.Background(), &Call{
		Tool: ToolGetCustomer, TenantID: "t", AgentID: "a", Args: map[string]any{"customer_id": "c-1"},
	}); err != nil {
		t.Fatalf("valid call must execute: %v", err)
	}
}

func TestToolGatewayRateLimitPerAgentTool(t *testing.T) {
	g, _, _ := harness(map[string]int{ToolSendWhatsApp: 1, ToolGetCustomer: 1})
	call := func() error {
		_, err := g.Invoke(context.Background(), &Call{
			Tool: ToolSendWhatsApp, TenantID: "t", AgentID: "a",
			Args: map[string]any{"to": "+254711", "body": "hi"},
		})
		return err
	}
	if err := call(); err != nil {
		t.Fatalf("first call must pass: %v", err)
	}
	err := call()
	if err == nil || !strings.Contains(err.Error(), "rate") {
		t.Fatalf("second call must be rate limited, got %v", err)
	}
	// a different agent has an independent window
	if _, err := g.Invoke(context.Background(), &Call{
		Tool: ToolSendWhatsApp, TenantID: "t", AgentID: "agent-b",
		Args: map[string]any{"to": "+254711", "body": "hi"},
	}); err != nil {
		t.Fatalf("other agent must have independent window: %v", err)
	}
}

func TestToolGatewayPlatformCapsClampPolicy(t *testing.T) {
	// policy allows 99 create_case, platform caps at 2
	g, exec, _ := harness(map[string]int{ToolCreateCase: 99})
	for i := 0; i < 2; i++ {
		if _, err := g.Invoke(context.Background(), &Call{
			Tool: ToolCreateCase, TenantID: "t", AgentID: "a",
			Args: map[string]any{"customer_id": "c1", "subject": "s"},
		}); err != nil {
			t.Fatalf("call %d must pass: %v", i+1, err)
		}
	}
	if _, err := g.Invoke(context.Background(), &Call{
		Tool: ToolCreateCase, TenantID: "t", AgentID: "a",
		Args: map[string]any{"customer_id": "c1", "subject": "s"},
	}); err == nil {
		t.Fatal("third create_case must hit platform cap")
	}
	if exec.calls != 2 {
		t.Fatalf("exactly 2 executions expected, got %d", exec.calls)
	}
}

func TestToolGatewayWhatsAppBodyCap(t *testing.T) {
	g, _, _ := harness(map[string]int{ToolSendWhatsApp: 5})
	long := make([]byte, 5000)
	for i := range long {
		long[i] = 'x'
	}
	_, err := g.Invoke(context.Background(), &Call{
		Tool: ToolSendWhatsApp, TenantID: "t", AgentID: "a",
		Args: map[string]any{"to": "+254711", "body": string(long)},
	})
	var appErr *apperrors.Error
	if err == nil || !errorsAs(err, &appErr) || appErr.Code != "tool.body_too_long" {
		t.Fatalf("oversized body must be rejected with stable code, got %v", err)
	}
}

func errorsAs(err error, target **apperrors.Error) bool {
	for err != nil {
		if e, ok := err.(*apperrors.Error); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
