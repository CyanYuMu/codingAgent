package tool

import (
	"context"
	"strings"
	"testing"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/permission"
)

type fakeApprover struct {
	decision bool
	called   bool
}

func (f *fakeApprover) Approve(ctx context.Context, call message.ToolCall) (bool, error) {
	f.called = true
	return f.decision, nil
}

func TestExecuteToolAllowAndPrompt(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeTool{name: "read", tier: permission.TierRead})
	r.Register(fakeTool{name: "bash", tier: permission.TierExec})

	e := NewExecutor(r, permission.ModeWrite, nil)
	if out := e.Execute(context.Background(), message.ToolCall{Name: "read", Args: "{}"}); out != "ok" {
		t.Fatalf("read 应执行，got %q", out)
	}
	if out := e.Execute(context.Background(), message.ToolCall{Name: "bash", Args: "{}"}); !strings.Contains(out, "denied") {
		t.Fatalf("bash 应被拒，got %q", out)
	}
}

func TestExecuteToolNotFound(t *testing.T) {
	r := NewRegistry()
	e := NewExecutor(r, permission.ModeYolo, nil)
	if out := e.Execute(context.Background(), message.ToolCall{Name: "nope", Args: "{}"}); !strings.Contains(out, "not found") {
		t.Fatalf("应返回 not found，got %q", out)
	}
}

func TestExecuteToolPromptCallsApprover(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeTool{name: "bash", tier: permission.TierExec})
	a := &fakeApprover{decision: true}
	e := NewExecutor(r, permission.ModeWrite, a)
	if out := e.Execute(context.Background(), message.ToolCall{Name: "bash", Args: "{}"}); !a.called || out != "ok" {
		t.Fatalf("批准后应执行 ok，got called=%v out=%q", a.called, out)
	}
}

func TestExecuteToolPromptDenied(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeTool{name: "bash", tier: permission.TierExec})
	a := &fakeApprover{decision: false}
	e := NewExecutor(r, permission.ModeWrite, a)
	if out := e.Execute(context.Background(), message.ToolCall{Name: "bash", Args: "{}"}); !strings.Contains(out, "denied") {
		t.Fatalf("拒绝后应 denied，got %q", out)
	}
}

func TestExecuteToolPromptNoApprover(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeTool{name: "bash", tier: permission.TierExec})
	e := NewExecutor(r, permission.ModeWrite, nil)
	if out := e.Execute(context.Background(), message.ToolCall{Name: "bash", Args: "{}"}); !strings.Contains(out, "denied") {
		t.Fatalf("无 approver 应 denied，got %q", out)
	}
}
