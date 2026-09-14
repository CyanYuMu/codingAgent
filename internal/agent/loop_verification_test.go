package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
	"einoclaw-build/internal/tool"
	"einoclaw-build/internal/verification"
	"einoclaw-build/internal/workspace"
)

func TestCompletionGateSteersBeforeFinishing(t *testing.T) {
	fm := &fakeModel{steps: []func() (model.ModelStream, error){textStep("done"), callStep("verify1", "echo", `{"v":"verification evidence"}`), textStep("verified")}}
	cc := NewMemoryContext(nil)
	a := newTestAgent(fm, cc)
	checked := 0
	a.SetCompletionCheck(func(context.Context) error {
		checked++
		if checked == 1 {
			return errors.New("run verify")
		}
		return nil
	})
	evs := drain(a.Run(context.Background(), nil))
	if len(fm.calls) != 3 || checked != 2 {
		t.Fatalf("calls=%d checks=%d", len(fm.calls), checked)
	}
	if hasEvent(evs, func(e AgentEvent) bool { return e.Type == EventError }) {
		t.Fatal("recovered gate errored")
	}
	msgs := cc.Messages()
	if msgs[1].Role != message.RoleUser {
		t.Fatal("missing persistent gate feedback")
	}
}

func TestCompletionGateBoundsUncooperativeModel(t *testing.T) {
	fm := &fakeModel{}
	a := newTestAgent(fm, NewMemoryContext(nil))
	a.SetCompletionCheck(func(context.Context) error { return errors.New("tests failing") })
	evs := drain(a.Run(context.Background(), nil))
	if len(fm.calls) != 3 || !hasEvent(evs, func(e AgentEvent) bool { return e.Type == EventError }) {
		t.Fatal("unverified task silently completed")
	}
}

func TestCompletionGateDoesNotTreatIterationLimitAsSuccess(t *testing.T) {
	fm := &fakeModel{steps: []func() (model.ModelStream, error){callStep("one", "echo", `{"v":"still working"}`)}}
	a := newTestAgent(fm, NewMemoryContext(nil))
	a.SetMaxIterations(1)
	a.SetCompletionCheck(func(context.Context) error { return nil })
	if !hasEvent(drain(a.Run(context.Background(), nil)), func(e AgentEvent) bool { return e.Type == EventError }) {
		t.Fatal("iteration limit reported as success")
	}
}

func TestCodingLoopEditsFailsRepairsAndVerifies(t *testing.T) {
	w, err := workspace.Open(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	os.WriteFile(filepath.Join(w.Path(), "result"), []byte("initial"), 0o644)
	s, err := runtime.NewProcessSandbox(w.Path(), runtime.SandboxConfig{Mode: "off"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	v, err := verification.New(w, s, verification.Config{Commands: []string{"test \"$(cat result)\" = good"}})
	if err != nil {
		t.Fatal(err)
	}
	b := runtime.NewBash(w.Path())
	b.SetSandbox(s)
	reg := tool.NewRegistry()
	for _, t := range tool.ScopedBuiltins(b, nil, w) {
		reg.Register(t)
	}
	reg.Register(tool.NewVerifyTool(v))
	fm := &fakeModel{steps: []func() (model.ModelStream, error){
		callStep("r1", "read_file", `{"file_path":"result"}`),
		callStep("w1", "write_file", `{"file_path":"result","content":"bad"}`),
		textStep("done too early"),
		callStep("v1", "verify", `{}`),
		callStep("w2", "edit", `{"file_path":"result","old_string":"bad","new_string":"good"}`),
		callStep("v2", "verify", `{}`), textStep("verified done"),
	}}
	cc := NewMemoryContext(nil)
	a := New("test", fm, reg, tool.NewExecutor(reg, permission.ModeYolo, nil), cc)
	a.SetCompletionCheck(v.CheckCompletion)
	events := drain(a.Run(context.Background(), nil))
	if len(fm.calls) != 7 || !w.Verified() {
		t.Fatalf("calls=%d verified=%v", len(fm.calls), w.Verified())
	}
	fails, passes := 0, 0
	for _, e := range events {
		if e.Type == EventError {
			t.Fatal(e.Err)
		}
		if e.Type == EventToolEnd && e.ToolEnd.Name == "verify" {
			if e.ToolEnd.IsError {
				fails++
			} else {
				passes++
			}
		}
	}
	if fails != 1 || passes != 1 {
		t.Fatalf("verification fails=%d passes=%d", fails, passes)
	}
	h, _ := w.History()
	if len(h) != 2 {
		t.Fatalf("missing edit journals: %d", len(h))
	}
}
