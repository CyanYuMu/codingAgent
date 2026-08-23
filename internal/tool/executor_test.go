package tool

import (
	"context"
	"strings"
	"testing"
	"time"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
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

// blockingTool 启动时发信号、等 release 才完成，用于检测并行执行。
type blockingTool struct {
	name    string
	started chan struct{}
	release chan struct{}
}

func (b blockingTool) Name() string               { return b.name }
func (b blockingTool) Description() string        { return "d" }
func (b blockingTool) Parameters() map[string]any { return nil }
func (b blockingTool) Tier() permission.Tier      { return permission.TierRead }
func (b blockingTool) Concurrency() Concurrency   { return ConcurrencyShared }
func (b blockingTool) Execute(ctx context.Context, args map[string]any, sink *runtime.Sink) error {
	b.started <- struct{}{}
	<-b.release
	sink.Write([]byte("done"))
	return nil
}

func TestExecuteAllParallel(t *testing.T) {
	release := make(chan struct{})
	r := NewRegistry()
	r.Register(blockingTool{name: "t1", started: make(chan struct{}, 1), release: release})
	r.Register(blockingTool{name: "t2", started: make(chan struct{}, 1), release: release})
	e := NewExecutor(r, permission.ModeYolo, nil)

	done := make(chan []string, 1)
	go func() {
		done <- e.ExecuteAll(context.Background(), []message.ToolCall{{Name: "t1", Args: "{}"}, {Name: "t2", Args: "{}"}})
	}()

	waitStarted(t, r, "t1")
	// 若串行，t2 要等 t1 释放才会启动，这里会超时
	waitStarted(t, r, "t2")
	close(release)

	results := <-done
	if len(results) != 2 || results[0] != "done" || results[1] != "done" {
		t.Fatalf("results = %v", results)
	}
}

func waitStarted(t *testing.T, r *Registry, name string) {
	t.Helper()
	tool, _ := r.Get(name)
	ch := tool.(blockingTool).started
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("工具 %s 未启动（可能串行卡住）", name)
	}
}
