package agent

import (
	"context"
	"errors"
	"io"
	"testing"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
)

func TestRunObservedDeliversAllDeltasAndFinalError(t *testing.T) {
	want := errors.New("stream failed")
	deltas := make([]model.ModelEvent, 128)
	for i := range deltas {
		deltas[i].Text = "x"
	}
	fm := &fakeModel{steps: []func() (model.ModelStream, error){func() (model.ModelStream, error) {
		return &fakeStream{events: deltas, err: want, usage: model.Usage{TotalTokens: 42}}, nil
	}}}
	a := newTestAgent(fm, NewMemoryContext(nil))
	updates, ends := 0, 0
	var gotErr error
	var last EventType
	a.RunObserved(context.Background(), nil, func(ev AgentEvent) {
		last = ev.Type
		switch ev.Type {
		case EventMessageUpdate:
			updates++
		case EventMessageEnd:
			ends++
			if ev.Ended.Usage.TotalTokens != 42 {
				t.Fatalf("usage = %+v", ev.Ended.Usage)
			}
		case EventError:
			gotErr = ev.Err
		}
	})
	if updates != 128 || ends != 1 || !errors.Is(gotErr, want) || last != EventAgentEnd {
		t.Fatalf("updates=%d ends=%d err=%v last=%v", updates, ends, gotErr, last)
	}
}

func TestRequestEOFRetriesAreBounded(t *testing.T) {
	for _, recover := range []bool{true, false} {
		steps := []func() (model.ModelStream, error){errStep(io.EOF)}
		if recover {
			steps = append(steps, textStep("recovered"))
		} else {
			steps = append(steps, errStep(io.EOF))
		}
		fm := &fakeModel{steps: steps}
		a := newTestAgent(fm, NewMemoryContext(nil))
		a.maxRetries = 1
		events := drain(a.Run(context.Background(), nil))
		failed := hasEvent(events, func(ev AgentEvent) bool { return ev.Type == EventError })
		if len(fm.calls) != 2 || failed == recover {
			t.Fatalf("recover=%v calls=%d failed=%v", recover, len(fm.calls), failed)
		}
	}
}

func TestRunObservedCanStopBeforeToolExecution(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cc := NewMemoryContext(nil)
	a := newTestAgent(&fakeModel{steps: []func() (model.ModelStream, error){
		callStep("c1", "echo", `{"v":"must not execute"}`),
	}}, cc)
	tools := 0
	a.RunObserved(ctx, nil, func(ev AgentEvent) {
		if ev.Type == EventMessageEnd {
			cancel() // 预算停机必须在同一步启动工具之前生效。
		}
		if ev.Type == EventToolStart {
			tools++
		}
	})
	msgs := cc.Messages()
	if tools != 0 || len(msgs) != 1 || msgs[0].Role != message.RoleAssistant {
		t.Fatalf("tools=%d messages=%+v", tools, msgs)
	}
}

func TestIngressFailureStopsBeforeModelCall(t *testing.T) {
	fm := &fakeModel{}
	a := newTestAgent(fm, NewMemoryContext(nil))
	want := errors.New("cannot persist inbox")
	a.SetBeforeStep(func(context.Context) error { return want })
	var got error
	a.RunObserved(context.Background(), nil, func(ev AgentEvent) {
		if ev.Type == EventError {
			got = ev.Err
		}
	})
	if !errors.Is(got, want) || len(fm.calls) != 0 {
		t.Fatalf("err=%v calls=%d", got, len(fm.calls))
	}
}
