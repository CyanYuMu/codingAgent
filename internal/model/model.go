package model

import (
	"context"

	"github.com/cloudwego/eino/schema"

	"einoclaw-build/internal/message"
)

// ToolSpec 描述一个可供模型调用的工具（模型视角，不含执行逻辑）。
type ToolSpec struct {
	Name        string
	Description string
	Parameters  map[string]any // JSON Schema 的 properties 部分
}

// Usage 一次模型调用的 token 用量（provider 真值，P3 记账基石）。
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	ReasoningTokens  int // 思考/推理 token
	CachedTokens     int // 提示词缓存命中
}

// ToolCallDelta 流式下工具调用的一个增量片段。
// 一次完整工具调用 = 多个同 CallID 的 delta 按序拼接（P1 循环负责合并）。
type ToolCallDelta struct {
	CallID string
	Name   string
	Args   string
}

// ModelEvent 模型流式输出的一个增量事件。
// 通常一次只有一个字段非空：要么正文、要么思考、要么工具调用增量。
type ModelEvent struct {
	Text      string
	Thinking  string
	ToolCalls []ToolCallDelta
}

// Stream 是一次流式调用的事件流。Recv 直到 io.EOF。
type Stream struct {
	reader *schema.StreamReader[*schema.AgenticMessage]
	usage  Usage
}

// Usage 返回本次调用的用量（流结束后才完整）。
func (s *Stream) Usage() Usage { return s.usage }

// Model 是模型客户端抽象 —— 唯一的 eino 依赖点。
type Model interface {
	Stream(ctx context.Context, msgs []message.Message, tools []ToolSpec) (*Stream, error)
}

// Config 描述要构建的模型。
type Config struct {
	Provider string // qwen | openai | ark | deepseek
	APIKey   string
	BaseURL  string
	Model    string // 模型 ID，如 "deepseek-chat" / "qwen-plus" / "gpt-4o"
}
