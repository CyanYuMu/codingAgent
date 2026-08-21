package session

import (
	"testing"

	"einoclaw-build/internal/message"
)

func msg(role message.Role, text string) message.Message {
	return message.Message{Role: role, Blocks: []message.ContentBlock{{Kind: message.BlockText, Text: text}}}
}

func TestReplayAppendsInOrder(t *testing.T) {
	s, _ := New("s1", &MemoryStorage{})
	_ = s.Append(msg(message.RoleUser, "u1"))
	_ = s.Append(msg(message.RoleAssistant, "a1"))

	ms, err := s.Replay()
	if err != nil || len(ms) != 2 {
		t.Fatalf("replay = %d, err = %v", len(ms), err)
	}
	if ms[0].Blocks[0].Text != "u1" || ms[1].Blocks[0].Text != "a1" {
		t.Fatalf("replay = %+v", ms)
	}
}

func TestResetSealsOldContext(t *testing.T) {
	s, _ := New("s1", &MemoryStorage{})
	_ = s.Append(msg(message.RoleUser, "old"))
	_ = s.Reset()
	_ = s.Append(msg(message.RoleUser, "new"))

	ms, err := s.Replay()
	if err != nil || len(ms) != 1 {
		t.Fatalf("replay = %d, err = %v", len(ms), err)
	}
	if ms[0].Blocks[0].Text != "new" {
		t.Fatalf("replay = %+v", ms)
	}
}

func TestForkCopiesAndIsolates(t *testing.T) {
	parent, _ := New("p", &MemoryStorage{})
	_ = parent.Append(msg(message.RoleUser, "u1"))
	_ = parent.Append(msg(message.RoleAssistant, "a1"))

	child, err := parent.Fork("c", &MemoryStorage{})
	if err != nil {
		t.Fatal(err)
	}
	// 子会话初始与父相同
	cm, _ := child.Replay()
	if len(cm) != 2 {
		t.Fatalf("child replay = %d, want 2", len(cm))
	}
	// 子会话追加不影响父
	_ = child.Append(msg(message.RoleUser, "u2"))
	pm, _ := parent.Replay()
	if len(pm) != 2 {
		t.Fatalf("parent replay after fork append = %d, want 2", len(pm))
	}
}
