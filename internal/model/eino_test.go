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
		PromptTokens:          10,
		CompletionTokens:      5,
		TotalTokens:           15,
		PromptTokenDetails:    schema.PromptTokenDetails{CachedTokens: 3},
		CompletionTokensDetails: schema.CompletionTokensDetails{ReasoningTokens: 2},
	})
	if u.PromptTokens != 10 || u.CompletionTokens != 5 || u.TotalTokens != 15 ||
		u.CachedTokens != 3 || u.ReasoningTokens != 2 {
		t.Fatalf("usage = %+v", u)
	}
}
