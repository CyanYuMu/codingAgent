package subagent

import (
	"time"

	"einoclaw-build/internal/model"
)

// SubagentSpec 一个子 agent 的声明。
type SubagentSpec struct {
	Name         string
	Description  string
	SystemPrompt string
	WhenToUse    string         // 触发场景，task 描述枚举时带上
	OutputSchema map[string]any // 可选 JSON Schema，要求 yield 产出结构化 data
	Timeout      time.Duration  // wall-clock 超时（0 = 无）
	MaxTurns     int            // 工具循环上限（默认 50）
}

// Status 子 agent 执行状态。
type Status int

const (
	StatusPending   Status = iota
	StatusRunning          // 运行中
	StatusCompleted        // 正常结束（yield 或无工具调用的收尾）
	StatusFailed           // 模型/工具不可恢复错误、schema 要求未满足、未知 agent
	StatusKilled           // 预算耗尽后被硬杀（M2 使用）
	StatusTimeout          // wall-clock 超时
	StatusAborted          // 父取消
)

// StatusString 返回状态的可读名。
func StatusString(s Status) string {
	switch s {
	case StatusPending:
		return "pending"
	case StatusRunning:
		return "running"
	case StatusCompleted:
		return "completed"
	case StatusFailed:
		return "failed"
	case StatusKilled:
		return "killed"
	case StatusTimeout:
		return "timeout"
	case StatusAborted:
		return "aborted"
	}
	return "unknown"
}

// Task 一个派发任务。
type Task struct {
	Name     string // 稳定名；缺省 <subagent>-<序号>；用于 sidecar 文件名与审批标签
	Subagent string
	Prompt   string
}

// Result 一个子 agent 的执行结果。
type Result struct {
	ID          string // 子 agent 类型名
	Name        string // 本次运行名
	Status      Status
	Yielded     bool           // 是否通过 yield 显式终止
	Data        map[string]any // yield 的结构化产出
	Text        string         // 最后一段 assistant 文本（失败/无 yield 时的 partial）
	Err         error
	Usage       model.Usage
	Requests    int   // 模型调用次数
	DurationMs  int64 // 耗时
	SessionFile string // sidecar 转录路径（MemoryStorage 时为空）
}
