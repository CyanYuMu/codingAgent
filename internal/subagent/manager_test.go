package subagent

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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
// 同时记录每次调用收到的工具名，供"强制收尾那一 turn 只有 yield"这类断言使用。
type scriptModel struct {
	mu    sync.Mutex
	steps []model.ModelEvent
	delay time.Duration
	tools [][]string // 每次调用收到的工具名（排序后）
	calls int
}

func (m *scriptModel) Stream(ctx context.Context, _ []message.Message, tools []model.ToolSpec) (model.ModelStream, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	m.mu.Lock()
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	m.tools = append(m.tools, names)
	m.calls++
	var ev model.ModelEvent
	if len(m.steps) == 0 {
		ev = model.ModelEvent{Text: "idle"}
	} else {
		ev, m.steps = m.steps[0], m.steps[1:]
	}
	delay := m.delay
	m.mu.Unlock()
	return &fakeStream{events: []model.ModelEvent{ev}, delay: delay, ctx: ctx}, nil
}

func (m *scriptModel) remaining() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.steps)
}

func (m *scriptModel) toolsAt(i int) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i < 0 || i >= len(m.tools) {
		return nil
	}
	return m.tools[i]
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
		Defs:          []AgentDef{{Name: "explorer", Description: "探索", SystemPrompt: "x", MaxTurns: 10}},
		ContextWindow: 100000,
		MinTaskChars:  1, // 测试里用短任务描述；预检长度另有专门测试
	}
}

// one 构造单任务批次。
func one(agent, task string) TaskBatch {
	return TaskBatch{Context: "测试批次背景", Tasks: []TaskItem{{Agent: agent, Task: task}}}
}

// runOne 跑一个单任务批次并返回唯一结果。
func runOne(t *testing.T, mgr *Manager, ctx context.Context, b TaskBatch) Result {
	t.Helper()
	rs, err := mgr.RunBatch(ctx, b, mgr.Env(0, "", nil))
	if err != nil {
		t.Fatalf("RunBatch: %v", err)
	}
	if len(rs) != 1 {
		t.Fatalf("want 1 result, got %d", len(rs))
	}
	return rs[0]
}

