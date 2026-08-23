package session

import (
	"testing"

	"einoclaw-build/internal/message"
)

func toolCallMsg(id, name string) message.Message {
	return message.Message{Role: message.RoleAssistant, Blocks: []message.ContentBlock{
		{Kind: message.BlockToolCall, ToolCall: &message.ToolCall{ID: id, Name: name, Args: "{}"}},
	}}
}

func TestAppendBuildsParentChain(t *testing.T) {
	s, _ := New("s1", &MemoryStorage{})
	_ = s.Append(msg(message.RoleUser, "u1"))
	_ = s.Append(msg(message.RoleAssistant, "a1"))
	es, _ := s.Entries()
	if len(es) != 3 || es[1].ID == "" || es[2].ParentID != es[1].ID || es[1].ParentID != "" {
		t.Fatalf("chain wrong: %+v", es)
	}
	if s.LastEntryID() != es[2].ID {
		t.Fatalf("leaf = %q, want %q", s.LastEntryID(), es[2].ID)
	}
	if es[0].Version != CurrentVersion || es[0].SessionID != "s1" {
		t.Fatalf("header = %+v", es[0])
	}
}

func TestReplayRepairsDanglingToolCall(t *testing.T) {
	s, _ := New("s1", &MemoryStorage{})
	_ = s.Append(msg(message.RoleUser, "u1"))
	_ = s.Append(toolCallMsg("c1", "bash")) // 中断：没有 tool 结果
	_ = s.Append(msg(message.RoleUser, "u2"))
	ms, _ := s.Replay()
	if len(ms) != 4 {
		t.Fatalf("replay = %d, want 4 (repair inserted): %+v", len(ms), ms)
	}
	tr := ms[2]
	if tr.Role != message.RoleTool || tr.Blocks[0].ToolResult == nil || tr.Blocks[0].ToolResult.ToolCallID != "c1" || !tr.Blocks[0].ToolResult.IsError {
		t.Fatalf("repair msg = %+v", tr)
	}
	// 有结果的调用不修复
	s2, _ := New("s2", &MemoryStorage{})
	_ = s2.Append(toolCallMsg("c1", "bash"))
	_ = s2.Append(message.NewToolMessage("c1", "bash", "ok", false))
	ms2, _ := s2.Replay()
	if len(ms2) != 2 {
		t.Fatalf("no repair expected, got %d", len(ms2))
	}
}

func TestOpenV1FileIsLinear(t *testing.T) {
	st := &MemoryStorage{}
	_ = st.Append(Entry{Type: EntrySession, Version: 1, ID: "old"})
	u := msg(message.RoleUser, "u1")
	a := msg(message.RoleAssistant, "a1")
	_ = st.Append(Entry{Type: EntryMessage, Message: &u})
	_ = st.Append(Entry{Type: EntryMessage, Message: &a})
	s, err := Open(st)
	if err != nil {
		t.Fatal(err)
	}
	if s.Header().ID != "old" {
		t.Fatalf("header id = %q", s.Header().ID)
	}
	ms, _ := s.Replay()
	if len(ms) != 2 || ms[1].Blocks[0].Text != "a1" {
		t.Fatalf("v1 replay = %+v", ms)
	}
	_ = s.Append(msg(message.RoleUser, "u2"))
	ms, _ = s.Replay()
	if len(ms) != 3 {
		t.Fatalf("after append = %d", len(ms))
	}
}

func TestOpenReopensV2FileAtLeaf(t *testing.T) {
	st := &MemoryStorage{}
	s, _ := New("s1", st)
	_ = s.Append(msg(message.RoleUser, "u1"))
	_ = s.Append(msg(message.RoleAssistant, "a1"))
	_ = s.SetTitle("标题")
	re, err := Open(st)
	if err != nil {
		t.Fatal(err)
	}
	if re.Header().Title != "标题" || re.LastEntryID() != s.LastEntryID() {
		t.Fatalf("reopen header=%+v leaf=%q want %q", re.Header(), re.LastEntryID(), s.LastEntryID())
	}
	ms, _ := re.Replay()
	if len(ms) != 2 {
		t.Fatalf("reopen replay = %d", len(ms))
	}
}

func TestInitAndCustomDoNotProduceMessages(t *testing.T) {
	s, _ := NewWithHeader(Header{ID: "c", CWD: "/x", ParentSession: "p"}, &MemoryStorage{})
	_ = s.AppendInit(SessionInit{Agent: "explorer", Task: "t"})
	_ = s.AppendCustom("tool_execution_start", map[string]any{"tool": "bash"})
	_ = s.Append(msg(message.RoleUser, "u"))
	ms, _ := s.Replay()
	if len(ms) != 1 {
		t.Fatalf("replay = %+v", ms)
	}
	if s.Header().ParentSession != "p" || s.Header().CWD != "/x" {
		t.Fatalf("header = %+v", s.Header())
	}
}
