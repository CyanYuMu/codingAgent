package tool

import (
	"context"
	"strings"
	"testing"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/permission"
)

func TestExecuteToolAllowAndPrompt(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeTool{name: "read", tier: permission.TierRead})
	r.Register(fakeTool{name: "bash", tier: permission.TierExec})

	e := NewExecutor(r, permission.ModeWrite)
	if out := e.Execute(context.Background(), message.ToolCall{Name: "read", Args: "{}"}); out != "ok" {
		t.Fatalf("read 应执行，got %q", out)
	}
	if out := e.Execute(context.Background(), message.ToolCall{Name: "bash", Args: "{}"}); !strings.Contains(out, "denied") {
		t.Fatalf("bash 应被拒，got %q", out)
	}
}

func TestExecuteToolNotFound(t *testing.T) {
	r := NewRegistry()
	e := NewExecutor(r, permission.ModeYolo)
	if out := e.Execute(context.Background(), message.ToolCall{Name: "nope", Args: "{}"}); !strings.Contains(out, "not found") {
		t.Fatalf("应返回 not found，got %q", out)
	}
}
