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

// decisionTool 带 Decision 自检的工具：按参数返回 ToolDecision。
type decisionTool struct {
	fakeTool
	decision permission.ToolDecision
}

func (d decisionTool) Decision(args map[string]any) permission.ToolDecision { return d.decision }

func TestExecutorAppliesRules(t *testing.T) {
	// allow 规则：write 模式下 exec 工具免审批
	r := NewRegistry()
	r.Register(fakeTool{name: "bash", tier: permission.TierExec})
	e := NewExecutor(r, permission.ModeWrite, nil)
	e.SetRules(permission.Rules{Allow: []permission.Rule{testRule(t, "bash(go test*)")}})
	if out := e.Execute(context.Background(), message.ToolCall{Name: "bash", Args: `{"command":"go test ./..."}`}); out.IsError || out.Content != "ok" {
		t.Fatalf("allow 规则应放行，got %+v", out)
	}
	// 未命中规则 → 仍询问
	if out := e.Execute(context.Background(), message.ToolCall{Name: "bash", Args: `{"command":"rm x"}`}); !out.IsError || !strings.Contains(out.Content, "denied") {
		t.Fatalf("未命中规则应 denied，got %+v", out)
	}

	// deny 规则：yolo 下也拒，approver 不被调用
	r2 := NewRegistry()
	r2.Register(fakeTool{name: "read", tier: permission.TierRead})
	a := &fakeApprover{decision: true}
	e2 := NewExecutor(r2, permission.ModeYolo, a)
	e2.SetRules(permission.Rules{Deny: []permission.Rule{testRule(t, "read(./.env*)")}})
	out := e2.Execute(context.Background(), message.ToolCall{Name: "read", Args: `{"file_path":"./.env"}`})
	if !out.IsError || !strings.Contains(out.Content, "denied by rule: read(./.env*)") || a.called {
		t.Fatalf("deny 规则应拒且不弹审批，got %+v called=%v", out, a.called)
	}
}

func TestExecutorUsesDecisioner(t *testing.T) {
	r := NewRegistry()
	r.Register(decisionTool{fakeTool: fakeTool{name: "bash", tier: permission.TierExec},
		decision: permission.ToolDecision{Tier: permission.TierExec, Override: true, Reason: "rm 递归/强制删除"}})
	e := NewExecutor(r, permission.ModeWrite, nil)
	out := e.Execute(context.Background(), message.ToolCall{Name: "bash", Args: `{"command":"rm -rf /"}`})
	if !out.IsError || !strings.Contains(out.Content, "rm 递归/强制删除") {
		t.Fatalf("Override 应强制询问并带原因，got %+v", out)
	}
	// 工具显式 deny 永远生效
	r2 := NewRegistry()
	r2.Register(decisionTool{fakeTool: fakeTool{name: "x", tier: permission.TierRead},
		decision: permission.ToolDecision{Tier: permission.TierRead, Policy: permission.PolicyDeny, Reason: "只读源"}})
	e2 := NewExecutor(r2, permission.ModeYolo, nil)
	if out := e2.Execute(context.Background(), message.ToolCall{Name: "x", Args: "{}"}); !out.IsError || !strings.Contains(out.Content, "只读源") {
		t.Fatalf("工具 deny 应永远生效，got %+v", out)
	}
}

func TestExecutorOverrideForcesPrompt(t *testing.T) {
	r := NewRegistry()
	r.Register(decisionTool{fakeTool: fakeTool{name: "bash", tier: permission.TierExec},
		decision: permission.ToolDecision{Tier: permission.TierExec, Override: true, Reason: "危险"}})
	a := &fakeApprover{decision: true}
	e := NewExecutor(r, permission.ModeWrite, a)
	if out := e.Execute(context.Background(), message.ToolCall{Name: "bash", Args: "{}"}); !a.called || out.Content != "ok" {
		t.Fatalf("Override + 批准应执行，got called=%v out=%+v", a.called, out)
	}
	// 拒绝路径
	a2 := &fakeApprover{decision: false}
	e2 := NewExecutor(r, permission.ModeWrite, a2)
	if out := e2.Execute(context.Background(), message.ToolCall{Name: "bash", Args: "{}"}); !out.IsError || !strings.Contains(out.Content, "危险") {
		t.Fatalf("Override + 拒绝应 denied 带原因，got %+v", out)
	}
}

func TestExecutorAllowSessionSkipsApproval(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeTool{name: "bash", tier: permission.TierExec})
	a := &fakeApprover{decision: false} // 拒绝型 approver：若被问必然 denied
	e := NewExecutor(r, permission.ModeWrite, a)
	e.AllowSession("bash")
	if out := e.Execute(context.Background(), message.ToolCall{Name: "bash", Args: "{}"}); out.IsError || out.Content != "ok" {
		t.Fatalf("本会话允许后应免审批执行，got %+v", out)
	}
	// 危险 Override 不被本会话允许跳过
	r2 := NewRegistry()
	r2.Register(decisionTool{fakeTool: fakeTool{name: "bash", tier: permission.TierExec},
		decision: permission.ToolDecision{Tier: permission.TierExec, Override: true, Reason: "危险"}})
	e2 := NewExecutor(r2, permission.ModeWrite, a)
	e2.AllowSession("bash")
	if out := e2.Execute(context.Background(), message.ToolCall{Name: "bash", Args: "{}"}); !out.IsError {
		t.Fatalf("Override 不受本会话允许豁免，got %+v", out)
	}
}

func testRule(t *testing.T, raw string) permission.Rule {
	t.Helper()
	r, err := permission.ParseRule(raw)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
