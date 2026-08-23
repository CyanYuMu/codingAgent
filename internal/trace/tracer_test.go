package trace

import (
	"testing"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
	"einoclaw-build/internal/session"
)

func TestAnalyze(t *testing.T) {
	s, _ := session.New("s1", &session.MemoryStorage{})
	_ = s.AppendWithUsage(message.Message{Role: message.RoleAssistant, Blocks: []message.ContentBlock{{Kind: message.BlockText, Text: "hi"}}}, model.Usage{PromptTokens: 100, CompletionTokens: 50})
	_ = s.AppendWithUsage(message.Message{Role: message.RoleAssistant, Blocks: []message.ContentBlock{
		{Kind: message.BlockText, Text: "doing"},
		{Kind: message.BlockToolCall, ToolCall: &message.ToolCall{ID: "c1", Name: "bash"}},
	}}, model.Usage{PromptTokens: 200, CompletionTokens: 30})
	_ = s.Append(message.Message{Role: message.RoleUser, Blocks: []message.ContentBlock{{Kind: message.BlockText, Text: "u"}}})

	st, err := Analyze(s)
	if err != nil {
		t.Fatal(err)
	}
	if st.Turns != 2 || st.ToolCalls != 1 || st.PromptTokens != 300 || st.CompletionTokens != 80 {
		t.Fatalf("stats = %+v", st)
	}
}
