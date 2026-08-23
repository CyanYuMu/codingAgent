package subagent

import "time"

// SubagentSpec 一个子 agent 的声明。
type SubagentSpec struct {
	Name         string
	Description  string
	SystemPrompt string
	WhenToUse    string        // 触发场景，task 描述枚举时带上
	OutputSchema map[string]any // 可选 JSON Schema，校验 yield 产出
	Timeout      time.Duration  // wall-clock 超时（0 = 无）
	MaxTurns     int            // 工具循环上限（默认 50）
}

// Status 子 agent 执行状态。
type Status int

const (
	StatusPending Status = iota
	StatusRunning
	StatusCompleted
	StatusFailed
	StatusKilled
)

// Task 一个派发任务。
type Task struct {
	Subagent string
	Prompt   string
}

// Result 一个子 agent 的执行结果。
type Result struct {
	ID     string
	Status Status
	Data   map[string]any // yield 的结构化产出
	Text   string         // 失败/无 yield 时的文本
	Err    error
}
