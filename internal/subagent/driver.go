package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"einoclaw-build/internal/agent"
	agentctx "einoclaw-build/internal/context"
	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
	"einoclaw-build/internal/session"
	"einoclaw-build/internal/tool"
)

// 驱动常量：turn 阶梯与软预算三段式。
const (
	maxYieldReminders    = 3   // turn 结束却没 yield 时的提醒次数；最后一次把工具集换成只有 yield
	budgetStopMultiplier = 1.5 // 软预算的 1.5 倍 = 停机线
	budgetGraceRequests  = 5   // 停机后仍在烧请求（例如反复交错格式的 yield）就硬杀
	forcedTurnIterations = 3   // 强制收尾那一 turn 的工具循环上限：够 yield 一次 + 一次纠错
)

// 事件总线通道名。父 agent 不订阅这些通道——原始事件只服务 UI 与 Manager。
const (
	ChLifecycle = "subagent.lifecycle"
	ChProgress  = "subagent.progress"
	ChEvent     = "subagent.event"
	ChJob       = "job.settled"
	ChMailbox   = "hub.message"
)

// Lifecycle 是一次状态变化。
type Lifecycle struct {
	RunID, Name, Agent, Status string
	SessionFile, OutputFile    string
	Depth                      int
}

// Progress 是一个 Run 的运行时快照（TUI Agent Hub 用）。
type Progress struct {
	RunID, Name, CurrentTool    string
	Requests, ToolCalls, Tokens int
	ContextTokens, Reminders    int
	BudgetStop                  bool
}

// SubEvent 是子 agent 的原始事件（供聚焦转录用）。
type SubEvent struct {
	RunID, Name string
	Event       agent.AgentEvent
}

// Run 是一次子 agent 执行的可变状态。对外只暴露快照（View），所有读写走 mu。
type Run struct {
	mu sync.Mutex

	id, name, agentName string
	depth               int
	status              Status
	currentTool         string
	requests            int
	toolCalls           int
	reminders           int
	revives             int
	usage               model.Usage
	contextTokens       int
	lastText            string
	runErr              error

	budgetStop  bool
	noticeSent  bool
	killed      bool
	startedAt   time.Time
	settledAt   time.Time
	sessionFile string
	outputFile  string

	steer      chan message.Message
	cancelRun  context.CancelFunc
	cancelTurn context.CancelFunc
}

// runtimeSet 是一个 Run 的运行时装配：工具集（含只含 yield 的备用集）、会话、上下文与产出累积。
type runtimeSet struct {
	def              AgentDef
	tools, yieldOnly *tool.Registry
	exec, yieldExec  *tool.Executor
	cc               *agentctx.Manager
	sess             *session.Session
	file             string
	ys               *YieldState
	schema           map[string]any
	mode             string
}

func newRun(name, agentName string, depth int) *Run {
	return &Run{
		id: name, name: name, agentName: agentName, depth: depth,
		status: StatusPending, startedAt: time.Now(),
		steer: make(chan message.Message, 8),
	}
}

// View 返回名册用的快照。
func (r *Run) View() RunView {
	r.mu.Lock()
	defer r.mu.Unlock()
	age := time.Since(r.startedAt)
	if !r.settledAt.IsZero() {
		age = r.settledAt.Sub(r.startedAt)
	}
	return RunView{
		ID: r.id, Name: r.name, Agent: r.agentName, Status: StatusString(r.status),
		CurrentTool: r.currentTool, Depth: r.depth, Requests: r.requests, ToolCalls: r.toolCalls,
		Tokens: r.usage.TotalTokens, ContextTokens: r.contextTokens, Revives: r.revives,
		Age: age, SessionFile: r.sessionFile, OutputFile: r.outputFile,
	}
}

// Name 返回运行名（hub 寻址）。
func (r *Run) Name() string { return r.name }

// Cancel 硬杀这个 Run（hub cancel / TUI x / 预算宽限耗尽）。
func (r *Run) Cancel() {
	r.mu.Lock()
	r.killed = true
	c := r.cancelRun
	r.mu.Unlock()
	if c != nil {
		c()
	}
}

// Steer 非阻塞注入一条消息（预算通知 / hub 消息 / 父 steering）；满则丢，避免拖慢驱动。
func (r *Run) Steer(m message.Message) bool {
	select {
	case r.steer <- m:
		return true
	default:
		return false
	}
}

func (r *Run) setStatus(s Status) {
	r.mu.Lock()
	r.status = s
	r.mu.Unlock()
}

