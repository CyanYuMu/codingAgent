package subagent

import (
	"context"

	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
	"einoclaw-build/internal/tool"
)

// yieldTool 是子 agent 的显式终止工具：调用它表示「任务完成，产出如下」。
// Manager 拦截这个调用（EventToolStart），从 args 里提取 data。
type yieldTool struct{}

func (yieldTool) Name() string        { return "yield" }
func (yieldTool) Description() string { return "完成任务时调用，返回结构化产出 data" }
func (yieldTool) Parameters() map[string]any {
	return map[string]any{"data": map[string]any{"type": "object"}}
}
func (yieldTool) Tier() permission.Tier          { return permission.TierRead }
func (yieldTool) Concurrency() tool.Concurrency  { return tool.ConcurrencyShared }

func (yieldTool) Execute(ctx context.Context, args map[string]any, sink *runtime.Sink) error {
	sink.Write([]byte("done"))
	return nil
}

// NewYieldTool 构造 yield 工具（子 agent 用）。
func NewYieldTool() tool.Tool { return yieldTool{} }
