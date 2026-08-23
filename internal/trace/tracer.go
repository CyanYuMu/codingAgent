package trace

import (
	"einoclaw-build/internal/message"
	"einoclaw-build/internal/session"
)

// Stats 一次会话的用量/消息/工具聚合统计。
type Stats struct {
	Turns            int // assistant 消息数
	ToolCalls        int // 工具调用数
	PromptTokens     int
	CompletionTokens int
}

// Analyze 读 session 原始条目（JSONL 即 trace），聚合统计。
func Analyze(s *session.Session) (Stats, error) {
	entries, err := s.Entries()
	if err != nil {
		return Stats{}, err
	}
	var st Stats
	for _, e := range entries {
		if e.Type != session.EntryMessage || e.Message == nil {
			continue
		}
		if e.Message.Role != message.RoleAssistant {
			continue
		}
		st.Turns++
		st.PromptTokens += e.Usage.PromptTokens
		st.CompletionTokens += e.Usage.CompletionTokens
		for _, b := range e.Message.Blocks {
			if b.Kind == message.BlockToolCall {
				st.ToolCalls++
			}
		}
	}
	return st, nil
}
