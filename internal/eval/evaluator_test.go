package eval

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
	"einoclaw-build/internal/runtime"
)

type fixtureModel struct {
	steps   []model.ModelEvent
	err     error
	ready   chan<- struct{}
	release <-chan struct{}
}

func (m *fixtureModel) Stream(ctx context.Context, _ []message.Message, _ []model.ToolSpec) (model.ModelStream, error) {
	if m.ready != nil {
		m.ready <- struct{}{}
		m.ready = nil
		select {
		case <-m.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if m.err != nil {
		return nil, m.err
	}
	ev := model.ModelEvent{Text: "done"}
	if len(m.steps) > 0 {
		ev, m.steps = m.steps[0], m.steps[1:]
	}
	return &fixtureStream{event: &ev}, nil
}

type fixtureStream struct{ event *model.ModelEvent }

func (s *fixtureStream) Recv() (model.ModelEvent, error) {
	if s.event == nil {
		return model.ModelEvent{}, io.EOF
	}
	ev := *s.event
	s.event = nil
	return ev, nil
}
func (*fixtureStream) Usage() model.Usage { return model.Usage{} }
func (*fixtureStream) Close()             {}

func TestConcurrentFixturesKeepIndependentWorkingDirectories(t *testing.T) {
	before, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ready, release := make(chan struct{}, 2), make(chan struct{})
	results := make(chan Result, 2)
	for _, value := range []string{"fixture-a", "fixture-b"} {
		go func() {
			m := &fixtureModel{ready: ready, release: release, steps: []model.ModelEvent{{ToolCalls: []model.ToolCallDelta{{Index: 0, CallID: "copy", Name: "bash", Args: `{"command":"cat input > result"}`}}}}}
			fx := Fixture{Name: value, Prompt: "copy input", Input: map[string]string{"input": value}, Expected: map[string]string{"result": value}}
			results <- RunWithSandbox(ctx, fx, m, nil, runtime.SandboxConfig{Mode: "off"})
		}()
	}
	for range 2 {
		select {
		case <-ready:
		case <-ctx.Done():
			t.Fatal("fixtures did not initialize concurrently")
		}
	}
	close(release)
	for range 2 {
		r := <-results
		if !r.Pass {
			t.Errorf("%s: %s", r.Name, r.Detail)
		}
	}
	after, err := os.Getwd()
	if err != nil || after != before {
		t.Fatalf("process cwd changed: %q -> %q (%v)", before, after, err)
	}
}

func TestFixtureBoundariesAndModelFailure(t *testing.T) {
	cfg := runtime.SandboxConfig{Mode: "off"}
	fx := Fixture{Name: "escape", Prompt: "done", Input: map[string]string{"../escape": "bad"}}
	r := RunWithSandbox(context.Background(), fx, &fixtureModel{}, nil, cfg)
	if r.Pass || !strings.Contains(r.Detail, "outside workspace") {
		t.Fatalf("accepted input escape: %+v", r)
	}
	out := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(out, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	fx = Fixture{Name: "expected-escape", Prompt: "done", Expected: map[string]string{out: "secret"}}
	if r = RunWithSandbox(context.Background(), fx, &fixtureModel{}, nil, cfg); r.Pass {
		t.Fatal("expected files read outside workspace")
	}
	fx = Fixture{Name: "model-failure", Prompt: "done", Input: map[string]string{"a": "same"}, Expected: map[string]string{"a": "same"}}
	r = RunWithSandbox(context.Background(), fx, &fixtureModel{err: errors.New("fixture model failed")}, nil, cfg)
	if r.Pass || !strings.Contains(r.Detail, "fixture model failed") {
		t.Fatalf("matching files hid model failure: %+v", r)
	}
}
