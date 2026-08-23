package subagent

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
	"einoclaw-build/internal/tool"
)

type fakeStream struct {
	events []model.ModelEvent
	i      int
	delay  time.Duration
	ctx    context.Context
}

func (f *fakeStream) Recv() (model.ModelEvent, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-f.ctx.Done():
			return model.ModelEvent{}, f.ctx.Err()
		}
	}
	if f.i < len(f.events) {
		ev := f.events[f.i]
		f.i++
		return ev, nil
	}
	return model.ModelEvent{}, io.EOF
}
func (f *fakeStream) Usage() model.Usage {
	return model.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
}
func (f *fakeStream) Close() {}

// scriptModel 每步返回一个预置事件；脚本耗尽后返回纯文本 "idle"（无工具调用 → 循环结束）。
type scriptModel struct {
	steps []model.ModelEvent
	delay time.Duration
}

func (m *scriptModel) Stream(ctx context.Context, _ []message.Message, _ []model.ToolSpec) (model.ModelStream, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if len(m.steps) == 0 {
		return &fakeStream{events: []model.ModelEvent{{Text: "idle"}}, delay: m.delay, ctx: ctx}, nil
	}
	s := m.steps[0]
	m.steps = m.steps[1:]
	return &fakeStream{events: []model.ModelEvent{s}, delay: m.delay, ctx: ctx}, nil
}

func call(id, name, args string) model.ModelEvent {
	return model.ModelEvent{ToolCalls: []model.ToolCallDelta{{Index: 0, CallID: id, Name: name, Args: args}}}
}

func workerTools(cwd string, store *runtime.ArtifactStore) *tool.Registry {
	reg := tool.NewRegistry()
	for _, t := range tool.Builtins(runtime.NewBash(cwd), store) {
		reg.Register(t)
	}
	return reg
}

func baseOpts(m model.Model, dir string) Options {
	return Options{
		Model: m, WorkerTools: workerTools, Mode: permission.ModeYolo, SessionDir: dir, CWD: dir, MaxConcurrency: 2,
		Defs:          []SubagentSpec{{Name: "explorer", SystemPrompt: "x", MaxTurns: 10}},
		ContextWindow: 100000,
	}
}

func TestYieldTerminatesAndExtractsData(t *testing.T) {
	m := &scriptModel{steps: []model.ModelEvent{
		call("c1", "yield", `{"data":{"files":["a.go"]}}`),
		{Text: "SHOULD NOT RUN"},
	}}
	dir := t.TempDir()
	mgr := NewManager(baseOpts(m, dir))
	r := mgr.Run(context.Background(), Task{Subagent: "explorer", Prompt: "look"})
	if r.Status != StatusCompleted || !r.Yielded || r.Data["files"] == nil || r.Requests != 1 {
		t.Fatalf("result = %+v", r)
	}
	if len(m.steps) != 1 {
		t.Fatalf("model should not be called after yield; remaining steps = %d", len(m.steps))
	}
	if r.SessionFile == "" || !strings.HasPrefix(filepath.Base(r.SessionFile), "agent-explorer") {
		t.Fatalf("session file = %q", r.SessionFile)
	}
	b, _ := os.ReadFile(r.SessionFile)
	if !strings.Contains(string(b), `"type":"session_init"`) || !strings.Contains(string(b), `"session_exit"`) {
		t.Fatalf("sidecar lacks init/exit: %s", b)
	}
	if r.Usage.TotalTokens != 15 || r.DurationMs < 0 {
		t.Fatalf("usage = %+v", r.Usage)
	}
}

func TestTimeoutIsReported(t *testing.T) {
	m := &scriptModel{delay: 300 * time.Millisecond, steps: []model.ModelEvent{{Text: "slow"}}}
	o := baseOpts(m, t.TempDir())
	o.Defs[0].Timeout = 100 * time.Millisecond
	r := NewManager(o).Run(context.Background(), Task{Subagent: "explorer", Prompt: "slow"})
	if r.Status != StatusTimeout {
		t.Fatalf("status = %s, want timeout (err=%v)", StatusString(r.Status), r.Err)
	}
}

func TestParentCancelIsAborted(t *testing.T) {
	m := &scriptModel{delay: 300 * time.Millisecond, steps: []model.ModelEvent{{Text: "slow"}}}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	r := NewManager(baseOpts(m, t.TempDir())).Run(ctx, Task{Subagent: "explorer", Prompt: "x"})
	if r.Status != StatusAborted {
		t.Fatalf("status = %s, want aborted", StatusString(r.Status))
	}
}

