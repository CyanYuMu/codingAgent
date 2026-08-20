package message

import "testing"

func TestNewUserMessage(t *testing.T) {
	m := NewUserMessage("hello")
	if m.Role != RoleUser {
		t.Fatalf("role = %q, want %q", m.Role, RoleUser)
	}
	if len(m.Blocks) != 1 || m.Blocks[0].Kind != BlockText || m.Blocks[0].Text != "hello" {
		t.Fatalf("blocks = %+v, want single text block 'hello'", m.Blocks)
	}
}

func TestNewSystemMessage(t *testing.T) {
	m := NewSystemMessage("sys")
	if m.Role != RoleSystem {
		t.Fatalf("role = %q, want %q", m.Role, RoleSystem)
	}
}

func TestNewToolMessage(t *testing.T) {
	m := NewToolMessage("c1", "read", "content", true)
	if m.Role != RoleTool {
		t.Fatalf("role = %q, want %q", m.Role, RoleTool)
	}
	if len(m.Blocks) != 1 {
		t.Fatalf("blocks len = %d, want 1", len(m.Blocks))
	}
	b := m.Blocks[0]
	if b.Kind != BlockToolResult || b.ToolResult == nil {
		t.Fatalf("block = %+v, want toolResult", b)
	}
	r := b.ToolResult
	if r.ToolCallID != "c1" || r.Name != "read" || r.Content != "content" || !r.IsError {
		t.Fatalf("toolResult = %+v", r)
	}
}
