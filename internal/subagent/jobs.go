package subagent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
)

// gateHardMax 并发闸的硬上限：运行时可以在 [1, gateHardMax] 之间调整。
const gateHardMax = 64

// gate 是可 resize 的并发闸。令牌放在一个容量固定的通道里，acquire 取一枚、release 还一枚；
// 缩容时如果当前取不到令牌就记一笔「债」，等在跑的 Run 归还时直接扣掉，从而不打断正在跑的任务。
type gate struct {
	tokens chan struct{}
	mu     sync.Mutex
	limit  int
	debt   int
}

func newGate(n int) *gate {
	g := &gate{tokens: make(chan struct{}, gateHardMax)}
	g.setLimit(n)
	return g
}

func (g *gate) acquire(ctx context.Context) error {
	select {
	case <-g.tokens:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *gate) release() {
	g.mu.Lock()
	if g.debt > 0 { // 缩容欠的债：这枚令牌不还回池子
		g.debt--
		g.mu.Unlock()
		return
	}
	g.mu.Unlock()
	select {
	case g.tokens <- struct{}{}:
	default: // 池子满：说明外部多还了一次，忽略
	}
}

func (g *gate) setLimit(n int) {
	if n < 1 {
		n = 1
	}
	if n > gateHardMax {
		n = gateHardMax
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case n > g.limit:
		add := n - g.limit
		if d := min(g.debt, add); d > 0 { // 先还债
			g.debt -= d
			add -= d
		}
		for i := 0; i < add; i++ {
			select {
			case g.tokens <- struct{}{}:
			default:
			}
		}
	case n < g.limit:
		for i := 0; i < g.limit-n; i++ {
			select {
			case <-g.tokens: // 空闲令牌直接收走
			default:
				g.debt++ // 令牌在跑，记账等归还
			}
		}
	}
	g.limit = n
}

func (g *gate) limitNow() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.limit
}

// JobInfo 一个后台作业的快照。作业 id 就是运行名（同时是 hub 地址与 agent:// 地址）。
type JobInfo struct {
	ID      string
	Agent   string
	Status  string
	Started time.Time
	Settled time.Time
	Summary string // 已结束时：给模型看的结果渲染
}

// JobResult 已结算、还没投递给父的结果。
type JobResult struct {
	DeliveryID string
	JobID      string
	Owner      string
	SessionID  string
	Result     Result
}

// StartBackground 派发一批后台作业：立刻返回作业 id，结果结算后按 async-result 投递。
// 定义里 blocking: true 的 agent 仍然内联执行（inline 返回），其余转后台。
// 后台 Run 挂 Manager 的根 ctx，不挂调用方 ctx —— 否则父 turn 一结束后台作业就被连带取消。
func (m *Manager) StartBackground(ctx context.Context, b TaskBatch, env Env) (inline []Result, jobs []JobInfo, err error) {
	items, err := Preflight(b, env)
	if err != nil {
		return nil, nil, err
	}
	for _, it := range items {
		def := m.resolveDef(it.Def)
		if def.Blocking {
			inline = append(inline, m.run(ctx, b.Context, it, env.Depth+1, env.SessionID))
			continue
		}
		owner := env.Owner
		if owner == "" {
			owner = MainName
		}
		sessionID := env.SessionID
		if sessionID == "" {
			sessionID = m.CurrentSessionID()
		}
		run := newOwnedRun(it.Item.Name, def.Name, env.Depth+1, owner, sessionID)
		run.background = true
		m.register(run)
		jobs = append(jobs, JobInfo{ID: run.name, Agent: def.Name, Status: StatusString(StatusPending), Started: run.startedAt})

		m.wg.Add(1)
		go func(it Resolved, def AgentDef, run *Run) {
			defer m.wg.Done()
			defer func() {
				if rec := recover(); rec != nil {
					m.settleJob(run, Result{ID: run.name, Name: run.name, Agent: def.Name,
						Status: StatusFailed, Err: fmt.Errorf("子 agent panic: %v", rec)})
					m.wakeOwner(run.name, run.sessionID)
				}
			}()
			rs, err := m.setup(def, it, b.Context, env.Depth+1, run)
			if err != nil {
				m.settleJob(run, Result{ID: run.name, Name: run.name, Agent: def.Name, Status: StatusFailed, Err: err})
				return
			}
			res := func() Result {
				defer rs.sess.Close()
				return m.drive(m.root, run, rs)
			}()
			m.settleJob(run, res)
			m.wakeOwner(run.name, run.sessionID)
		}(it, def, run)
	}
	return inline, jobs, nil
}