func TestHeadlessDeniesPromptByDefault(t *testing.T) {
	m := &scriptModel{steps: []model.ModelEvent{call("c1", "bash", `{"command":"echo hi"}`), {Text: "end"}}}
	o := baseOpts(m, t.TempDir())
	o.Mode = permission.ModeAlwaysAsk
	r := NewManager(o).Run(context.Background(), Task{Subagent: "explorer", Prompt: "x"})
	b, _ := os.ReadFile(r.SessionFile)
	if !strings.Contains(string(b), "headless subagent cannot prompt") {
		t.Fatalf("bash should be denied, transcript: %s", b)
	}
	if r.Status != StatusCompleted {
		t.Fatalf("denied tool should not fail the run: %+v", r)
	}
}

type recordingApprover struct{ got []message.ToolCall }

func (r *recordingApprover) Approve(_ context.Context, c message.ToolCall) (bool, error) {
	r.got = append(r.got, c)
	return true, nil
}

func TestEscalationLabelsCall(t *testing.T) {
	m := &scriptModel{steps: []model.ModelEvent{call("c1", "bash", `{"command":"echo hi"}`), {Text: "end"}}}
	o := baseOpts(m, t.TempDir())
	o.Mode = permission.ModeAlwaysAsk
	ap := &recordingApprover{}
	o.Approver, o.Escalate = ap, true
	r := NewManager(o).Run(context.Background(), Task{Name: "Scout", Subagent: "explorer", Prompt: "x"})
	if len(ap.got) != 1 || !strings.Contains(ap.got[0].Name, "[子 agent Scout]") {
		t.Fatalf("approver got %+v", ap.got)
	}
	b, _ := os.ReadFile(r.SessionFile)
	if !strings.Contains(string(b), "hi") {
		t.Fatalf("escalated approval should let bash run: %s", b)
	}
}

func TestSchemaRequiredButNoData(t *testing.T) {
	m := &scriptModel{steps: []model.ModelEvent{{Text: "just text"}}}
	o := baseOpts(m, t.TempDir())
	o.Defs[0].OutputSchema = map[string]any{"type": "object"}
	r := NewManager(o).Run(context.Background(), Task{Subagent: "explorer", Prompt: "x"})
	if r.Status != StatusFailed || r.Err == nil || r.Text != "just text" {
		t.Fatalf("result = %+v", r)
	}
}

func TestIndependentBashPerRun(t *testing.T) {
	// 两个子 agent 各自 cd 到不同目录，再 pwd；互不影响
	dir := t.TempDir()
	dir, _ = filepath.EvalSymlinks(dir)
	_ = os.MkdirAll(filepath.Join(dir, "a"), 0o755)
	_ = os.MkdirAll(filepath.Join(dir, "b"), 0o755)
	mk := func(sub string) *scriptModel {
		return &scriptModel{steps: []model.ModelEvent{
			call("c1", "bash", `{"command":"cd `+sub+` && true"}`),
			call("c2", "bash", `{"command":"pwd"}`),
			{Text: "end"},
		}}
	}
	ra := NewManager(baseOpts(mk("a"), dir)).Run(context.Background(), Task{Subagent: "explorer", Prompt: "x"})
	rb := NewManager(baseOpts(mk("b"), dir)).Run(context.Background(), Task{Subagent: "explorer", Prompt: "x"})
	ba, _ := os.ReadFile(ra.SessionFile)
	bb, _ := os.ReadFile(rb.SessionFile)
	if !strings.Contains(string(ba), filepath.Join(dir, "a")) || !strings.Contains(string(bb), filepath.Join(dir, "b")) {
		t.Fatalf("cwd leaked between runs:\nA: %s\nB: %s", ba, bb)
	}
}

func TestRunManyOrderAndUnknown(t *testing.T) {
	mgr := NewManager(baseOpts(&scriptModel{}, t.TempDir()))
	rs := mgr.RunMany(context.Background(), []Task{{Subagent: "nope", Prompt: "x"}, {Subagent: "explorer", Prompt: "y"}})
	if len(rs) != 2 || rs[0].Status != StatusFailed || rs[1].Status != StatusCompleted || rs[1].Yielded {
		t.Fatalf("results = %+v", rs)
	}
	if rs[1].Name != "explorer-2" {
		t.Fatalf("default name = %q", rs[1].Name)
	}
	out := renderResult(rs[1])
	if !strings.Contains(out, "[未显式 yield") || !strings.Contains(out, "transcript:") {
		t.Fatalf("render = %s", out)
	}
}

func TestMemorySidecarWhenNoSessionDir(t *testing.T) {
	o := baseOpts(&scriptModel{}, t.TempDir())
	o.SessionDir = ""
	r := NewManager(o).Run(context.Background(), Task{Subagent: "explorer", Prompt: "x"})
	if r.Status != StatusCompleted || r.SessionFile != "" {
		t.Fatalf("result = %+v", r)
	}
}
