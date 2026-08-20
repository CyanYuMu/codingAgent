package agent

import (
	"testing"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
)

func TestAccumulatorMergesTextThinkingToolCalls(t *testing.T) {
	acc := newStreamAccumulator()
	acc.add(model.ModelEvent{Thinking: "think1"})
	acc.add(model.ModelEvent{Thinking: "think2"})
	acc.add(model.ModelEvent{Text: "hello "})
	acc.add(model.ModelEvent{Text: "world"})
	acc.add(model.ModelEvent{ToolCalls: []model.ToolCallDelta{{CallID: "c1", Name: "read", Args: `{"file_`}}})
	acc.add(model.ModelEvent{ToolCalls: []model.ToolCallDelta{{CallID: "c1", Args: `path":"a.go"}`}}})

	got := acc.message()
	if got.Role != message.RoleAssistant {
		t.Fatalf("role = %q, want assistant", got.Role)
	}
	if len(got.Blocks) != 3 {
		t.Fatalf("blocks = %d, want 3 (thinking,text,toolCall)", len(got.Blocks))
	}
	if got.Blocks[0].Kind != message.BlockThinking || got.Blocks[0].Thinking != "think1think2" {
		t.Fatalf("thinking block = %+v", got.Blocks[0])
	}
	if got.Blocks[1].Kind != message.BlockText || got.Blocks[1].Text != "hello world" {
		t.Fatalf("text block = %+v", got.Blocks[1])
	}
	tc := got.Blocks[2]
	if tc.Kind != message.BlockToolCall || tc.ToolCall == nil {
		t.Fatalf("toolCall block = %+v", tc)
	}
	if tc.ToolCall.ID != "c1" || tc.ToolCall.Name != "read" || tc.ToolCall.Args != `{"file_path":"a.go"}` {
		t.Fatalf("toolCall = %+v", tc.ToolCall)
	}
}

func TestAccumulatorEmpty(t *testing.T) {
	acc := newStreamAccumulator()
	got := acc.message()
	if got.Role != message.RoleAssistant || len(got.Blocks) != 0 {
		t.Fatalf("empty message = %+v", got)
	}
}