// settleJob 把一个后台作业的结果放进待投递队列，并广播作业结束。
func (m *Manager) settleJob(run *Run, res Result) {
	run.setStatus(StatusParked)
	owner, sessionID := run.deliveryRoute()
	m.mu.Lock()
	m.pending = append(m.pending, JobResult{
		DeliveryID: fmt.Sprintf("delivery-%d", m.deliverySeq.Add(1)),
		JobID:      run.name, Owner: owner, SessionID: sessionID, Result: res,
	})
	m.mu.Unlock()
	if m.o.Bus != nil {
		m.o.Bus.Publish(ChJob, JobSettled{JobID: run.name, Name: run.name,
			Status: StatusString(res.Status), Summary: firstLine(res)})
	}
	m.wakeOwner(owner, sessionID)
}

// wakeOwner 把属于某个已 parked 父 Run 的结果持久化进它的 sidecar，并自动续跑。
// 结果只有在 Record 成功、revive 已启动后才 ACK；父仍在运行时先保留，等它结算后再处理。
func (m *Manager) wakeOwner(owner, sessionID string) {
	if owner == "" || owner == MainName {
		return
	}
	parent := m.lookup(owner)
	if parent == nil || !parent.settled() {
		return
	}
	items := m.PeekSettled(owner, sessionID)
	if len(items) == 0 {
		return
	}
	parentOwner, parentSession := parent.deliveryRoute()
	if _, err := m.revive(owner, RenderAsyncResult(items, nil), parentOwner, parentSession); err != nil {
		return
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.DeliveryID)
	}
	m.AckSettled(ids)
}

// JobSettled 是作业结束广播（TUI 用它触发投递）。
type JobSettled struct{ JobID, Name, Status, Summary string }

// Jobs 返回后台作业快照。已结束的行会**同时消费投递**：模型从这里看到结果后，
// 就不该再收到一份 async-result（对齐 oh-my-pi 的 "settled row consumes auto-delivery"）。
func (m *Manager) Jobs() []JobInfo {
	return m.JobsFor(MainName, m.CurrentSessionID())
}

// JobsFor 返回指定调用者/根会话能看到的后台作业，并消费其中已结算的结果。
func (m *Manager) JobsFor(owner, sessionID string) []JobInfo {
	taken := m.TakeSettledFor(owner, sessionID)
	byID := map[string]Result{}
	for _, s := range taken {
		byID[s.JobID] = s.Result
	}
	m.mu.Lock()
	names := append([]string(nil), m.order...)
	runs := m.runs
	out := make([]JobInfo, 0, len(names))
	for _, n := range names {
		r := runs[n]
		if r == nil || !r.isBackground() {
			continue
		}
		runOwner, runSession := r.deliveryRoute()
		if runOwner != owner || runSession != sessionID {
			continue
		}
		v := r.View()
		info := JobInfo{ID: v.ID, Agent: v.Agent, Status: v.Status, Started: r.startedAt, Settled: r.settledAt}
		if res, ok := byID[v.ID]; ok {
			info.Summary = renderResult(res)
		}
		out = append(out, info)
	}
	m.mu.Unlock()
	return out
}

// TakeSettled 取走已结算但还没投递的结果（一次性：谁先取到谁负责投递）。
func (m *Manager) TakeSettled() []JobResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.pending
	m.pending = nil
	return out
}

// PeekSettled 返回指定收件人/根会话的待投递结果，但不删除；调用方在把通知
// 成功写入目标会话后再 AckSettled，避免“先取走、后注入失败”导致结果丢失。
func (m *Manager) PeekSettled(owner, sessionID string) []JobResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []JobResult
	for _, item := range m.pending {
		if item.Owner == owner && item.SessionID == sessionID {
			out = append(out, item)
		}
	}
	return out
}

// AckSettled 确认已持久化的投递；其它结果保持原顺序。
func (m *Manager) AckSettled(deliveryIDs []string) {
	if len(deliveryIDs) == 0 {
		return
	}
	done := make(map[string]bool, len(deliveryIDs))
	for _, id := range deliveryIDs {
		done[id] = true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	keep := m.pending[:0]
	for _, item := range m.pending {
		if !done[item.DeliveryID] {
			keep = append(keep, item)
		}
	}
	m.pending = keep
}

// TakeSettledFor 是 hub 的作用域化一次性取件入口。
func (m *Manager) TakeSettledFor(owner, sessionID string) []JobResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	var items, keep []JobResult
	for _, item := range m.pending {
		if item.Owner == owner && item.SessionID == sessionID {
			items = append(items, item)
		} else {
			keep = append(keep, item)
		}
	}
	m.pending = keep
	return items
}