// statusNow 返回当前状态。
func (r *Run) statusNow() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

// settled 表示已结算（不再消耗模型请求，可读产出与转录）。
func (r *Run) settled() bool { return r.statusNow().Settled() }

// drive 驱动一个 Run 走完 turn 阶梯：每 turn 一个可单独取消的 ctx；turn 结束若没 terminal yield
// 就注入提醒（最后一次只给 yield 工具）；软预算越界先通知、再停机强制收尾、宽限耗尽硬杀。
func (m *Manager) drive(parent context.Context, r *Run, rs *runtimeSet) Result {
	def := rs.def
	runCtx, cancelRun := context.WithCancel(parent)
	defer cancelRun()
	if def.Timeout > 0 {
		var cancelTimeout context.CancelFunc
		runCtx, cancelTimeout = context.WithTimeout(runCtx, def.Timeout)
		defer cancelTimeout()
	}
	r.mu.Lock()
	r.cancelRun = cancelRun
	r.status = StatusRunning
	r.sessionFile = rs.file
	r.mu.Unlock()
	m.publishLifecycle(r)

	forced := false
	for {
		turnCtx, cancelTurn := context.WithCancel(runCtx)
		r.mu.Lock()
		r.cancelTurn = cancelTurn
		r.mu.Unlock()

		reg, exec, iters := rs.tools, rs.exec, def.MaxTurns
		if forced {
			reg, exec, iters = rs.yieldOnly, rs.yieldExec, forcedTurnIterations
		}
		sub := agent.New(def.Name, m.o.Model, reg, exec, rs.cc)
		sub.SetMaxIterations(iters)
		// 必须把事件通道读到关闭：agent.Run 的 goroutine 在那之后才结束，否则下一 turn 会与它并发写同一 session
		m.consume(r, def, sub.Run(turnCtx, r.steer))
		cancelTurn()

		if _, _, _, terminal := rs.ys.Snapshot(); terminal {
			break
		}
		if runCtx.Err() != nil || r.isKilled() {
			break
		}
		if r.budgetStopped() {
			if forced {
				break // 已经强制收尾过一轮还是不 yield：交给状态判定（killed）
			}
			forced = true
			r.setStatus(StatusBudgetStop)
			m.publishLifecycle(r)
			_ = rs.cc.Record(message.NewUserMessage(forcedBudgetNotice(r.requestCount(), def.SoftBudget)), model.Usage{})
			continue
		}
		if r.reminderCount() >= maxYieldReminders {
			break
		}
		n := r.bumpReminder()
		forced = n >= maxYieldReminders
		r.setStatus(StatusIdle)
		m.publishLifecycle(r)
		_ = rs.cc.Record(message.NewUserMessage(idleReminder(n, forced)), model.Usage{})
		r.setStatus(StatusRunning)
	}
	return m.settle(parent, runCtx, r, rs)
}

// consume 消费一个 turn 的事件：记账、进度发布、预算判定。
func (m *Manager) consume(r *Run, def AgentDef, events <-chan agent.AgentEvent) {
	for ev := range events {
		switch ev.Type {
		case agent.EventMessageEnd:
			r.mu.Lock()
			r.requests++
			r.usage = r.usage.Add(ev.Ended.Usage)
			if ev.Ended.Usage.PromptTokens > 0 {
				r.contextTokens = ev.Ended.Usage.PromptTokens
			}
			if txt := textOf(ev.Ended.Message); strings.TrimSpace(txt) != "" {
				r.lastText = txt
			}
			r.mu.Unlock()
			m.checkBudget(r, def)
		case agent.EventToolStart:
			r.mu.Lock()
			r.toolCalls++
			r.currentTool = ev.ToolStart.Name
			r.mu.Unlock()
		case agent.EventToolEnd:
			r.mu.Lock()
			r.currentTool = ""
			r.mu.Unlock()
		case agent.EventError:
			r.mu.Lock()
			r.runErr = ev.Err
			r.mu.Unlock()
		}
		m.publishProgress(r)
		if m.o.Bus != nil {
			m.o.Bus.Publish(ChEvent, SubEvent{RunID: r.id, Name: r.name, Event: ev})
		}
	}
}

