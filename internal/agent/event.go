package agent

import (
	"time"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
)

// EventType 事件类型。
type EventType int

const (
	EventAgentStart    EventType = iota // run 开始
	EventTurnStart                      // turn 开始
	EventMessageStart                   // assistant 消息开始（流即将到来）
	EventMessageUpdate                  // 流式增量（只含 delta）
	EventMessageEnd                     // 消息定稿（完整累积的消息）
	EventToolStart                      // 工具调用开始
	EventToolEnd                        // 工具调用结束（含结果）
	EventTurnEnd                        // turn 结束
	EventAgentEnd                       // run 结束
	EventError                          // 出错
	EventCompaction                     // 上下文已压缩（threshold | mid-turn | overflow）
	EventRetry                          // 模型瞬时错误，退避后重试
	EventTerminated                     // 终止型工具（如 yield）结束了本次 run
)

// AgentEvent 是 agent 执行过程中吐出的流式事件。
type AgentEvent struct {
	Type EventType

	// EventMessageUpdate: 流式增量
	Update *MessageUpdate
	// EventMessageEnd: 定稿消息
	Ended *MessageEnd
	// EventToolStart: 工具调用
	ToolStart *ToolStart
	// EventToolEnd: 工具结果
	ToolEnd *ToolEnd
	// EventError
	Err error
	// EventCompaction
	Compaction *CompactionInfo
	// EventRetry
	Retry *RetryInfo
	// EventTerminated
	Terminated *TerminatedInfo
}

// MessageUpdate 是一次流式增量。P1 只含 text/thinking（TUI 渲染用）。
type MessageUpdate struct {
	Text     string
	Thinking string
}

// MessageEnd 是定稿的完整 assistant 消息。
type MessageEnd struct {
	Message message.Message
	Usage   model.Usage // 本次 turn 的用量（P3 上下文记账）
}

// ToolStart 是一次工具调用的开始（名 + 参数）。
type ToolStart struct {
	ID   string
	Name string
	Args string
}

// ToolEnd 是一次工具调用的结束（名 + 结果文本 + 是否出错）。
type ToolEnd struct {
	ID      string
	Name    string
	Content string
	IsError bool
}

// CompactionInfo 说明压缩原因。
type CompactionInfo struct{ Reason string }

// RetryInfo 说明一次重试。
type RetryInfo struct {
	Attempt int
	Delay   time.Duration
	Err     error
}

// TerminatedInfo 说明哪个终止型工具结束了 run。
type TerminatedInfo struct{ ToolName string }