// Pending 返回还在跑的后台作业数。
func (m *Manager) Pending() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, name := range m.order {
		if r := m.runs[name]; r != nil && r.isBackground() && !r.settled() {
			n++
		}
	}
	return n
}

// SetConcurrency 运行时调整并发上限（缩容不打断在跑的 Run）。
func (m *Manager) SetConcurrency(n int) { m.gate.setLimit(n) }

// Concurrency 返回当前并发上限。
func (m *Manager) Concurrency() int { return m.gate.limitNow() }

// Shutdown 取消所有后台 Run 并等它们退出（超过 grace 就不再等，打印责任交给调用方）。
func (m *Manager) Shutdown(grace time.Duration) error {
	m.rootCancel()
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-time.After(grace):
		return fmt.Errorf("仍有 %d 个后台作业未在 %s 内退出", m.Pending(), grace)
	}
}

// Revive 唤醒一个已结束的 Run：重开它的 sidecar 续跑，结果按后台作业投递。
// text 原样记进它的会话（署名由调用方写好，见 formatMail），转录里能直接看出是谁在追问。
func (m *Manager) Revive(name, text string) (JobInfo, error) {
	return m.revive(name, text, MainName, m.CurrentSessionID())
}

func (m *Manager) revive(name, text, owner, sessionID string) (JobInfo, error) {
	m.reviveMu.Lock()
	defer m.reviveMu.Unlock()
	run := m.lookup(name)
	if run == nil {
		return JobInfo{}, fmt.Errorf("没有名为 %q 的子 agent", name)
	}
	if !run.settled() {
		return JobInfo{}, fmt.Errorf("%s 还在运行中，直接 send 即可", name)
	}
	spec := run.spawnSpec()
	if spec.file == "" {
		return JobInfo{}, fmt.Errorf("%s 没有可续跑的转录（内存会话）", name)
	}
	rs, err := m.setupResume(spec)
	if err != nil {
		return JobInfo{}, err
	}
	if err := rs.cc.Record(message.NewUserMessage(text), model.Usage{}); err != nil {
		rs.sess.Close()
		return JobInfo{}, err
	}
	run.mu.Lock()
	run.owner, run.sessionID = owner, sessionID
	run.mu.Unlock()
	run.resetForRevive()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() {
			if rec := recover(); rec != nil {
				m.settleJob(run, Result{ID: run.name, Name: run.name, Agent: spec.def.Name,
					Status: StatusFailed, Err: fmt.Errorf("子 agent panic: %v", rec)})
				m.wakeOwner(run.name, run.sessionID)
			}
		}()
		res := func() Result {
			defer rs.sess.Close()
			return m.drive(m.root, run, rs)
		}()
		m.settleJob(run, res)
		m.wakeOwner(run.name, run.sessionID)
	}()
	return JobInfo{ID: run.name, Agent: spec.def.Name, Status: StatusString(StatusRunning), Started: time.Now()}, nil
}

// RenderAsyncResult 渲染后台作业结果与 hub 消息，作为一条系统通知注入父会话。
func RenderAsyncResult(jobs []JobResult, mails []Mail) string {
	var sb strings.Builder
	sb.WriteString("<system-notice>\n")
	if len(jobs) == 1 {
		fmt.Fprintf(&sb, "后台作业 %s 已完成。用下面的结果继续你的工作。\n\n", jobs[0].JobID)
	} else if len(jobs) > 1 {
		fmt.Fprintf(&sb, "%d 个后台作业已完成。用下面的结果继续你的工作。\n\n", len(jobs))
	}
	for i, j := range jobs {
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(renderResult(j.Result))
		sb.WriteString("\n")
	}
	if len(mails) > 0 {
		if len(jobs) > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString("子 agent 给你发了消息：\n")
		for _, ml := range mails {
			fmt.Fprintf(&sb, "- 来自 %s：%s\n", ml.From, ml.Text)
		}
	}
	sb.WriteString("</system-notice>")
	return sb.String()
}

// firstLine 取结果的一行摘要（作业广播用）。
func firstLine(res Result) string {
	s := fmt.Sprintf("%s [%s] requests=%d tokens=%d", res.Name, StatusString(res.Status), res.Requests, res.Usage.TotalTokens)
	if res.Err != nil {
		s += " error=" + res.Err.Error()
	}
	return s
}

func (r *Run) isBackground() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.background
}

func (r *Run) spawnSpec() spawnSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.spawn
}