func TestYieldTerminatesAndExtractsData(t *testing.T) {
	m := &scriptModel{steps: []model.ModelEvent{
		call("c1", "yield", `{"data":{"files":["a.go"]}}`),
		{Text: "SHOULD NOT RUN"},
	}}
	dir := t.TempDir()
	mgr := NewManager(baseOpts(m, dir))
	r := runOne(t, mgr, context.Background(), one("explorer", "look"))
	data, _ := r.Data.(map[string]any)
	if r.Status != StatusCompleted || !r.Yielded || data["files"] == nil || r.Requests != 1 {
		t.Fatalf("result = %+v", r)
	}
	if m.remaining() != 1 {
		t.Fatalf("model should not be called after yield; remaining steps = %d", m.remaining())
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
	r := runOne(t, NewManager(o), context.Background(), one("explorer", "slow"))
	if r.Status != StatusTimeout {
		t.Fatalf("status = %s, want timeout (err=%v)", StatusString(r.Status), r.Err)
	}
}

func TestParentCancelIsAborted(t *testing.T) {
	m := &scriptModel{delay: 300 * time.Millisecond, steps: []model.ModelEvent{{Text: "slow"}}}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	r := runOne(t, NewManager(baseOpts(m, t.TempDir())), ctx, one("explorer", "x"))
	if r.Status != StatusAborted {
		t.Fatalf("status = %s, want aborted", StatusString(r.Status))
	}
}

func TestHeadlessDeniesPromptByDefault(t *testing.T) {
	m := &scriptModel{steps: []model.ModelEvent{call("c1", "bash", `{"command":"echo hi"}`), {Text: "end"}}}
	o := baseOpts(m, t.TempDir())
	o.Mode = permission.ModeAlwaysAsk
	r := runOne(t, NewManager(o), context.Background(), one("explorer", "x"))
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
	b := one("explorer", "x")
	b.Tasks[0].Name = "Scout"
	r := runOne(t, NewManager(o), context.Background(), b)
	if len(ap.got) != 1 || !strings.Contains(ap.got[0].Name, "[子 agent Scout]") {
		t.Fatalf("approver got %+v", ap.got)
	}
	tr, _ := os.ReadFile(r.SessionFile)
	if !strings.Contains(string(tr), "hi") {
		t.Fatalf("escalated approval should let bash run: %s", tr)
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
	ra := runOne(t, NewManager(baseOpts(mk("a"), dir)), context.Background(), one("explorer", "x"))
	rb := runOne(t, NewManager(baseOpts(mk("b"), dir)), context.Background(), one("explorer", "x"))
	ba, _ := os.ReadFile(ra.SessionFile)
	bb, _ := os.ReadFile(rb.SessionFile)
	if !strings.Contains(string(ba), filepath.Join(dir, "a")) || !strings.Contains(string(bb), filepath.Join(dir, "b")) {
		t.Fatalf("cwd leaked between runs:\nA: %s\nB: %s", ba, bb)
	}
}

func TestRunBatchOrderAndDefaultNames(t *testing.T) {
	mgr := NewManager(baseOpts(&scriptModel{}, t.TempDir()))
	rs, err := mgr.RunBatch(context.Background(), TaskBatch{
		Context: "背景",
		Tasks:   []TaskItem{{Agent: "explorer", Task: "第一件事"}, {Agent: "explorer", Task: "第二件事"}},
	}, mgr.Env(0, "", nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || rs[0].Status != StatusCompleted || rs[1].Status != StatusCompleted || rs[0].Yielded {
		t.Fatalf("results = %+v", rs)
	}
	if rs[0].Name != "explorer-1" || rs[1].Name != "explorer-2" {
		t.Fatalf("names = %q %q", rs[0].Name, rs[1].Name)
	}
	out := renderResult(rs[0])
	if !strings.Contains(out, "[未显式 yield") || !strings.Contains(out, "转录: history://") {
		t.Fatalf("render = %s", out)
	}
}

func TestRunBatchUnknownAgentRejectedBeforeSpawning(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(baseOpts(&scriptModel{}, dir))
	_, err := mgr.RunBatch(context.Background(), one("nope", "做点什么"), mgr.Env(0, "", nil))
	if err == nil || !strings.Contains(err.Error(), "未知 agent") {
		t.Fatalf("err = %v", err)
	}
	des, _ := os.ReadDir(dir)
	if len(des) != 0 {
		t.Fatalf("预检失败不该产生 sidecar：%v", des)
	}
}

func TestMemorySidecarWhenNoSessionDir(t *testing.T) {
	o := baseOpts(&scriptModel{}, t.TempDir())
	o.SessionDir = ""
	r := runOne(t, NewManager(o), context.Background(), one("explorer", "x"))
	if r.Status != StatusCompleted || r.SessionFile != "" {
		t.Fatalf("result = %+v", r)
	}
}

func TestToolSetReadOnly(t *testing.T) {
	o := baseOpts(&scriptModel{}, t.TempDir())
	o.Defs[0].ReadOnly = true
	m := o.Model.(*scriptModel)
	runOne(t, NewManager(o), context.Background(), one("explorer", "x"))
	got := m.toolsAt(0)
	want := []string{"glob", "grep", "read_file", "yield"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("read-only 工具集 = %v, want %v", got, want)
	}
}

func TestToolSetSpawnsAndDepth(t *testing.T) {
	for _, tc := range []struct {
		name     string
		spawns   []string
		maxDepth int
		wantTask bool
	}{
		{"有 spawns 且深度未满", []string{"worker"}, 2, true},
		{"无 spawns", nil, 2, false},
		{"深度已满", []string{"worker"}, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := baseOpts(&scriptModel{}, t.TempDir())
			o.Defs[0].Spawns = tc.spawns
			o.MaxDepth = tc.maxDepth
			o.Defs = append(o.Defs, AgentDef{Name: "worker", Description: "干活", SystemPrompt: "w"})
			m := o.Model.(*scriptModel)
			runOne(t, NewManager(o), context.Background(), one("explorer", "x"))
			hasTask := false
			for _, n := range m.toolsAt(0) {
				if n == "task" {
					hasTask = true
				}
			}
			if hasTask != tc.wantTask {
				t.Fatalf("task in toolset = %v, want %v（工具集 %v）", hasTask, tc.wantTask, m.toolsAt(0))
			}
		})
	}
}

func TestResolveDefAppliesDefaultsAndBudgetCeiling(t *testing.T) {
	mgr := NewManager(Options{DefaultMaxTurns: 40, DefaultTimeout: time.Minute, SoftBudget: 200})
	got := mgr.resolveDef(AgentDef{Name: "a"})
	if got.MaxTurns != 40 || got.Timeout != time.Minute || got.SoftBudget != 200 {
		t.Fatalf("defaults = %+v", got)
	}
	if got := mgr.resolveDef(AgentDef{Name: "a", SoftBudget: 500}); got.SoftBudget != 200 {
		t.Fatalf("定义不该放大全局预算上限，got %d", got.SoftBudget)
	}
	if got := mgr.resolveDef(AgentDef{Name: "a", SoftBudget: 50}); got.SoftBudget != 50 {
		t.Fatalf("定义可以更小，got %d", got.SoftBudget)
	}
	if got := mgr.resolveDef(AgentDef{Name: "a", SoftBudget: -1}); got.SoftBudget != 0 {
		t.Fatalf("-1 应表示关闭护栏，got %d", got.SoftBudget)
	}
}
