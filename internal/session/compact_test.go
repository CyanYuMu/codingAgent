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
	keptID := s.LastEntryID()
	_ = s.Append(msg(message.RoleAssistant, "m3"))

	// 压缩：摘要 m0-m1，保留从 m2 起
	if err := s.Compact("SUMMARY", keptID, 100); err != nil {
		t.Fatal(err)
	}

	ms, err := s.Replay()
	if err != nil || len(ms) != 3 {
		t.Fatalf("replay = %d, err = %v", len(ms), err)
	}
	if ms[0].Blocks[0].Text != "SUMMARY" || ms[1].Blocks[0].Text != "m2" || ms[2].Blocks[0].Text != "m3" {
		t.Fatalf("replay = %+v", ms)
	}
	// ContextEntryIDs 与 Replay 对齐：摘要对应 compaction 条目
	ids, _ := s.ContextEntryIDs()
	if len(ids) != 3 || ids[1] != keptID {
		t.Fatalf("ids = %v", ids)
	}
	// 再次压缩：保留段起点可以是上一次摘要之后的新消息
	_ = s.Append(msg(message.RoleUser, "m4"))
	kept2 := s.LastEntryID()
	if err := s.Compact("SUMMARY2", kept2, 200); err != nil {
		t.Fatal(err)
	}
	ms, _ = s.Replay()
	if len(ms) != 2 || ms[0].Blocks[0].Text != "SUMMARY2" || ms[1].Blocks[0].Text != "m4" {
		t.Fatalf("second replay = %+v", ms)
	}
}

func TestV1CompactionWithoutFirstKeptKeepsTail(t *testing.T) {
	st := &MemoryStorage{}
	_ = st.Append(Entry{Type: EntrySession, Version: 1, ID: "old"})
	u := msg(message.RoleUser, "u1")
	_ = st.Append(Entry{Type: EntryMessage, Message: &u})
	_ = st.Append(Entry{Type: EntryCompaction, Compaction: &Compaction{Summary: "S"}})
	k := msg(message.RoleUser, "kept")
	_ = st.Append(Entry{Type: EntryMessage, Message: &k})
	s, err := Open(st)
	if err != nil {
		t.Fatal(err)
	}
	ms, _ := s.Replay()
	if len(ms) != 2 || ms[0].Blocks[0].Text != "S" || ms[1].Blocks[0].Text != "kept" {
		t.Fatalf("v1 replay = %+v", ms)
	}
}
