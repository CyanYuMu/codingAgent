package session

import (
	"path/filepath"
	"testing"

	"einoclaw-build/internal/message"
)

func TestMemoryStorageAppendEntries(t *testing.T) {
	st := &MemoryStorage{}
	_ = st.Append(Entry{Type: EntryMessage, Message: &message.Message{Role: message.RoleUser, Blocks: []message.ContentBlock{{Kind: message.BlockText, Text: "hi"}}}})
	_ = st.Append(Entry{Type: EntryReset})
	es, err := st.Entries()
	if err != nil || len(es) != 2 {
		t.Fatalf("entries = %d, err = %v", len(es), err)
	}
	if es[0].Type != EntryMessage || es[1].Type != EntryReset {
		t.Fatalf("entries = %+v", es)
	}
}

func TestFileStorageRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	fs, err := NewFileStorage(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()

	_ = fs.Append(Entry{Type: EntrySession, Version: 1, ID: "s1"})
	_ = fs.Append(Entry{Type: EntryMessage, Message: &message.Message{Role: message.RoleUser, Blocks: []message.ContentBlock{{Kind: message.BlockText, Text: "你好"}}}})

	es, err := fs.Entries()
	if err != nil || len(es) != 2 {
		t.Fatalf("entries = %d, err = %v", len(es), err)
	}
	if es[1].Message == nil || es[1].Message.Blocks[0].Text != "你好" {
		t.Fatalf("round-trip message = %+v", es[1])
	}
}
