package subagent

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"einoclaw-build/internal/model"
	"einoclaw-build/internal/runtime"
)

// bgOpts 后台作业测试用的装配：慢一点的模型，方便断言「立刻返回」。
func bgOpts(t *testing.T, m model.Model) Options {
	o := baseOpts(m, t.TempDir())
	o.AllowBackground = true
	return o
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待 %s 后条件仍不满足", d)
}

func TestStartBackgroundReturnsImmediately(t *testing.T) {
	mgr := NewManager(bgOpts(t, &scriptModel{delay: 200 * time.Millisecond,
		steps: []model.ModelEvent{call("c1", "yield", `{"data":{"ok":true}}`)}}))
	start := time.Now()
	inline, jobs, err := mgr.StartBackground(context.Background(), one("explorer", "慢慢做"), mgr.Env(0, "", nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || len(inline) != 0 {
		t.Fatalf("jobs=%+v inline=%+v", jobs, inline)
	}
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Fatalf("StartBackground 阻塞了 %s，应立刻返回", took)
	}
	if mgr.Pending() != 1 {
		t.Fatalf("Pending = %d", mgr.Pending())
	}
	waitFor(t, 3*time.Second, func() bool { return mgr.Pending() == 0 })
}

func TestBackgroundOutlivesCallerContext(t *testing.T) {
	mgr := NewManager(bgOpts(t, &scriptModel{delay: 150 * time.Millisecond,
		steps: []model.ModelEvent{call("c1", "yield", `{"data":{"ok":true}}`)}}))
	ctx, cancel := context.WithCancel(context.Background())
	_, jobs, err := mgr.StartBackground(ctx, one("explorer", "x"), mgr.Env(0, "", nil))
	if err != nil {
		t.Fatal(err)
	}
	cancel() // 父 turn 结束：后台作业不该被连带取消
	waitFor(t, 3*time.Second, func() bool { return mgr.Pending() == 0 })
	settled := mgr.TakeSettled()
	if len(settled) != 1 || settled[0].JobID != jobs[0].ID || settled[0].Result.Status != StatusCompleted {
		t.Fatalf("settled = %+v", settled)
	}
}

func TestSettledDeliveredExactlyOnce(t *testing.T) {
	mgr := NewManager(bgOpts(t, &scriptModel{steps: []model.ModelEvent{call("c1", "yield", `{"data":{"ok":true}}`)}}))
	_, _, err := mgr.StartBackground(context.Background(), one("explorer", "x"), mgr.Env(0, "", nil))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return len(mgr.pendingSnapshot()) == 1 })
	if got := mgr.TakeSettled(); len(got) != 1 {
		t.Fatalf("第一次取件 = %d 条", len(got))
	}
	if got := mgr.TakeSettled(); len(got) != 0 {
		t.Fatalf("第二次取件应为空，实际 %d 条", len(got))
	}
}

func TestSettledDeliveryIsScopedAndAckedAfterPersistence(t *testing.T) {
	o := bgOpts(t, &scriptModel{steps: []model.ModelEvent{call("c1", "yield", `{"data":{"ok":true}}`)}})
	o.SessionID = "session-a"
	dirA := o.SessionDir
	dirB := t.TempDir()
	o.ArtifactStore = runtime.NewArtifactStore(dirA)
	mgr := NewManager(o)

	env := mgr.Env(0, "", nil)
	env.Owner = "ParentRun"
	_, _, err := mgr.StartBackground(context.Background(), one("explorer", "x"), env)
	if err != nil {
		t.Fatal(err)
	}
	mgr.SetMainSession("session-b", dirB)
	if got := o.ArtifactStore.Dir(); got != dirB {
		t.Fatalf("main artifact store still points at old session: %q", got)
	}
	waitFor(t, 3*time.Second, func() bool { return mgr.Pending() == 0 })

	if got := mgr.PeekSettled(MainName, "session-a"); len(got) != 0 {
		t.Fatalf("wrong owner saw delivery: %+v", got)
	}
	if got := mgr.PeekSettled("ParentRun", "session-b"); len(got) != 0 {
		t.Fatalf("wrong session saw delivery: %+v", got)
	}
	first := mgr.PeekSettled("ParentRun", "session-a")
	second := mgr.PeekSettled("ParentRun", "session-a")
	if len(first) != 1 || len(second) != 1 || first[0].DeliveryID == "" || first[0].DeliveryID != second[0].DeliveryID {
		t.Fatalf("delivery should remain stable before ack: first=%+v second=%+v", first, second)
	}
	if !strings.HasPrefix(first[0].Result.OutputFile, dirA) {
		t.Fatalf("old-session job wrote into switched session: %q, want prefix %q", first[0].Result.OutputFile, dirA)
	}
	mgr.AckSettled([]string{first[0].DeliveryID})
	if got := mgr.PeekSettled("ParentRun", "session-a"); len(got) != 0 {
		t.Fatalf("acked delivery remained pending: %+v", got)
	}
}

