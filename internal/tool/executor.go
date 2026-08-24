package tool

import (
	"context"
	"encoding/json"
	"sync"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
)

// 工具结果 sink 的头尾窗口（超过则截断 + 落盘，L6）。
const (
	sinkHeadLimit = 4000
	sinkTailLimit = 4000
)

// Approver 是审批的中断点：blocking 等待用户决定。
// true = 允许执行，false = 拒绝；ctx 取消时返回 error（中断）。
type Approver interface {
	Approve(ctx context.Context, call message.ToolCall) (bool, error)
}

// DenyReasoner 可选接口：审批器拒绝时给模型的说明（如 headless 无法弹窗）。
type DenyReasoner interface{ DenyReason() string }

// Result 是一次工具执行给模型的结果。
type Result struct {
	Content  string
	IsError  bool
	Terminal bool // 本次调用要求终止 run（yield 的终止提交）
}

// Executor 执行工具调用：查表 → 审批 → 执行 → 塑形结果。
type Executor struct {
	registry *Registry
	mode     permission.Mode
	approver Approver      // nil = 无 HITL（Prompt 降级拒绝）
	sem      chan struct{} // 并发上限（Shared 工具并行数）
	store    *runtime.ArtifactStore
}

func NewExecutor(r *Registry, mode permission.Mode, approver Approver) *Executor {
	return &Executor{registry: r, mode: mode, approver: approver, sem: make(chan struct{}, 8)}
}

// SetArtifactStore 设置截断落盘用的产物存储（nil = 只截断不落盘）。
func (e *Executor) SetArtifactStore(s *runtime.ArtifactStore) { e.store = s }

// Mode 返回审批模式。
func (e *Executor) Mode() permission.Mode { return e.mode }

// Execute 执行一次工具调用，返回给模型的结果。
func (e *Executor) Execute(ctx context.Context, call message.ToolCall) Result {
	t, ok := e.registry.Get(call.Name)
	if !ok {
		return Result{Content: "tool not found: " + call.Name, IsError: true}
	}
	if permission.Resolve(t.Tier(), e.mode) == permission.DecisionPrompt {
		if e.approver == nil {
			return Result{Content: "tool denied: requires approval (tier=" + string(t.Tier()) + ", no approver)", IsError: true}
		}
		approved, err := e.approver.Approve(ctx, call)
		if err != nil {
			return Result{Content: "tool approval interrupted: " + err.Error(), IsError: true}
		}
		if !approved {
			reason := "tool denied by user"
			if r, ok := e.approver.(DenyReasoner); ok && r.DenyReason() != "" {
				reason = r.DenyReason()
			}
			return Result{Content: reason, IsError: true}
		}
	}

	var args map[string]any
	if call.Args != "" {
		_ = json.Unmarshal([]byte(call.Args), &args) // 非法 JSON 按空参处理
	}

	sink := runtime.NewSink(sinkHeadLimit, sinkTailLimit)
	if e.store != nil {
		sink.SetArtifactStore(e.store, call.Name)
	}
	defer sink.Close()
	err := t.Execute(ctx, args, sink)
	res := sink.Result()
	terminal := false
	if term, ok := t.(Terminal); ok {
		terminal = term.IsTerminal(args, err)
	}
	if err != nil {
		return Result{Content: res + "\n[tool error: " + err.Error() + "]", IsError: true, Terminal: terminal}
	}
	return Result{Content: res, Terminal: terminal}
}

// ExecuteAll 并行执行多个工具调用：Shared 用 goroutine 并行（Semaphore 限并发），Exclusive 串行，结果按调用序返回。
func (e *Executor) ExecuteAll(ctx context.Context, calls []message.ToolCall) []Result {
	results := make([]Result, len(calls))
	var wg sync.WaitGroup

	for i, call := range calls {
		t, ok := e.registry.Get(call.Name)
		if ok && t.Concurrency() == ConcurrencyExclusive {
			wg.Wait() // 等之前并行的完成，再串行执行
			results[i] = e.Execute(ctx, call)
			continue
		}
		if err := e.acquire(ctx); err != nil {
			results[i] = Result{Content: "tool error: " + err.Error(), IsError: true}
			continue
		}
		wg.Add(1)
		go func(i int, call message.ToolCall) {
			defer wg.Done()
			defer e.release()
			results[i] = e.Execute(ctx, call)
		}(i, call)
	}
	wg.Wait()
	return results
}

func (e *Executor) acquire(ctx context.Context) error {
	select {
	case e.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *Executor) release() { <-e.sem }
