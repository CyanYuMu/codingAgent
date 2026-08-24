package tool

import (
	"context"
	"errors"
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

type reasonApprover struct{}

func (reasonApprover) Approve(context.Context, message.ToolCall) (bool, error) { return false, nil }
func (reasonApprover) DenyReason() string                                      { return "headless subagent cannot prompt" }

func TestExecuteToolAllowAndPrompt(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeTool{name: "read", tier: permission.TierRead})
	r.Register(fakeTool{name: "bash", tier: permission.TierExec})

	e := NewExecutor(r, permission.ModeWrite, nil)
	if out := e.Execute(context.Background(), message.ToolCall{Name: "read", Args: "{}"}); out.Content != "ok" || out.IsError {
		t.Fatalf("read 应执行，got %+v", out)
	}
	if out := e.Execute(context.Background(), message.ToolCall{Name: "bash", Args: "{}"}); !strings.Contains(out.Content, "denied") || !out.IsError {
		t.Fatalf("bash 应被拒，got %+v", out)
	}
}

func TestExecuteToolNotFound(t *testing.T) {
	r := NewRegistry()
	e := NewExecutor(r, permission.ModeYolo, nil)
	if out := e.Execute(context.Background(), message.ToolCall{Name: "nope", Args: "{}"}); !strings.Contains(out.Content, "not found") || !out.IsError {
		t.Fatalf("应返回 not found，got %+v", out)
	}
}

func TestExecuteToolPromptCallsApprover(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeTool{name: "bash", tier: permission.TierExec})
	a := &fakeApprover{decision: true}
	e := NewExecutor(r, permission.ModeWrite, a)
	if out := e.Execute(context.Background(), message.ToolCall{Name: "bash", Args: "{}"}); !a.called || out.Content != "ok" {
		t.Fatalf("批准后应执行 ok，got called=%v out=%+v", a.called, out)
	}
}

func TestExecuteToolPromptDenied(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeTool{name: "bash", tier: permission.TierExec})
	a := &fakeApprover{decision: false}
	e := NewExecutor(r, permission.ModeWrite, a)
	if out := e.Execute(context.Background(), message.ToolCall{Name: "bash", Args: "{}"}); !strings.Contains(out.Content, "denied") {
		t.Fatalf("拒绝后应 denied，got %+v", out)
	}
}

func TestExecuteToolPromptNoApprover(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeTool{name: "bash", tier: permission.TierExec})
	e := NewExecutor(r, permission.ModeWrite, nil)
	if out := e.Execute(context.Background(), message.ToolCall{Name: "bash", Args: "{}"}); !strings.Contains(out.Content, "denied") {
		t.Fatalf("无 approver 应 denied，got %+v", out)
	}
}

func TestDeniedApprovalUsesReason(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeTool{name: "bash", tier: permission.TierExec})
	e := NewExecutor(r, permission.ModeAlwaysAsk, reasonApprover{})
	out := e.Execute(context.Background(), message.ToolCall{Name: "bash", Args: "{}"})
	if !out.IsError || !strings.Contains(out.Content, "headless subagent cannot prompt") {
		t.Fatalf("result = %+v", out)
	}
}

func TestExecuteReadsArtifactBack(t *testing.T) {
	store := runtime.NewArtifactStore(t.TempDir())
	reg := NewRegistry()
	for _, tl := range Builtins(runtime.NewBash(t.TempDir()), store) {
		reg.Register(tl)
	}
	ex := NewExecutor(reg, permission.ModeYolo, nil)
	ex.SetArtifactStore(store)
	// 3000 行 > 8000 字节窗口
	r := ex.Execute(context.Background(), message.ToolCall{ID: "1", Name: "bash", Args: `{"command":"for i in $(seq 1 3000); do echo line$i; done"}`})
	if r.IsError || !strings.Contains(r.Content, "artifact://0") {
		t.Fatalf("result = %+v", r)
	}
	rr := ex.Execute(context.Background(), message.ToolCall{ID: "2", Name: "read_file", Args: `{"file_path":"artifact://0","offset":2,"limit":2}`})
	if rr.IsError || !strings.HasPrefix(rr.Content, "line2\nline3") || !strings.Contains(rr.Content, "offset=4") {
		t.Fatalf("read back = %+v", rr)
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

	done := make(chan []Result, 1)
	go func() {
		done <- e.ExecuteAll(context.Background(), []message.ToolCall{{Name: "t1", Args: "{}"}, {Name: "t2", Args: "{}"}})
	}()

	waitStarted(t, r, "t1")
	// 若串行，t2 要等 t1 释放才会启动，这里会超时
	waitStarted(t, r, "t2")
	close(release)

	results := <-done
	if len(results) != 2 || results[0].Content != "done" || results[1].Content != "done" {
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

// stopTool 按调用参数决定是否终止（模拟 yield 的三态：增量提交不终止、出错不终止）。
type stopTool struct{}

func (stopTool) Name() string               { return "stop" }
func (stopTool) Description() string        { return "" }
func (stopTool) Parameters() map[string]any { return map[string]any{} }
func (stopTool) Tier() permission.Tier      { return permission.TierRead }
func (stopTool) Concurrency() Concurrency   { return ConcurrencyShared }
func (stopTool) IsTerminal(args map[string]any, err error) bool {
	if err != nil {
		return false
	}
	stop, _ := args["stop"].(bool)
	return stop
}
func (stopTool) Execute(_ context.Context, args map[string]any, sink *runtime.Sink) error {
	sink.Write([]byte("ok"))
	if bad, _ := args["bad"].(bool); bad {
		return errors.New("retry me")
	}
	return nil
}

func TestResultTerminalIsPerCall(t *testing.T) {
	reg := NewRegistry()
	reg.Register(stopTool{})
	e := NewExecutor(reg, permission.ModeYolo, nil)
	for _, tc := range []struct {
		args     string
		terminal bool
		isErr    bool
	}{
		{`{"stop":true}`, true, false},
		{`{"stop":false}`, false, false},
		{`{}`, false, false},
		{`{"stop":true,"bad":true}`, false, true}, // 工具内退回重试：不终止
	} {
		res := e.Execute(context.Background(), message.ToolCall{ID: "c", Name: "stop", Args: tc.args})
		if res.Terminal != tc.terminal || res.IsError != tc.isErr {
			t.Fatalf("args %s → terminal=%v isErr=%v, want %v/%v", tc.args, res.Terminal, res.IsError, tc.terminal, tc.isErr)
		}
	}
}
