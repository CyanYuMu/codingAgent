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
	// Concurrency 声明工具的并发性：Shared 可并行，Exclusive 必须串行（如 bash/write）。
	Concurrency() Concurrency
	// Execute 执行工具，把结果文本写入 sink（截断/落盘由 sink 处理）；返回 error 表示执行失败。
	Execute(ctx context.Context, args map[string]any, sink *runtime.Sink) error
}

// Terminal 可选接口：工具执行成功后终止本次 run（如子 agent 的 yield）。
type Terminal interface{ IsTerminal() bool }

// RequiredParams 可选接口：声明必填参数名（进入工具定义的 required）。
type RequiredParams interface{ Required() []string }

// Concurrency 工具的并发性。
type Concurrency int

const (
	ConcurrencyShared    Concurrency = iota // 可并行
	ConcurrencyExclusive                    // 必须串行
)
