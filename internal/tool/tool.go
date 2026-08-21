package tool

import (
	"context"

	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
)

// Tool 是面向模型的工具入口。
type Tool interface {
	Name() string
	Description() string
	Parameters() map[string]any // JSON Schema properties（给模型的工具定义）
	Tier() permission.Tier
	// Execute 执行工具，把结果文本写入 sink（截断/落盘由 sink 处理）；返回 error 表示执行失败。
	Execute(ctx context.Context, args map[string]any, sink *runtime.Sink) error
}
