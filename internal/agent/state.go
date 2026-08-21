package agent

import (
	"strings"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
)

// streamAccumulator 把流式增量累积成一条完整的 assistant 消息。
// text/thinking 拼接；工具调用按 Index 合并（同 Index 的 Args 片段拼接，CallID 只在首个分块出现）。
type streamAccumulator struct {
	text      strings.Builder
	thinking  strings.Builder
	toolCalls map[int]*message.ToolCall // key = StreamingMeta.Index
	toolOrder []int                     // 保留工具调用到达顺序
}

func newStreamAccumulator() *streamAccumulator {
	return &streamAccumulator{toolCalls: map[int]*message.ToolCall{}}
}

func (a *streamAccumulator) add(ev model.ModelEvent) {
	if ev.Text != "" {
		a.text.WriteString(ev.Text)
	}
	if ev.Thinking != "" {
		a.thinking.WriteString(ev.Thinking)
	}
	for _, tc := range ev.ToolCalls {
		t, ok := a.toolCalls[tc.Index]
		if !ok {
			t = &message.ToolCall{}
			a.toolCalls[tc.Index] = t
			a.toolOrder = append(a.toolOrder, tc.Index)
		}
		if tc.CallID != "" {
			t.ID = tc.CallID
		}
		if tc.Name != "" {
			t.Name = tc.Name // 名字通常只在首个 delta 出现
		}
		t.Args += tc.Args // 参数片段拼接
	}
}

// message 产出累积完成的 assistant 消息。
// 块顺序固定：thinking → text → toolCalls（按到达序）。
func (a *streamAccumulator) message() message.Message {
	var blocks []message.ContentBlock
	if a.thinking.Len() > 0 {
		blocks = append(blocks, message.ContentBlock{Kind: message.BlockThinking, Thinking: a.thinking.String()})
	}
	if a.text.Len() > 0 {
		blocks = append(blocks, message.ContentBlock{Kind: message.BlockText, Text: a.text.String()})
	}
	for _, idx := range a.toolOrder {
		blocks = append(blocks, message.ContentBlock{Kind: message.BlockToolCall, ToolCall: a.toolCalls[idx]})
	}
	return message.Message{Role: message.RoleAssistant, Blocks: blocks}
}
