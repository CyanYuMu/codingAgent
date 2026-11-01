package subagent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"einoclaw-build/internal/agent"
	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
)

type failingInboxContext struct {
	*agent.MemoryContext
	fail bool
}

func (c *failingInboxContext) Record(m message.Message, u model.Usage) error {
	if c.fail {
		return errors.New("storage unavailable")
	}
	return c.MemoryContext.Record(m, u)
}

func TestPeerWaitWakesAndIngressAcknowledgesOnce(t *testing.T) {
	for _, prequeued := range []bool{false, true} {
		t.Run(map[bool]string{false: "during_wait", true: "before_wait"}[prequeued], func(t *testing.T) {
			m := NewManager(baseOpts(&scriptModel{}, t.TempDir()))
			r := newRun("Peer", "explorer", 1)
			m.register(r)
			r.setStatus(StatusRunning)
			h := hubTool{mgr: m, self: r.Name()}
			if prequeued {
				_, _ = m.Deliver(MainName, r.Name(), "response-47", "")
			}
			done := make(chan string, 1)
			go func() { done <- h.wait(context.Background(), nil, 3*time.Second) }()
			if !prequeued {
				if _, err := m.Deliver(MainName, r.Name(), "response-47", ""); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case out := <-done:
				if !strings.Contains(out, "response-47") {
					t.Fatalf("wait=%s", out)
				}
			case <-time.After(time.Second):
				t.Fatal("peer message did not wake wait")
			}
			cc := &failingInboxContext{MemoryContext: agent.NewMemoryContext(nil), fail: true}
			if err := m.recordPeerMail(r, cc); err == nil {
				t.Fatal("failed storage accepted")
			}
			if len(m.box(r.Name()).peek("")) != 1 {
				t.Fatal("unpersisted mail was acknowledged")
			}
			cc.fail = false
			for i := 0; i < 2; i++ {
				if err := m.recordPeerMail(r, cc); err != nil {
					t.Fatal(err)
				}
			}
			if msgs := cc.Messages(); len(msgs) != 1 || !strings.Contains(messageText(msgs[0]), "response-47") {
				t.Fatalf("messages=%+v", msgs)
			}
		})
	}
}

func TestIngressDoesNotConsumeOtherSessions(t *testing.T) {
	m := NewManager(baseOpts(&scriptModel{}, t.TempDir()))
	r := newOwnedRun("Peer", "explorer", 1, MainName, "current")
	m.box(r.Name()).push(Mail{DeliveryID: "old", SessionID: "other", Text: "old"})
	cc := agent.NewMemoryContext(nil)
	if err := m.recordPeerMail(r, cc); err != nil {
		t.Fatal(err)
	}
	if len(cc.Messages()) != 0 || len(m.box(r.Name()).peek("other")) != 1 {
		t.Fatal("cross-session mail consumed")
	}
}