func TestNestedBackgroundResultRevivesItsParkedOwner(t *testing.T) {
	m := &scriptModel{steps: []model.ModelEvent{
		call("parent-first", "yield", `{"data":{"round":1}}`),
		call("child-done", "yield", `{"data":{"child":"done"}}`),
		call("parent-resumed", "yield", `{"data":{"integrated":true}}`),
	}}
	o := bgOpts(t, m)
	o.SessionID = "session-a"
	mgr := NewManager(o)

	parentBatch := one("explorer", "start parent")
	parentBatch.Tasks[0].Name = "ParentRun"
	if got := runOne(t, mgr, context.Background(), parentBatch); got.Status != StatusCompleted {
		t.Fatalf("parent first run = %+v", got)
	}
	env := mgr.Env(1, "parent-agent", []string{"explorer"})
	env.Owner = "ParentRun"
	childBatch := one("explorer", "background child")
	childBatch.Tasks[0].Name = "ChildRun"
	if _, _, err := mgr.StartBackground(context.Background(), childBatch, env); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		return len(mgr.PeekSettled(MainName, "session-a")) == 1
	})

	if got := mgr.PeekSettled("ParentRun", "session-a"); len(got) != 0 {
		t.Fatalf("child result was not acknowledged after reviving parent: %+v", got)
	}
	mainResults := mgr.PeekSettled(MainName, "session-a")
	if len(mainResults) != 1 || mainResults[0].JobID != "ParentRun" || mainResults[0].Result.Status != StatusCompleted {
		t.Fatalf("resumed parent result was not routed to Main: %+v", mainResults)
	}
	transcript, err := os.ReadFile(mainResults[0].Result.SessionFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(transcript), "后台作业 ChildRun 已完成") {
		t.Fatalf("parent transcript lacks child delivery: %s", transcript)
	}
}

func TestJobsSnapshotConsumesDelivery(t *testing.T) {
	mgr := NewManager(bgOpts(t, &scriptModel{steps: []model.ModelEvent{call("c1", "yield", `{"data":{"ok":true}}`)}}))
	_, _, _ = mgr.StartBackground(context.Background(), one("explorer", "x"), mgr.Env(0, "", nil))
	waitFor(t, 3*time.Second, func() bool { return mgr.Pending() == 0 })

	jobs := mgr.Jobs()
	if len(jobs) != 1 || jobs[0].Summary == "" || !strings.Contains(jobs[0].Summary, "completed") {
		t.Fatalf("jobs 快照应带结果：%+v", jobs)
	}
	if got := mgr.TakeSettled(); len(got) != 0 {
		t.Fatalf("jobs 已消费投递，不该再有 async-result：%+v", got)
	}
}

func TestBlockingAgentStaysInline(t *testing.T) {
	o := bgOpts(t, &scriptModel{steps: []model.ModelEvent{call("c1", "yield", `{"data":{"ok":true}}`)}})
	o.Defs = append(o.Defs, AgentDef{Name: "checker", Description: "验收", SystemPrompt: "c", Blocking: true, MaxTurns: 5})
	mgr := NewManager(o)
	inline, jobs, err := mgr.StartBackground(context.Background(), TaskBatch{
		Context: "背景", Background: true,
		Tasks: []TaskItem{{Agent: "explorer", Task: "后台做"}, {Agent: "checker", Task: "当场验"}},
	}, mgr.Env(0, "", nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Agent != "explorer" {
		t.Fatalf("jobs = %+v", jobs)
	}
	if len(inline) != 1 || inline[0].Agent != "checker" || !inline[0].Status.Settled() {
		t.Fatalf("blocking agent 应当场跑完：%+v", inline)
	}
	waitFor(t, 3*time.Second, func() bool { return mgr.Pending() == 0 })
}

func TestCancelBackgroundJob(t *testing.T) {
	mgr := NewManager(bgOpts(t, &scriptModel{delay: time.Second, steps: []model.ModelEvent{{Text: "慢"}}}))
	_, jobs, err := mgr.StartBackground(context.Background(), one("explorer", "x"), mgr.Env(0, "", nil))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return mgr.lookup(jobs[0].ID).statusNow() == StatusRunning })
	if n := mgr.Cancel([]string{jobs[0].ID}); n != 1 {
		t.Fatalf("取消数 = %d", n)
	}
	waitFor(t, 3*time.Second, func() bool { return mgr.Pending() == 0 })
	settled := mgr.TakeSettled()
	if len(settled) != 1 || settled[0].Result.Status != StatusKilled {
		t.Fatalf("取消后的结果 = %+v", settled)
	}
}

