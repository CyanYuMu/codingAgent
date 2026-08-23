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

// Executor 执行工具调用：查表 → 审批 → 执行 → 塑形结果。
type Executor struct {
	registry *Registry
	mode     permission.Mode
	approver Approver // nil = 无 HITL（Prompt 降级拒绝）
}

func NewExecutor(r *Registry, mode permission.Mode, approver Approver) *Executor {
	return &Executor{registry: r, mode: mode, approver: approver}
}

// Execute 执行一次工具调用，返回给模型的结果文本。
func (e *Executor) Execute(ctx context.Context, call message.ToolCall) string {
	t, ok := e.registry.Get(call.Name)
	if !ok {
		return "tool not found: " + call.Name
	}
	decision := permission.Resolve(t.Tier(), e.mode)
	if decision == permission.DecisionPrompt {
		if e.approver == nil {
			return "tool denied: requires approval (tier=" + string(t.Tier()) + ")"
		}
		approved, err := e.approver.Approve(ctx, call)
		if err != nil {
			return "tool approval interrupted: " + err.Error()
		}
		if !approved {
			return "tool denied by user"
		}
		// 批准：继续执行
	}

	var args map[string]any
	if call.Args != "" {
		_ = json.Unmarshal([]byte(call.Args), &args) // 非法 JSON 按空参处理
	}

	sink := runtime.NewSink(sinkHeadLimit, sinkTailLimit)
	defer sink.Close()
	err := t.Execute(ctx, args, sink)
	result := sink.Result()
	if err != nil {
		return result + "\n[tool error: " + err.Error() + "]"
	}
	return result
}

// ExecuteAll 并行执行多个工具调用：Shared 用 goroutine 并行，Exclusive 串行，结果按调用序返回。
func (e *Executor) ExecuteAll(ctx context.Context, calls []message.ToolCall) []string {
	results := make([]string, len(calls))
	var wg sync.WaitGroup

	for i, call := range calls {
		t, ok := e.registry.Get(call.Name)
		if ok && t.Concurrency() == ConcurrencyExclusive {
			wg.Wait() // 等之前并行的完成，再串行执行
			results[i] = e.Execute(ctx, call)
			continue
		}
		wg.Add(1)
		go func(i int, call message.ToolCall) {
			defer wg.Done()
			results[i] = e.Execute(ctx, call)
		}(i, call)
	}
	wg.Wait()
	return results
}
