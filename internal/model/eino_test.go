package model

import (
	"testing"

	"github.com/cloudwego/eino/schema"

	"einoclaw-build/internal/message"
)

func TestTextOf(t *testing.T) {
	m := message.Message{Role: message.RoleUser, Blocks: []message.ContentBlock{
		{Kind: message.BlockText, Text: "a"},
		{Kind: message.BlockThinking, Thinking: "think"},
		{Kind: message.BlockText, Text: "b"},
	}}
	if got := textOf(m); got != "ab" {
		t.Fatalf("textOf = %q, want %q", got, "ab")
	}
}

func TestToAgenticMessagesSystemUser(t *testing.T) {
	msgs := []message.Message{
		message.NewSystemMessage("sys"),
		message.NewUserMessage("hi"),
	}
	out := toAgenticMessages(msgs)
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	if out[0].Role != schema.AgenticRoleTypeSystem {
		t.Fatalf("out[0].Role = %q, want system", out[0].Role)
	}
	if got := out[0].ContentBlocks[0].UserInputText.Text; got != "sys" {
		t.Fatalf("out[0] text = %q, want sys", got)
	}
	if out[1].Role != schema.AgenticRoleTypeUser {
		t.Fatalf("out[1].Role = %q, want user", out[1].Role)
	}
	if got := out[1].ContentBlocks[0].UserInputText.Text; got != "hi" {
		t.Fatalf("out[1] text = %q, want hi", got)
	}
}

func TestFromSchemaUsage(t *testing.T) {
	u := fromSchemaUsage(&schema.TokenUsage{
		PromptTokens:            10,
		CompletionTokens:        5,
		TotalTokens:             15,
		PromptTokenDetails:      schema.PromptTokenDetails{CachedTokens: 3},
		CompletionTokensDetails: schema.CompletionTokensDetails{ReasoningTokens: 2},
	})
	if u.PromptTokens != 10 || u.CompletionTokens != 5 || u.TotalTokens != 15 ||
		u.CachedTokens != 3 || u.ReasoningTokens != 2 {
		t.Fatalf("usage = %+v", u)
	}
}

func TestToSchemaTools(t *testing.T) {
	tools := []ToolSpec{
		{Name: "read", Description: "d", Parameters: map[string]any{"file_path": map[string]any{"type": "string"}}},
	}
	infos := toSchemaTools(tools)
	if len(infos) != 1 || infos[0].Name != "read" || infos[0].Desc != "d" {
		t.Fatalf("infos = %+v", infos)
	}
	if infos[0].ParamsOneOf == nil {
		t.Fatal("ParamsOneOf 应为非空（工具参数已描述）")
	}
}

func TestToAgenticMessagesAssistantAndTool(t *testing.T) {
	assistant := message.Message{
		Role: message.RoleAssistant,
		Blocks: []message.ContentBlock{
			{Kind: message.BlockText, Text: "checking"},
			{Kind: message.BlockToolCall, ToolCall: &message.ToolCall{ID: "c1", Name: "bash", Args: `{"command":"ls"}`}},
		},
	}
	toolMsg := message.Message{
		Role: message.RoleTool,
		Blocks: []message.ContentBlock{
			{Kind: message.BlockToolResult, ToolResult: &message.ToolResult{ToolCallID: "c1", Name: "bash", Content: "a.go"}},
		},
	}
	out := toAgenticMessages([]message.Message{assistant, toolMsg})
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	if out[0].Role != schema.AgenticRoleTypeAssistant {
		t.Fatalf("out[0].Role = %v, want assistant", out[0].Role)
	}
	if !hasBlock(out[0], func(b *schema.ContentBlock) bool {
		return b.FunctionToolCall != nil && b.FunctionToolCall.Name == "bash"
	}) {
		t.Fatal("assistant 应含 bash 工具调用块")
	}
	if out[1].Role != schema.AgenticRoleTypeUser {
		t.Fatalf("out[1].Role = %v, want user", out[1].Role)
	}
	if !hasBlock(out[1], func(b *schema.ContentBlock) bool {
		return b.FunctionToolResult != nil && b.FunctionToolResult.Name == "bash"
	}) {
		t.Fatal("tool 消息应含 bash 工具结果块")
	}
}

func hasBlock(m *schema.AgenticMessage, f func(*schema.ContentBlock) bool) bool {
	for _, b := range m.ContentBlocks {
		if f(b) {
			return true
		}
	}
	return false
}
