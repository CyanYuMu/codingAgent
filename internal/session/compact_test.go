package session

import (
	"testing"

	"einoclaw-build/internal/message"
)

func TestCompactReplacesPrefixWithSummary(t *testing.T) {
	s, _ := New("s1", &MemoryStorage{})
	_ = s.Append(msg(message.RoleUser, "m0"))
	_ = s.Append(msg(message.RoleAssistant, "m1"))
	_ = s.Append(msg(message.RoleUser, "m2"))
	_ = s.Append(msg(message.RoleAssistant, "m3"))

	// 压缩：摘要 m0-m1，保留 m2-m3
	kept := []message.Message{msg(message.RoleUser, "m2"), msg(message.RoleAssistant, "m3")}
	if err := s.Compact("SUMMARY", kept); err != nil {
		t.Fatal(err)
	}

	ms, err := s.Replay()
	if err != nil || len(ms) != 3 {
		t.Fatalf("replay = %d, err = %v", len(ms), err)
	}
	if ms[0].Blocks[0].Text != "SUMMARY" || ms[1].Blocks[0].Text != "m2" || ms[2].Blocks[0].Text != "m3" {
		t.Fatalf("replay = %+v", ms)
	}
}
