package subagent

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"einoclaw-build/internal/bus"
	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
)

// toolAwareModel 模拟一个「不肯收尾」的子 agent：只要还能调别的工具就一直调 glob；
// 工具集被裁到只剩 yield 时才（可选地）提交结果。用来测强制收尾这条路径。
type toolAwareModel struct {
	mu              sync.Mutex
	yieldWhenForced bool
	calls           int
}

func (m *toolAwareModel) Stream(ctx context.Context, _ []message.Message, tools []model.ToolSpec) (model.ModelStream, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	onlyYield := len(tools) == 1 && tools[0].Name == "yield"
	m.mu.Lock()
	m.calls++
	yield := onlyYield && m.yieldWhenForced
	m.mu.Unlock()

	var ev model.ModelEvent
	switch {
	case yield:
		ev = call("cy", "yield", `{"data":{"done":true}}`)
	case onlyYield:
		ev = model.ModelEvent{Text: "我再想想"} // 拒绝收尾
	default:
		ev = call("cg", "glob", `{"pattern":"*.go"}`)
	}
	return &fakeStream{events: []model.ModelEvent{ev}, ctx: ctx}, nil
}

func sidecar(t *testing.T, r Result) string {
	t.Helper()
	b, err := os.ReadFile(r.SessionFile)
	if err != nil {
		t.Fatalf("读 sidecar: %v", err)
	}
	return string(b)
}

func TestLadderRemindsThreeTimesThenStops(t *testing.T) {
	m := &scriptModel{} // 永远只回文本，从不调 yield
	r := runOne(t, NewManager(baseOpts(m, t.TempDir())), context.Background(), one("explorer", "x"))
	if r.Reminders != maxYieldReminders {
		t.Fatalf("reminders = %d, want %d", r.Reminders, maxYieldReminders)
	}
	if r.Requests != maxYieldReminders+1 {
		t.Fatalf("requests = %d，应为首轮 + 3 次提醒", r.Requests)
	}
	tr := sidecar(t, r)
	for _, want := range []string{"[提醒 1/3]", "[提醒 2/3]", "[提醒 3/3]"} {
		if !strings.Contains(tr, want) {
			t.Fatalf("转录里缺 %s", want)
		}
	}
	if r.Status != StatusCompleted || r.Yielded || !strings.Contains(r.Warning, "未 yield") {
		t.Fatalf("无 schema 时应 completed 但标注未 yield：%+v", r)
	}
}

func TestForcedTurnOnlyExposesYield(t *testing.T) {
	m := &scriptModel{}
	runOne(t, NewManager(baseOpts(m, t.TempDir())), context.Background(), one("explorer", "x"))
	// 第 4 次模型调用 = 第 3 次提醒之后的那一轮：工具集只剩 yield
	if got := m.toolsAt(3); strings.Join(got, ",") != "yield" {
		t.Fatalf("强制收尾那轮的工具集 = %v，want [yield]", got)
	}
	if got := m.toolsAt(0); len(got) <= 1 {
		t.Fatalf("正常轮次不该被裁：%v", got)
	}
}

func TestLadderExhaustedWithSchemaFails(t *testing.T) {
	o := baseOpts(&scriptModel{}, t.TempDir())
	o.Defs[0].OutputSchema = map[string]any{"type": "object"}
	r := runOne(t, NewManager(o), context.Background(), one("explorer", "x"))
	if r.Status != StatusFailed || r.Err == nil || !strings.Contains(r.Err.Error(), "仍未通过 yield") {
		t.Fatalf("result = %+v", r)
	}
	if r.Text == "" {
		t.Fatal("失败也要把最后文本带回去")
	}
}

func TestSoftBudgetNoticeThenForcedYield(t *testing.T) {
	o := baseOpts(&toolAwareModel{yieldWhenForced: true}, t.TempDir())
	o.Defs[0].SoftBudget = 4 // 停机线 = 6
	o.Defs[0].MaxTurns = 50
	r := runOne(t, NewManager(o), context.Background(), one("explorer", "x"))
	tr := sidecar(t, r)
	if !strings.Contains(tr, "[预算提醒]") {
		t.Fatalf("越过软预算应注入收尾通知：\n%s", tr)
	}
	if !strings.Contains(tr, "[强制收尾]") {
		t.Fatalf("到停机线应注入强制收尾通知：\n%s", tr)
	}
	if r.Status != StatusCompleted || !r.Yielded || !r.BudgetStopped {
		t.Fatalf("强制收尾后 yield 应算 completed：%+v", r)
	}
	if !strings.Contains(r.Warning, "强制收尾") {
		t.Fatalf("应提示结果可能不完整：%q", r.Warning)
	}
}

func TestBudgetStopWithoutYieldIsKilled(t *testing.T) {
	o := baseOpts(&toolAwareModel{yieldWhenForced: false}, t.TempDir())
	o.Defs[0].SoftBudget = 4
	o.Defs[0].MaxTurns = 50
	r := runOne(t, NewManager(o), context.Background(), one("explorer", "x"))
	if r.Status != StatusKilled || r.Err == nil {
		t.Fatalf("强制收尾后仍不 yield 应被终止：%+v", r)
	}
	if r.Text == "" {
		t.Fatal("被终止也要保留最后文本")
	}
}