// checkBudget 软预算三段式：越界通知 → 停机强制收尾 → 宽限耗尽硬杀。
func (m *Manager) checkBudget(r *Run, def AgentDef) {
	if def.SoftBudget <= 0 {
		return
	}
	stopAt := int(math.Ceil(float64(def.SoftBudget) * budgetStopMultiplier))
	req := r.requestCount()
	switch {
	case r.budgetStopped():
		if req >= stopAt+budgetGraceRequests {
			r.Cancel() // 停机后还在烧请求（例如反复提交非法 yield）：不再等
		}
	case req >= stopAt:
		r.markBudgetStop()
		r.cancelCurrentTurn() // 只停当前 turn，Run 继续活着去做强制收尾
	case req >= def.SoftBudget && !r.markNoticeSent():
		r.Steer(message.NewUserMessage(budgetNotice(req, def.SoftBudget, stopAt)))
	}
}

// settle 判定终态、落盘产出、写 session_exit，并组装 Result。
func (m *Manager) settle(parent, runCtx context.Context, r *Run, rs *runtimeSet) Result {
	data, sections, yieldErr, terminal := rs.ys.Snapshot()
	overridden, violation, issues := rs.ys.Flags()

	r.mu.Lock()
	res := Result{
		ID: r.id, Name: r.name, Agent: rs.def.Name, Data: data, Sections: sections, Text: r.lastText,
		Usage: r.usage, Requests: r.requests, ToolCalls: r.toolCalls, Reminders: r.reminders,
		BudgetStopped: r.budgetStop, DurationMs: time.Since(r.startedAt).Milliseconds(), SessionFile: rs.file,
	}
	killed, runErr := r.killed, r.runErr
	r.mu.Unlock()

	switch {
	case parent.Err() != nil:
		res.Status, res.Err = StatusAborted, parent.Err()
	case killed:
		res.Status = StatusKilled
		res.Err = fmt.Errorf("子 agent 被终止（预算耗尽或人工取消），已用 %d 次请求", res.Requests)
	case res.BudgetStopped && !terminal:
		res.Status = StatusKilled
		res.Err = fmt.Errorf("软预算耗尽后仍未 yield（%d 次请求）", res.Requests)
	case runCtx.Err() != nil:
		res.Status, res.Err = StatusTimeout, fmt.Errorf("子 agent 超时（%s）", rs.def.Timeout)
	case runErr != nil:
		res.Status, res.Err = StatusFailed, runErr
	case yieldErr != "":
		res.Status, res.Err = StatusFailed, errors.New(yieldErr)
	case violation:
		res.Status, res.Err = StatusFailed, fmt.Errorf("schema_violation: %s", strings.Join(issues, "；"))
	case terminal:
		res.Status, res.Yielded = StatusCompleted, true
		if overridden {
			res.Warning = "产出未通过 schema 校验，已按 permissive 放行：" + strings.Join(issues, "；")
		}
		if res.BudgetStopped {
			res.Warning = strings.TrimSpace(res.Warning + " 结果是软预算强制收尾后提交的，可能不完整。")
		}
	case rs.schema != nil:
		res.Status = StatusFailed
		res.Err = fmt.Errorf("提醒 %d 次后仍未通过 yield 提交结构化产出", res.Reminders)
	default:
		res.Status = StatusCompleted
		res.Warning = fmt.Sprintf("提醒 %d 次后仍未 yield，以下是最后一段输出（可能不是完整结论）", res.Reminders)
	}

	if f, err := m.writeOutput(r.name, res); err == nil {
		res.OutputFile = f
	}
	_ = rs.sess.AppendCustom("session_exit", map[string]any{
		"status": StatusString(res.Status), "requests": res.Requests, "toolCalls": res.ToolCalls,
		"reminders": res.Reminders, "yielded": res.Yielded, "budgetStopped": res.BudgetStopped,
		"schemaOverridden": overridden, "schemaViolation": violation, "durationMs": res.DurationMs,
	})

	r.mu.Lock()
	r.status = StatusParked // 已结算，转录与产出保留，可被 hub 唤醒
	r.settledAt = time.Now()
	r.outputFile = res.OutputFile
	r.currentTool = ""
	r.mu.Unlock()
	m.publishLifecycle(r)
	return res
}

