package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"einoclaw-build/internal/model"
)

type auditEntry struct {
	Kind string
	Data map[string]any
}

func TestAuditDistinguishesDeadlineFromCancellation(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	cc := &auditContext{MemoryContext: NewMemoryContext(nil)}
	fm := &fakeModel{}
	a := newTestAgent(fm, cc)
	a.RunObserved(ctx, nil, func(AgentEvent) {})
	end := cc.entries[len(cc.entries)-1]
	if end.Data["stop_reason"] != "timeout" || len(fm.calls) != 0 {
		t.Fatalf("end=%+v calls=%d", end, len(fm.calls))
	}
}

type auditContext struct {
	*MemoryContext
	entries []auditEntry
	failAt  string
}

func (c *auditContext) RecordAudit(kind string, data any) error {
	if kind == c.failAt {
		return errors.New("audit storage failed")
	}
	b, _ := json.Marshal(data)
	var value map[string]any
	_ = json.Unmarshal(b, &value)
	c.entries = append(c.entries, auditEntry{kind, value})
	return nil
}

func TestAuditCapturesRetryFailureAndStopWithoutProviderBody(t *testing.T) {
	cc := &auditContext{MemoryContext: NewMemoryContext(nil)}
	fm := &fakeModel{steps: []func() (model.ModelStream, error){errStep(errors.New("429 private-provider-body")), errStep(errors.New("fatal private-provider-body"))}}
	a := newTestAgent(fm, cc)
	drain(a.Run(context.Background(), nil))
	counts := map[string]int{}
	for _, e := range cc.entries {
		counts[e.Kind]++
		b, _ := json.Marshal(e.Data)
		if strings.Contains(string(b), "private-provider-body") {
			t.Fatal("provider contents copied into audit")
		}
		if e.Data["run_id"] != cc.entries[0].Data["run_id"] {
			t.Fatal("run id mismatch")
		}
	}
	end := cc.entries[len(cc.entries)-1]
	if counts["model_request"] != 2 || counts["model_retry"] != 1 || counts["run_error"] != 1 || end.Kind != "run_end" || end.Data["stop_reason"] != "error" || end.Data["model_attempts"] != float64(2) {
		t.Fatalf("audit=%+v", cc.entries)
	}
}

func TestAuditStopsBeforeModelWhenPersistenceFails(t *testing.T) {
	cc := &auditContext{MemoryContext: NewMemoryContext(nil), failAt: "model_request"}
	fm := &fakeModel{}
	a := newTestAgent(fm, cc)
	var failed bool
	a.RunObserved(context.Background(), nil, func(ev AgentEvent) {
		if ev.Type == EventError {
			failed = true
		}
	})
	if !failed || len(fm.calls) != 0 {
		t.Fatalf("failed=%v model calls=%d", failed, len(fm.calls))
	}
}

func TestAuditDistinguishesCanceledAndCompleted(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		cc := &auditContext{MemoryContext: NewMemoryContext(nil)}
		a := newTestAgent(&fakeModel{steps: []func() (model.ModelStream, error){textStep("done")}}, cc)
		a.RunObserved(ctx, nil, func(ev AgentEvent) {
			if canceled && ev.Type == EventMessageEnd {
				cancel()
			}
		})
		cancel()
		want := "completed"
		if canceled {
			want = "canceled"
		}
		end := cc.entries[len(cc.entries)-1]
		if end.Data["stop_reason"] != want {
			t.Fatalf("end=%+v", end)
		}
	}
}
