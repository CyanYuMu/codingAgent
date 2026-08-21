package tool

import (
	"context"
	"encoding/json"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
)

// 工具结果 sink 的头尾窗口（超过则截断 + 落盘，L6）。
const (
	sinkHeadLimit = 4000
	sinkTailLimit = 4000
)

// Executor 执行工具调用：查表 → 审批 → 执行 → 塑形结果。
type Executor struct {
	registry *Registry
	mode     permission.Mode
}

func NewExecutor(r *Registry, mode permission.Mode) *Executor {
	return &Executor{registry: r, mode: mode}
}

// Execute 执行一次工具调用，返回给模型的结果文本。
func (e *Executor) Execute(ctx context.Context, call message.ToolCall) string {
	t, ok := e.registry.Get(call.Name)
	if !ok {
		return "tool not found: " + call.Name
	}
	decision := permission.Resolve(t.Tier(), e.mode)
	if decision == permission.DecisionPrompt {
		return "tool denied: requires approval (tier=" + string(t.Tier()) + ")"
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