func TestBudgetOffMeansNoNotice(t *testing.T) {
	o := baseOpts(&scriptModel{}, t.TempDir())
	o.Defs[0].SoftBudget = -1 // 显式关闭
	r := runOne(t, NewManager(o), context.Background(), one("explorer", "x"))
	if strings.Contains(sidecar(t, r), "[预算提醒]") {
		t.Fatal("关闭护栏后不该有预算提醒")
	}
}

func TestYieldErrorMakesRunFailed(t *testing.T) {
	m := &scriptModel{steps: []model.ModelEvent{call("c1", "yield", `{"error":"缺少依赖接口"}`)}}
	r := runOne(t, NewManager(baseOpts(m, t.TempDir())), context.Background(), one("explorer", "x"))
	if r.Status != StatusFailed || r.Err == nil || r.Err.Error() != "缺少依赖接口" {
		t.Fatalf("result = %+v", r)
	}
	if r.Reminders != 0 {
		t.Fatalf("error yield 是终止提交，不该再提醒：%d", r.Reminders)
	}
}

func TestIncrementalThenFinalYield(t *testing.T) {
	o := baseOpts(&scriptModel{steps: []model.ModelEvent{
		call("c1", "yield", `{"section":"findings","data":{"file":"a.go","severity":"low"}}`),
		call("c2", "yield", `{"section":"verdict","data":"就一个小问题"}`),
		call("c3", "yield", `{}`),
	}}, t.TempDir())
	o.Defs[0].OutputSchema = findingsSchema
	r := runOne(t, NewManager(o), context.Background(), one("explorer", "x"))
	if r.Status != StatusCompleted || !r.Yielded {
		t.Fatalf("result = %+v", r)
	}
	if len(r.Sections["findings"]) != 1 || r.Data == nil {
		t.Fatalf("分段应保留并装配：sections=%v data=%v", r.Sections, r.Data)
	}
	if r.Requests != 3 {
		t.Fatalf("增量提交不该终止运行：requests = %d", r.Requests)
	}
}

func TestOutputFileWritten(t *testing.T) {
	m := &scriptModel{steps: []model.ModelEvent{call("c1", "yield", `{"data":{"files":["a.go"]}}`)}}
	r := runOne(t, NewManager(baseOpts(m, t.TempDir())), context.Background(), one("explorer", "x"))
	if r.OutputFile == "" {
		t.Fatal("完整产出应落盘")
	}
	b, err := os.ReadFile(r.OutputFile)
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	if !strings.Contains(out, "## data") || !strings.Contains(out, "a.go") || !strings.Contains(out, "status: completed") {
		t.Fatalf("产出文件内容 = %s", out)
	}
}

func TestLifecycleAndProgressPublished(t *testing.T) {
	b := bus.New()
	life, cancelLife := b.Subscribe(ChLifecycle, 64)
	defer cancelLife()
	prog, cancelProg := b.Subscribe(ChProgress, 256)
	defer cancelProg()

	o := baseOpts(&scriptModel{steps: []model.ModelEvent{
		call("c1", "glob", `{"pattern":"*.go"}`),
		call("c2", "yield", `{"data":{"ok":true}}`),
	}}, t.TempDir())
	o.Bus = b
	runOne(t, NewManager(o), context.Background(), one("explorer", "x"))
	cancelLife()
	cancelProg()

	var statuses []string
	for env := range life {
		statuses = append(statuses, env.Payload.(Lifecycle).Status)
	}
	joined := strings.Join(statuses, ",")
	if !strings.HasPrefix(joined, "running") || !strings.HasSuffix(joined, "parked") {
		t.Fatalf("生命周期 = %s（应 running 开头、parked 结尾）", joined)
	}
	sawTool, sawRequests := false, false
	for env := range prog {
		p := env.Payload.(Progress)
		if p.CurrentTool == "glob" {
			sawTool = true
		}
		if p.Requests > 0 && p.Tokens > 0 {
			sawRequests = true
		}
	}
	if !sawTool || !sawRequests {
		t.Fatalf("进度事件缺内容：tool=%v requests=%v", sawTool, sawRequests)
	}
}

func TestCancelRunFromRoster(t *testing.T) {
	o := baseOpts(&scriptModel{delay: 500 * time.Millisecond}, t.TempDir())
	o.Defs[0].Timeout = 5 * time.Second
	mgr := NewManager(o)
	run := newRun("Scout", "explorer", 1)
	rs, err := mgr.setup(mgr.resolveDef(o.Defs[0]), Resolved{Item: TaskItem{Name: "Scout", Agent: "explorer", Task: "x"}}, "背景", 1, run)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.sess.Close()
	go func() { time.Sleep(80 * time.Millisecond); run.Cancel() }()
	r := mgr.drive(context.Background(), run, rs)
	if r.Status != StatusKilled {
		t.Fatalf("人工取消应标 killed：%+v", r)
	}
}
