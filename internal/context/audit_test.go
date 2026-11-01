package context

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/session"
)

func TestInputAuditDoesNotCopySystemPromptOrEnterReplay(t *testing.T) {
	s, err := session.New("audit", &session.MemoryStorage{})
	if err != nil {
		t.Fatal(err)
	}
	cm := New(s, nil, 128000, 1000, func(context.Context) []message.Message {
		return []message.Message{message.NewSystemMessage("private-system-value")}
	})
	for i := 0; i < 2; i++ {
		msgs, err := cm.Build(context.Background())
		if err != nil || len(msgs) != 1 {
			t.Fatalf("build=%+v err=%v", msgs, err)
		}
	}
	entries, _ := s.Entries()
	n := 0
	for _, e := range entries {
		if e.CustomType == "model_input" {
			n++
			if strings.Contains(string(e.Data), "private-system-value") {
				t.Fatal("system body copied to audit")
			}
			var d map[string]any
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatal(err)
			}
			if len(d["system_sha256"].(string)) != 64 {
				t.Fatal("missing prefix digest")
			}
		}
	}
	history, err := s.Replay()
	if n != 2 || err != nil || len(history) != 0 {
		t.Fatalf("n=%d history=%+v err=%v", n, history, err)
	}
}
