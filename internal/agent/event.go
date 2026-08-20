package agent

import "einoclaw-build/internal/message"

// EventType 事件类型。
type EventType int

const (
	EventAgentStart     EventType = iota // run 开始
	EventTurnStart                       // turn 开始
	EventMessageStart                    // assistant 消息开始（流即将到来）
	EventMessageUpdate                   // 流式增量（只含 delta）
	EventMessageEnd                      // 消息定稿（完整累积的消息）
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
	// EventError
	Err error
}

// MessageUpdate 是一次流式增量。P1 只含 text/thinking（TUI 渲染用）。
// 工具调用增量只进累积器，不上 TUI（P4 加工具展示时再扩展）。
type MessageUpdate struct {
	Text     string
	Thinking string
}

// MessageEnd 是定稿的完整 assistant 消息。
type MessageEnd struct {
	Message message.Message
}
