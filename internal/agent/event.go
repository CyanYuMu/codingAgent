package agent

import (
	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
)

// EventType 事件类型。
type EventType int

const (
	EventAgentStart     EventType = iota // run 开始
	EventTurnStart                       // turn 开始
	EventMessageStart                    // assistant 消息开始（流即将到来）
	EventMessageUpdate                   // 流式增量（只含 delta）
	EventMessageEnd                      // 消息定稿（完整累积的消息）
	EventToolStart                       // 工具调用开始
	EventToolEnd                         // 工具调用结束（含结果）
	EventTurnEnd                         // turn 结束
	EventAgentEnd                        // run 结束
	EventError                           // 出错
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

// ToolEnd 是一次工具调用的结束（名 + 结果文本）。
type ToolEnd struct {
	ID      string
	Name    string
	Content string
}
