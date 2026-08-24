package subagent

import (
	"context"
	"strings"
	"testing"
	"time"

	"einoclaw-build/internal/model"
	"einoclaw-build/internal/runtime"
	"einoclaw-build/internal/tool"
)

// execHub 调一次 hub 工具，返回给模型的文本与错误。
func execHub(t *testing.T, h tool.Tool, args map[string]any) (string, error) {
	t.Helper()
	sink := runtime.NewSink(4000, 4000)
	defer sink.Close()
	err := h.Execute(context.Background(), args, sink)
	return sink.Result(), err
}

func TestHubListShowsPeersNotSelf(t *testing.T) {
	mgr := NewManager(baseOpts(&scriptModel{}, t.TempDir()))
	a, b := newRun("Scout", "explorer", 1), newRun("Fixer", "worker", 1)
	mgr.register(a)
	mgr.register(b)
	a.setStatus(StatusRunning)

	out, err := execHub(t, NewHubTool(mgr, "Scout"), map[string]any{"op": "list"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "- Scout") || !strings.Contains(out, "Fixer") || !strings.Contains(out, "Main") {
		t.Fatalf("名册 = %s（不该含自己，应含 Main 与其它 peer）", out)
	}
}

func TestHubSendAndInbox(t *testing.T) {
	mgr := NewManager(baseOpts(&scriptModel{}, t.TempDir()))
	scout := newRun("Scout", "explorer", 1)
	mgr.register(scout)
	scout.setStatus(StatusRunning)

	// 子 agent → Main：进主信箱
	out, err := execHub(t, NewHubTool(mgr, "Scout"), map[string]any{
		"op": "send", "to": MainName, "text": "接口定为 Add(a,b int) int"})
	if err != nil || !strings.Contains(out, "已送达 Main") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	// Main → 运行中的子 agent：进它的 steering 队列
	out, err = execHub(t, NewHubTool(mgr, MainName), map[string]any{
		"op": "send", "to": "Scout", "text": "按这个签名来"})
	if err != nil || !strings.Contains(out, "已送达 Scout") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	select {
	case msg := <-scout.steer:
		if !strings.Contains(messageText(msg), "按这个签名来") {
			t.Fatalf("steer = %q", messageText(msg))
		}
	default:
		t.Fatal("没进 steer 队列")
	}
	// Main 收件箱
	out, err = execHub(t, NewHubTool(mgr, MainName), map[string]any{"op": "inbox"})
	if err != nil || !strings.Contains(out, "来自 Scout") {
		t.Fatalf("inbox = %q err=%v", out, err)
	}
	if out, _ := execHub(t, NewHubTool(mgr, MainName), map[string]any{"op": "inbox"}); !strings.Contains(out, "为空") {
		t.Fatalf("收件应是一次性的：%s", out)
	}
}

func TestHubSendRequiresArgsAndKnownPeer(t *testing.T) {
	mgr := NewManager(baseOpts(&scriptModel{}, t.TempDir()))
	h := NewHubTool(mgr, MainName)
	if _, err := execHub(t, h, map[string]any{"op": "send", "to": "Scout"}); err == nil {
		t.Fatal("缺 text 应报错")
	}
	if _, err := execHub(t, h, map[string]any{"op": "send", "to": "Nope", "text": "hi"}); err == nil ||
		!strings.Contains(err.Error(), "没有名为") {
		t.Fatalf("未知 peer 的错误 = %v", err)
	}
	if _, err := execHub(t, h, map[string]any{"op": "nope"}); err == nil || !strings.Contains(err.Error(), "未知 op") {
		t.Fatalf("未知 op 的错误 = %v", err)
	}
}

func TestHubWaitWokenByMessage(t *testing.T) {
	mgr := NewManager(baseOpts(&scriptModel{}, t.TempDir()))
	h := NewHubTool(mgr, MainName)
	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _ = mgr.Deliver("Scout", MainName, "我做完了", "")
	}()
	start := time.Now()
	out, err := execHub(t, h, map[string]any{"op": "wait", "timeout": float64(5)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "我做完了") {
		t.Fatalf("wait 应被消息唤醒：%s", out)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("被唤醒后应立刻返回")
	}
}

func TestHubWaitTimesOut(t *testing.T) {
	mgr := NewManager(baseOpts(&scriptModel{}, t.TempDir()))
	out, err := execHub(t, NewHubTool(mgr, MainName), map[string]any{"op": "wait", "timeout": float64(1)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "没有新消息") {
		t.Fatalf("超时文本 = %s", out)
	}
}

func TestHubWaitWokenBySettledJob(t *testing.T) {
	mgr := NewManager(bgOpts(t, &scriptModel{delay: 80 * time.Millisecond,
		steps: []model.ModelEvent{call("c1", "yield", `{"data":{"ok":true}}`)}}))
	_, jobs, err := mgr.StartBackground(context.Background(), one("explorer", "x"), mgr.Env(0, "", nil))
	if err != nil {
		t.Fatal(err)
	}
	out, err := execHub(t, NewHubTool(mgr, MainName), map[string]any{
		"op": "wait", "ids": []any{jobs[0].ID}, "timeout": float64(5)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "作业已结束") || !strings.Contains(out, jobs[0].ID) {
		t.Fatalf("wait 应被作业结束唤醒：%s", out)
	}
	if got := mgr.TakeSettled(); len(got) != 0 {
		t.Fatalf("wait 已消费投递，不该再有 async-result：%+v", got)
	}
}

func TestHubWaitKeepsUnwatchedJobsForDelivery(t *testing.T) {
	mgr := NewManager(bgOpts(t, &scriptModel{steps: []model.ModelEvent{call("c1", "yield", `{"data":{"ok":true}}`)}}))
	_, _, err := mgr.StartBackground(context.Background(), one("explorer", "x"), mgr.Env(0, "", nil))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return mgr.Pending() == 0 })
	// 只关注一个不存在的作业：已结束的那个应留在待投递队列里
	out, err := execHub(t, NewHubTool(mgr, MainName), map[string]any{
		"op": "wait", "ids": []any{"OtherJob"}, "timeout": float64(1)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "没有新消息") {
		t.Fatalf("不关注的作业不该唤醒 wait：%s", out)
	}
	if got := mgr.TakeSettled(); len(got) != 1 {
		t.Fatalf("不关注的作业结果应留给正常投递：%+v", got)
	}
}

func TestHubJobsAndCancel(t *testing.T) {
	mgr := NewManager(bgOpts(t, &scriptModel{delay: 500 * time.Millisecond, steps: []model.ModelEvent{{Text: "慢"}}}))
	_, jobs, err := mgr.StartBackground(context.Background(), one("explorer", "x"), mgr.Env(0, "", nil))
	if err != nil {
		t.Fatal(err)
	}
	h := NewHubTool(mgr, MainName)
	out, err := execHub(t, h, map[string]any{"op": "jobs"})
	if err != nil || !strings.Contains(out, jobs[0].ID) {
		t.Fatalf("jobs = %q err=%v", out, err)
	}
	out, err = execHub(t, h, map[string]any{"op": "cancel", "ids": []any{jobs[0].ID}})
	if err != nil || !strings.Contains(out, "已取消 1") {
		t.Fatalf("cancel = %q err=%v", out, err)
	}
	if _, err := execHub(t, h, map[string]any{"op": "cancel"}); err == nil {
		t.Fatal("cancel 缺 ids 应报错")
	}
	waitFor(t, 3*time.Second, func() bool { return mgr.Pending() == 0 })
}

func TestHubToolInSubagentToolset(t *testing.T) {
	o := baseOpts(&scriptModel{}, t.TempDir())
	m := o.Model.(*scriptModel)
	runOne(t, NewManager(o), context.Background(), one("explorer", "x"))
	found := false
	for _, n := range m.toolsAt(0) {
		if n == "hub" {
			found = true
		}
	}
	if !found {
		t.Fatalf("子 agent 应带 hub 工具：%v", m.toolsAt(0))
	}
}