func TestShutdownWaitsForBackground(t *testing.T) {
	mgr := NewManager(bgOpts(t, &scriptModel{delay: 100 * time.Millisecond, steps: []model.ModelEvent{{Text: "x"}}}))
	if _, _, err := mgr.StartBackground(context.Background(), one("explorer", "x"), mgr.Env(0, "", nil)); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Shutdown(5 * time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if mgr.Pending() != 0 {
		t.Fatalf("Shutdown 后仍有 %d 个作业在跑", mgr.Pending())
	}
}

func TestReviveParkedRun(t *testing.T) {
	m := &scriptModel{steps: []model.ModelEvent{call("c1", "yield", `{"data":{"round":1}}`)}}
	o := bgOpts(t, m)
	mgr := NewManager(o)
	b := one("explorer", "第一轮")
	b.Tasks[0].Name = "Scout"
	first := runOne(t, mgr, context.Background(), b)
	if first.Status != StatusCompleted {
		t.Fatalf("首轮 = %+v", first)
	}

	// 追问：已 parked 的 Run 被唤醒续跑，结果按后台作业投递
	m.mu.Lock()
	m.steps = []model.ModelEvent{call("c2", "yield", `{"data":{"round":2}}`)}
	m.mu.Unlock()
	receipt, err := mgr.Deliver(MainName, "Scout", "再看一眼 util.go", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(receipt, "唤醒") {
		t.Fatalf("回执 = %q", receipt)
	}
	waitFor(t, 3*time.Second, func() bool { return mgr.Pending() == 0 })
	settled := mgr.TakeSettled()
	if len(settled) != 1 || settled[0].JobID != "Scout" {
		t.Fatalf("续跑结果 = %+v", settled)
	}
	data, _ := settled[0].Result.Data.(map[string]any)
	if data["round"] != float64(2) && data["round"] != 2 {
		t.Fatalf("续跑应产出第二轮结果：%+v", settled[0].Result.Data)
	}
	if v := mgr.lookup("Scout").View(); v.Revives != 1 {
		t.Fatalf("revives = %d", v.Revives)
	}
	// 转录里应能看到追问那句（续跑写进了同一个 sidecar）
	tr := sidecar(t, settled[0].Result)
	if !strings.Contains(tr, "再看一眼 util.go") {
		t.Fatalf("续跑没写进原转录：%s", tr)
	}
}

func TestRenderAsyncResultShapes(t *testing.T) {
	jobs := []JobResult{{JobID: "Scout", Result: Result{Name: "Scout", Agent: "explorer", Status: StatusCompleted, Data: map[string]any{"ok": true}}}}
	out := RenderAsyncResult(jobs, nil)
	if !strings.Contains(out, "<system-notice>") || !strings.Contains(out, "后台作业 Scout 已完成") || !strings.Contains(out, "Scout (explorer)") {
		t.Fatalf("单作业渲染 = %s", out)
	}
	out = RenderAsyncResult(append(jobs, JobResult{JobID: "Fixer", Result: Result{Name: "Fixer", Status: StatusFailed}}),
		[]Mail{{From: "Scout", Text: "接口定了"}})
	if !strings.Contains(out, "2 个后台作业") || !strings.Contains(out, "来自 Scout：接口定了") {
		t.Fatalf("多作业 + 消息渲染 = %s", out)
	}
}

func TestGateResize(t *testing.T) {
	g := newGate(1)
	ctx := context.Background()
	if err := g.acquire(ctx); err != nil {
		t.Fatal(err)
	}
	// 扩容后另外两个也能拿到
	g.setLimit(3)
	if err := g.acquire(ctx); err != nil {
		t.Fatal(err)
	}
	if err := g.acquire(ctx); err != nil {
		t.Fatal(err)
	}
	// 缩容：在跑的不打断，归还的令牌被扣掉
	g.setLimit(1)
	g.release()
	g.release()
	blocked := make(chan struct{})
	go func() { _ = g.acquire(ctx); close(blocked) }()
	select {
	case <-blocked:
		t.Fatal("缩容后不该还有空闲令牌")
	case <-time.After(100 * time.Millisecond):
	}
	g.release() // 最后一个归还后，池子里恰好剩 1 枚
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("归还后应能拿到令牌")
	}
	if g.limitNow() != 1 {
		t.Fatalf("limit = %d", g.limitNow())
	}
}

func TestGateAcquireRespectsContext(t *testing.T) {
	g := newGate(1)
	_ = g.acquire(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); time.Sleep(30 * time.Millisecond); cancel() }()
	if err := g.acquire(ctx); err == nil {
		t.Fatal("ctx 取消后 acquire 应返回错误")
	}
	wg.Wait()
}

// pendingSnapshot 测试用：看一眼待投递队列而不消费。
func (m *Manager) pendingSnapshot() []JobResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]JobResult(nil), m.pending...)
}
