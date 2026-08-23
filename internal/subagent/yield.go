package subagent

import (
	"context"

	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
	"einoclaw-build/internal/tool"
)

// yieldTool 是子 agent 的显式终止工具：调用它表示「任务完成，产出如下」。
// 它实现 tool.Terminal，循环在它执行成功后立即结束；Manager 从调用参数里提取 data。
type yieldTool struct{}

func (yieldTool) Name() string { return "yield" }
func (yieldTool) Description() string {
	return "任务完成时调用，提交最终结构化产出 data 并结束运行。这是返回结果的唯一方式；调用后不会再执行任何工具。"
}
func (yieldTool) Parameters() map[string]any {
	return map[string]any{"data": map[string]any{"type": "object", "description": "结构化产出；按任务要求的字段填写"}}
}
func (yieldTool) Required() []string            { return []string{"data"} }
func (yieldTool) Tier() permission.Tier         { return permission.TierRead }
func (yieldTool) Concurrency() tool.Concurrency { return tool.ConcurrencyShared }
func (yieldTool) IsTerminal() bool              { return true }

func (yieldTool) Execute(ctx context.Context, args map[string]any, sink *runtime.Sink) error {
	sink.Write([]byte("result submitted"))
	return nil
}

// NewYieldTool 构造 yield 工具（子 agent 用）。
func NewYieldTool() tool.Tool { return yieldTool{} }