// writeOutput 把完整产出写进会话产物目录，父只拿摘要 + agent://<Name> 指针。
func (m *Manager) writeOutput(name string, res Result) (string, error) {
	if m.o.SessionDir == "" {
		return "", errors.New("no session dir")
	}
	if err := os.MkdirAll(m.o.SessionDir, 0o755); err != nil {
		return "", err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s (%s)\n\n", name, res.Agent)
	fmt.Fprintf(&sb, "- status: %s\n- requests: %d\n- tool_calls: %d\n- tokens: %d\n- duration_ms: %d\n",
		StatusString(res.Status), res.Requests, res.ToolCalls, res.Usage.TotalTokens, res.DurationMs)
	if res.Warning != "" {
		fmt.Fprintf(&sb, "- warning: %s\n", res.Warning)
	}
	if res.Err != nil {
		fmt.Fprintf(&sb, "- error: %v\n", res.Err)
	}
	if res.Data != nil {
		sb.WriteString("\n## data\n\n```json\n")
		if b, err := json.MarshalIndent(res.Data, "", "  "); err == nil {
			sb.Write(b)
		} else {
			fmt.Fprintf(&sb, "%v", res.Data)
		}
		sb.WriteString("\n```\n")
	}
	if len(res.Sections) > 0 {
		sb.WriteString("\n## sections\n\n```json\n")
		if b, err := json.MarshalIndent(res.Sections, "", "  "); err == nil {
			sb.Write(b)
		}
		sb.WriteString("\n```\n")
	}
	if res.Text != "" {
		sb.WriteString("\n## last text\n\n")
		sb.WriteString(res.Text)
		sb.WriteString("\n")
	}
	path := filepath.Join(m.o.SessionDir, sanitizeName(name)+".md")
	return path, os.WriteFile(path, []byte(sb.String()), 0o644)
}

// ---------- 提醒与通知文本 ----------

func idleReminder(n int, forced bool) string {
	if forced {
		return fmt.Sprintf("[提醒 %d/%d] 本轮只剩 yield 一个工具可用：立刻调用 yield 提交你已有的结论；"+
			"确实无法完成就用 yield(error=\"…\") 说明卡在哪。只输出文本不会有任何结果回到主 agent。", n, maxYieldReminders)
	}
	return fmt.Sprintf("[提醒 %d/%d] 你还没有调用 yield 提交结果。任务已完成就调用 yield(data=…)；"+
		"无法完成就调用 yield(error=…)。只有 yield 的内容会回到主 agent，纯文本输出会被丢弃。", n, maxYieldReminders)
}

func budgetNotice(requests, budget, stopAt int) string {
	return fmt.Sprintf("[预算提醒] 本次运行已用 %d 次模型请求（软预算 %d）。请开始收尾：结束当前步骤并调用 yield 提交结论。"+
		"到 %d 次请求时本轮会被强制停止，届时只能调用 yield。", requests, budget, stopAt)
}

func forcedBudgetNotice(requests, budget int) string {
	return fmt.Sprintf("[强制收尾] 已用 %d 次模型请求（软预算 %d），本轮已被停止，工具集现在只有 yield。"+
		"把已有的结论或半成品用 yield(data=…) 提交上来；完全没有可交付内容就用 yield(error=…) 说明原因。", requests, budget)
}

// ---------- Run 的小访问器 ----------

func (r *Run) requestCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests
}

func (r *Run) reminderCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reminders
}

func (r *Run) bumpReminder() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reminders++
	return r.reminders
}

func (r *Run) budgetStopped() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.budgetStop
}

func (r *Run) markBudgetStop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.budgetStop = true
}

// markNoticeSent 返回之前是否已发过收尾通知（一次运行只发一次）。
func (r *Run) markNoticeSent() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	was := r.noticeSent
	r.noticeSent = true
	return was
}

func (r *Run) isKilled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.killed
}

func (r *Run) cancelCurrentTurn() {
	r.mu.Lock()
	c := r.cancelTurn
	r.mu.Unlock()
	if c != nil {
		c()
	}
}

// ---------- 总线发布 ----------

func (m *Manager) publishLifecycle(r *Run) {
	if m.o.Bus == nil {
		return
	}
	v := r.View()
	m.o.Bus.Publish(ChLifecycle, Lifecycle{
		RunID: v.ID, Name: v.Name, Agent: v.Agent, Status: v.Status,
		SessionFile: v.SessionFile, OutputFile: v.OutputFile, Depth: v.Depth,
	})
}

func (m *Manager) publishProgress(r *Run) {
	if m.o.Bus == nil {
		return
	}
	v := r.View()
	m.o.Bus.Publish(ChProgress, Progress{
		RunID: v.ID, Name: v.Name, CurrentTool: v.CurrentTool, Requests: v.Requests,
		ToolCalls: v.ToolCalls, Tokens: v.Tokens, ContextTokens: v.ContextTokens,
		Reminders: r.reminderCount(), BudgetStop: r.budgetStopped(),
	})
}
