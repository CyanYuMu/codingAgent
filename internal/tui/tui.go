package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"einoclaw-build/internal/agent"
	"einoclaw-build/internal/bus"
	agentctx "einoclaw-build/internal/context"
	"einoclaw-build/internal/memory"
	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
	"einoclaw-build/internal/session"
	"einoclaw-build/internal/skills"
	"einoclaw-build/internal/subagent"
)

// 事件源是我们自己的 agent.AgentEvent；持久化由循环内的 Context 完成，TUI 只负责渲染与会话切换。

var (
	userPrefix = lipgloss.NewStyle().Foreground(lipgloss.Color("63")).Bold(true).Render("┃ ")
	aiPrefix   = lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Render("● ")
	dimStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))

	// 双协程桥接用（沿用「全局 program + program.Send」模式）
	program       *tea.Program
	currentCancel context.CancelFunc
	currentSteer  chan message.Message // 当前 run 的 steering 通道
	currentRunID  uint64
	runSeq        atomic.Uint64
	runMu         sync.Mutex // 保证同一时刻只有一个 run（防双 run 竞态）
	steerMu       sync.Mutex // 保护 currentCancel/currentSteer：TUI 主循环与 run goroutine 都会碰
)

// setCurrentRun 记录当前 run 的取消函数与 steering 通道，并返回代次 id。
func setCurrentRun(cancel context.CancelFunc, steer chan message.Message) uint64 {
	steerMu.Lock()
	defer steerMu.Unlock()
	id := runSeq.Add(1)
	currentRunID = id
	currentCancel, currentSteer = cancel, steer
	return id
}

// clearCurrentRun 只清理自己这一代，避免旧 run 结束时抹掉已经启动的新 run。
func clearCurrentRun(id uint64) {
	steerMu.Lock()
	defer steerMu.Unlock()
	if currentRunID == id {
		currentCancel, currentSteer, currentRunID = nil, nil, 0
	}
}

func runActive() bool {
	steerMu.Lock()
	defer steerMu.Unlock()
	return currentRunID != 0
}

// cancelCurrent 取消当前 run（若有）。
func cancelCurrent() {
	steerMu.Lock()
	c := currentCancel
	steerMu.Unlock()
	if c != nil {
		c()
	}
}

// trySteer 把消息注入当前 run；没有活动 run 或队列满则返回 false，调用方改为另起一轮。
func trySteer(msg message.Message) bool {
	steerMu.Lock()
	defer steerMu.Unlock()
	if currentSteer == nil {
		return false
	}
	select {
	case currentSteer <- msg:
		return true
	default:
		return false
	}
}

// SetProgram 注入 BubbleTea program，供后台 goroutine 把事件塞回 TUI 主循环。
func SetProgram(p *tea.Program) { program = p }

type teaModel struct {
	width             int
	height            int
	chatLines         []string // 已完成的终端行(已渲染、已带前缀)
	streaming         string   // 当前流式 AI 正文(Markdown 原文)
	stream            *streamingMarkdown
	streamingThinking string // 当前流式思考(原文)
	inputArea         textarea.Model
	agent             *agent.Agent
	session           *session.Session
	mgr               *session.Manager // 多会话管理（/new /resume）
	cmgr              *agentctx.Manager
	mem               *memory.Store       // 长期记忆（/forget 用）
	cwd               string              // 新建会话时写入 header
	pendingApproval   *approvalRequestMsg // nil = 无待审批
	scrollOffset      int                 // 聊天滚动偏移（0=底部，>0=上滚 N 行）

	sub     *subagent.Manager   // 子 agent 名册（Agent Hub 面板 / /agent 转发）
	hubCh   <-chan bus.Envelope // 子 agent 事件（唤醒重绘）
	hubOpen bool
	hubSel  int

	skillCatalog  *skills.Manager
	skillCommands bool
	pendingSwitch *sessionSwitch
}

type sessionSwitch struct {
	new bool
	id  string
}

type runFinishedMsg struct{ id uint64 }

// NewModel 构造 TUI 模型；cmgr 持有当前会话，cwd 用于新建会话，sub/b 提供 Agent Hub。
func NewModel(ag *agent.Agent, mgr *session.Manager, cmgr *agentctx.Manager, mem *memory.Store, cwd string,
	sub *subagent.Manager, b *bus.Bus, skillCatalog *skills.Manager, skillCommands bool) teaModel {
	ta := textarea.New()
	ta.Placeholder = " Type your message... (Enter=send, Ctrl+J=newline, Ctrl+E=steer, Ctrl+C=quit)"
	ta.SetHeight(3)
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("ctrl+j"), key.WithHelp("ctrl+j", "newline"))
	ta.Focus()
	s := cmgr.Session()
	m := teaModel{inputArea: ta, agent: ag, session: s, mgr: mgr, cmgr: cmgr, mem: mem, cwd: cwd,
		sub: sub, hubCh: mergeHubEvents(b), skillCatalog: skillCatalog, skillCommands: skillCommands}
	// 恢复历史：replay 后渲染进聊天区
	if msgs, err := s.Replay(); err == nil {
		m.chatLines = renderHistory(msgs)
	}
	return m
}

func (m teaModel) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, tea.RequestWindowSize, waitHubEvent(m.hubCh), pollHub())
}

func (m teaModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.inputArea.SetWidth(max(1, m.width-2))
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case agent.AgentEvent:
		return m.handleAgentEvent(msg)

	case approvalRequestMsg:
		m.pendingApproval = &msg // 弹审批窗
		return m, nil

	case hubTickMsg:
		m = m.deliverPending()
		return m, waitHubEvent(m.hubCh)

	case hubPollMsg:
		m = m.deliverPending()
		return m, pollHub()

	case runFinishedMsg:
		if m.pendingSwitch != nil && !runActive() {
			m = m.applySessionSwitch(*m.pendingSwitch)
			m.pendingSwitch = nil
		}
		m = m.deliverPending()
		return m, nil

	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelUp:
			m.scrollOffset += 3 // 上滚
		case tea.MouseWheelDown:
			m.scrollOffset -= 3 // 下滚
			if m.scrollOffset < 0 {
				m.scrollOffset = 0
			}
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.inputArea, cmd = m.inputArea.Update(msg)
	return m, cmd
}

func (m teaModel) handleAgentEvent(ev agent.AgentEvent) (teaModel, tea.Cmd) {
	switch ev.Type {
	case agent.EventMessageUpdate:
		if ev.Update.Thinking != "" {
			m.streamingThinking += ev.Update.Thinking
		}
		if ev.Update.Text != "" {
			// 第一个正文块到达 = 思考阶段结束，把思考收尾进 chatLines
			if m.streamingThinking != "" {
				m.chatLines = append(m.chatLines, renderThinking(m.streamingThinking, m.width)...)
				m.streamingThinking = ""
			}
			if m.stream == nil {
				m.stream = &streamingMarkdown{}
			}
			m.streaming += ev.Update.Text
		}
	case agent.EventMessageEnd:
		m = m.finalizeStreaming() // 正文定稿进 chatLines
	case agent.EventToolStart:
		m = m.finalizeStreaming() // 工具调用前收尾正文（若还有流式残留）
		m.chatLines = append(m.chatLines, renderToolCall(ev.ToolStart))
	case agent.EventToolEnd:
		m.chatLines = append(m.chatLines, renderToolResult(ev.ToolEnd)...)
	case agent.EventCompaction:
		m = m.finalizeStreaming()
		m.chatLines = append(m.chatLines, dimStyle.Render("── 上下文已压缩（"+ev.Compaction.Reason+"）──"))
	case agent.EventRetry:
		m.chatLines = append(m.chatLines, dimStyle.Render(fmt.Sprintf("⟳ 模型错误，%v 后重试（%d/3）：%v", ev.Retry.Delay, ev.Retry.Attempt, ev.Retry.Err)))
	case agent.EventTerminated:
		m.chatLines = append(m.chatLines, dimStyle.Render("  ✓ "+ev.Terminated.ToolName))
	case agent.EventError:
		m.chatLines = append(m.chatLines, renderError(ev.Err))
	}
	return m, nil
}

func (m teaModel) View() tea.View {
	chatHeight := max(1, m.height-4)
	var hubLines []string
	if m.hubOpen {
		hubLines = renderHub(m.roster(), m.hubSel, m.width, max(3, m.height/3))
		chatHeight = max(1, chatHeight-len(hubLines)-1)
	}

	var all []string
	all = append(all, m.chatLines...)
	if m.streamingThinking != "" {
		all = append(all, renderThinking(m.streamingThinking, m.width)...)
	}
	if m.streaming != "" {
		var lines []string
		if m.stream != nil {
			lines = m.stream.Render(m.streaming, m.width)
		} else {
			lines = renderMarkdown(m.streaming, m.width)
		}
		if len(lines) > 0 {
			lines[0] = aiPrefix + lines[0]
		}
		all = append(all, lines...)
	}
	// 虚拟滚动：只渲染可见窗口（start..end），scrollOffset 控制上滚量
	start := max(len(all)-chatHeight-m.scrollOffset, 0)
	end := min(start+chatHeight, len(all))
	all = all[start:end]
	for len(all) < chatHeight {
		all = append(all, "")
	}

	chatView := strings.Join(all, "\n")
	bottom := m.inputArea.View()
	if m.pendingApproval != nil {
		bottom = renderApprovalDialog(m.pendingApproval.call)
	}
	parts := []string{chatView, ""}
	if len(hubLines) > 0 {
		parts = append(parts, strings.Join(hubLines, "\n"), "")
	}
	content := lipgloss.JoinVertical(lipgloss.Top, append(parts, bottom)...)
	return tea.View{Content: content, AltScreen: true, MouseMode: tea.MouseModeCellMotion}
}

// roster 返回子 agent 名册快照（未装配 Manager 时为空）。
func (m teaModel) roster() []subagent.RunView {
	if m.sub == nil {
		return nil
	}
	return m.sub.RosterFor(m.session.Header().ID)
}

func (m teaModel) handleKey(msg tea.KeyPressMsg) (teaModel, tea.Cmd) {
	// 审批弹窗是 modal：待审批时只响应 y/a/n/esc
	if m.pendingApproval != nil {
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("y", "enter"))):
			m.pendingApproval.resp <- approvalAnswer{allow: true}
			m.pendingApproval = nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("a"))):
			m.pendingApproval.resp <- approvalAnswer{allow: true, sessionAllow: true}
			m.pendingApproval = nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("n", "esc"))):
			m.pendingApproval.resp <- approvalAnswer{allow: false}
			m.pendingApproval = nil
		}
		return m, nil
	}

	// Agent Hub 打开时接管导航键；输入仍然可打字（j/k 只在输入框为空时当导航用）
	if m.hubOpen {
		empty := strings.TrimSpace(m.inputArea.Value()) == ""
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
			m.hubOpen = false
			return m, nil
		case empty && key.Matches(msg, key.NewBinding(key.WithKeys("j", "down"))):
			m.hubSel = min(m.hubSel+1, max(len(m.roster())-1, 0))
			return m, nil
		case empty && key.Matches(msg, key.NewBinding(key.WithKeys("k", "up"))):
			m.hubSel = max(m.hubSel-1, 0)
			return m, nil
		case empty && key.Matches(msg, key.NewBinding(key.WithKeys("x"))):
			rows := m.roster()
			if m.sub != nil && m.hubSel < len(rows) {
				if n := m.sub.Cancel([]string{rows[m.hubSel].Name}); n > 0 {
					m.chatLines = append(m.chatLines, dimStyle.Render("已终止子 agent "+rows[m.hubSel].Name))
				}
			}
			return m, nil
		}
	}

	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+a"))):
		m.hubOpen = !m.hubOpen
		m.hubSel = 0
		return m, nil

	case key.Matches(msg, key.NewBinding(key.WithKeys("pgup"))):
		m.scrollOffset += max(1, m.height/2) // 上滚半屏
		return m, nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("pgdown"))):
		m.scrollOffset -= max(1, m.height/2) // 下滚半屏
		if m.scrollOffset < 0 {
			m.scrollOffset = 0
		}
		return m, nil

	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+e"))):
		// steering：注入当前输入作为修正，不取消当前 run
		if t := strings.TrimSpace(m.inputArea.Value()); t != "" && trySteer(message.NewUserMessage(t)) {
			m.inputArea.Reset()
		}
		return m, nil

	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+c"))):
		cancelCurrent() // 停当前流
		return m, tea.Quit

	case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
		text := strings.TrimSpace(m.inputArea.Value())
		if text == "" {
			return m, nil
		}
		if handled, nm := m.handleSlash(text); handled {
			return nm, nil
		}
		m.inputArea.Reset()
		m = m.finalizeStreaming() // 收尾当前 AI 消息(若有)
		// 追加用户消息行(首行加前缀)
		userLines := strings.Split(text, "\n")
		userLines[0] = userPrefix + userLines[0]
		m.chatLines = append(m.chatLines, userLines...)
		m.scrollOffset = 0 // 发新消息跳到底部

		// 取消上一轮，清掉可能残留的审批弹窗，起新一轮 agent run
		m.pendingApproval = nil
		m.startRun(text)
		return m, nil
	}
	var cmd tea.Cmd
	m.inputArea, cmd = m.inputArea.Update(msg)
	return m, cmd
}

// handleSlash 处理斜杠命令；返回是否已处理。
func (m teaModel) handleSlash(text string) (bool, teaModel) {
	switch {
	case strings.HasPrefix(text, "/skill:"):
		m.inputArea.Reset()
		if !m.skillCommands || m.skillCatalog == nil {
			m.chatLines = append(m.chatLines, dimStyle.Render("Skills 命令未启用"))
			return true, m
		}
		name, args, ok := skills.ParseInvocation(text)
		if !ok {
			m.chatLines = append(m.chatLines, dimStyle.Render("用法：/skill:<name> [args]"))
			return true, m
		}
		expanded, skill, err := m.skillCatalog.BuildPrompt(name, args, skills.InvocationUser)
		if err != nil {
			m.chatLines = append(m.chatLines, renderError(err))
			return true, m
		}
		m = m.finalizeStreaming()
		m.chatLines = append(m.chatLines, userPrefix+text)
		m.scrollOffset = 0
		m.pendingApproval = nil
		_ = m.session.AppendCustom("skill_invocation", map[string]any{
			"name": skill.Name, "path": skill.FilePath, "args": args, "source": "user",
		})
		m.startRun(expanded)
		return true, m
	case text == "/skills" || text == "/skills list":
		m.inputArea.Reset()
		if !m.skillCommands || m.skillCatalog == nil {
			m.chatLines = append(m.chatLines, dimStyle.Render("Skills 命令未启用"))
			return true, m
		}
		list := m.skillCatalog.List()
		if len(list) == 0 {
			m.chatLines = append(m.chatLines, dimStyle.Render("没有发现 skill"))
			return true, m
		}
		m.chatLines = append(m.chatLines, fmt.Sprintf("Skills（%d）：", len(list)))
		for _, skill := range list {
			flags := ""
			if skill.DisableModelInvocation {
				flags += " [仅显式]"
			}
			if !skill.UserInvocable {
				flags += " [仅模型]"
			}
			m.chatLines = append(m.chatLines, fmt.Sprintf("  %s%s — %s", skill.Name, flags, skill.Description))
		}
		return true, m
	case strings.HasPrefix(text, "/skills show "):
		m.inputArea.Reset()
		if !m.skillCommands || m.skillCatalog == nil {
			m.chatLines = append(m.chatLines, dimStyle.Render("Skills 命令未启用"))
			return true, m
		}
		name := strings.TrimSpace(strings.TrimPrefix(text, "/skills show "))
		skill, ok := m.skillCatalog.Get(name)
		if !ok {
			m.chatLines = append(m.chatLines, renderError(fmt.Errorf("未知 skill %q；运行 /skills 查看可用项", name)))
			return true, m
		}
		m.chatLines = append(m.chatLines,
			"Skill: "+skill.Name,
			"  描述: "+skill.Description,
			"  来源: "+skill.Source.Label,
			"  路径: "+skill.FilePath,
			fmt.Sprintf("  调用: model=%t user=%t", !skill.DisableModelInvocation, skill.UserInvocable),
		)
		if len(skill.AllowedTools) > 0 {
			m.chatLines = append(m.chatLines, "  建议工具: "+strings.Join(skill.AllowedTools, ", "))
		}
		return true, m
	case text == "/skills reload":
		m.inputArea.Reset()
		if !m.skillCommands || m.skillCatalog == nil {
			m.chatLines = append(m.chatLines, dimStyle.Render("Skills 命令未启用"))
			return true, m
		}
		warnings, err := m.skillCatalog.Reload()
		if err != nil {
			m.chatLines = append(m.chatLines, renderError(err))
			return true, m
		}
		m.cmgr.InvalidateSystem()
		m.chatLines = append(m.chatLines, fmt.Sprintf("已重新加载 %d 个 skills（%d 条告警）", len(m.skillCatalog.List()), len(warnings)))
		for _, warning := range warnings {
			where := warning.Path
			if where != "" {
				where += ": "
			}
			m.chatLines = append(m.chatLines, dimStyle.Render("  ⚠ "+where+warning.Message))
		}
		return true, m
	case strings.HasPrefix(text, "/skills"):
		m.inputArea.Reset()
		m.chatLines = append(m.chatLines, dimStyle.Render("用法：/skills [list|show <name>|reload]；调用：/skill:<name> [args]"))
		return true, m
	case text == "/clear":
		_ = m.session.Reset()
		m.agent.Registry().ResetConv() // reset_boundary 封存旧上下文，已读记录随之失效
		m.chatLines = nil
		m.inputArea.Reset()
		return true, m
	case text == "/forget":
		if m.mem != nil {
			_ = m.mem.Clear()
		}
		m.chatLines = append(m.chatLines, "已清空本项目的长期记忆")
		m.inputArea.Reset()
		return true, m
	case text == "/new":
		cancelCurrent()
		m.inputArea.Reset()
		sw := sessionSwitch{new: true}
		if runActive() {
			m.pendingSwitch = &sw
			m.chatLines = append(m.chatLines, dimStyle.Render("正在结束当前轮，随后新建会话…"))
			return true, m
		}
		m = m.applySessionSwitch(sw)
		return true, m
	case text == "/sessions":
		infos, err := m.mgr.List()
		if err != nil {
			m.chatLines = append(m.chatLines, renderError(err))
			return true, m
		}
		m.chatLines = append(m.chatLines, "会话列表（/resume <id前缀> 切换）：")
		for _, in := range infos {
			mark := "  "
			if in.ID == m.session.Header().ID {
				mark = "* "
			}
			m.chatLines = append(m.chatLines, fmt.Sprintf("%s%s  %s  %s", mark, in.ID, dimStyle.Render(in.ModTime.Format("01-02 15:04")), in.Label()))
		}
		m.inputArea.Reset()
		return true, m
	case strings.HasPrefix(text, "/resume "):
		id := strings.TrimSpace(strings.TrimPrefix(text, "/resume "))
		cancelCurrent()
		m.inputArea.Reset()
		sw := sessionSwitch{id: id}
		if runActive() {
			m.pendingSwitch = &sw
			m.chatLines = append(m.chatLines, dimStyle.Render("正在结束当前轮，随后切换会话…"))
			return true, m
		}
		m = m.applySessionSwitch(sw)
		return true, m
	case text == "/agents":
		rows := m.roster()
		if len(rows) == 0 {
			m.chatLines = append(m.chatLines, dimStyle.Render("还没有派发过子 agent（ctrl+a 可随时打开 Agent Hub）"))
		} else {
			m.chatLines = append(m.chatLines, renderHub(rows, -1, m.width, 0)...)
		}
		m.inputArea.Reset()
		return true, m
	case strings.HasPrefix(text, "/agent "):
		name, body, ok := parseAgentCommand(text)
		switch {
		case !ok:
			m.chatLines = append(m.chatLines, dimStyle.Render("用法：/agent <子agent名> <要说的话>"))
		case m.sub == nil:
			m.chatLines = append(m.chatLines, dimStyle.Render("本实例未装配子 agent"))
		default:
			receipt, err := m.sub.Send("Main", name, body)
			if err != nil {
				m.chatLines = append(m.chatLines, renderError(err))
			} else {
				m.chatLines = append(m.chatLines, dimStyle.Render("→ "+name+"："+body+"（"+receipt+"）"))
			}
		}
		m.inputArea.Reset()
		return true, m
	case strings.HasPrefix(text, "/title "):
		title := strings.TrimSpace(strings.TrimPrefix(text, "/title "))
		if title != "" {
			_ = m.session.SetTitle(title)
			m.chatLines = append(m.chatLines, dimStyle.Render("标题已设为："+title))
		}
		m.inputArea.Reset()
		return true, m
	}
	return false, m
}

// startRun 取消上一轮并起一轮新的 agent run（用户输入与 auto-continue 共用这条路径）。
func (m teaModel) startRun(text string) {
	m.launchRun(text, true)
}

func (m teaModel) startRecordedRun() {
	m.launchRun("", false)
}

func (m teaModel) launchRun(text string, record bool) {
	cancelCurrent()
	ctx, cancel := context.WithCancel(context.Background())
	steer := make(chan message.Message, 8)
	id := setCurrentRun(cancel, steer)
	go m.runAgent(ctx, text, steer, record, id)
}

// deliverPending 把已结算的后台作业结果与发给 Main 的消息交给主 agent：
// 活动 run 结束后再投递；先持久化通知，成功后 ACK，再自动起一轮处理。
func (m teaModel) deliverPending() teaModel {
	if m.sub == nil || runActive() || m.pendingSwitch != nil {
		return m
	}
	sessionID := m.session.Header().ID
	jobs, mails := m.sub.PeekSettled(subagent.MainName, sessionID), m.sub.PeekMainInbox(sessionID)
	if len(jobs) == 0 && len(mails) == 0 {
		return m
	}
	m = m.finalizeStreaming()
	for _, j := range jobs {
		m.chatLines = append(m.chatLines, dimStyle.Render(fmt.Sprintf("── 后台作业完成：%s [%s] ──",
			j.JobID, subagent.StatusString(j.Result.Status))))
	}
	for _, ml := range mails {
		m.chatLines = append(m.chatLines, dimStyle.Render("← "+ml.From+"："+ml.Text))
	}
	notice := subagent.RenderAsyncResult(jobs, mails)
	if err := m.cmgr.Record(message.NewUserMessage(notice), model.Usage{}); err != nil {
		m.chatLines = append(m.chatLines, renderError(fmt.Errorf("后台结果写入会话失败（保留待重试）：%w", err)))
		return m
	}
	jobIDs := make([]string, 0, len(jobs))
	for _, job := range jobs {
		jobIDs = append(jobIDs, job.DeliveryID)
	}
	mailIDs := make([]string, 0, len(mails))
	for _, mail := range mails {
		mailIDs = append(mailIDs, mail.DeliveryID)
	}
	m.sub.AckSettled(jobIDs)
	m.sub.AckMainInbox(mailIDs)
	m.chatLines = append(m.chatLines, dimStyle.Render("（主 agent 空闲，自动继续处理上面的结果）"))
	m.scrollOffset = 0
	m.startRecordedRun()
	return m
}

// runAgent 在后台 goroutine 跑 agent：记录用户消息 → 跑循环（循环内记录 assistant/tool）。
func (m teaModel) runAgent(ctx context.Context, text string, steer chan message.Message, record bool, id uint64) {
	runMu.Lock() // 等上一个 run 结束（cancel 后它会快速退出），避免新旧两轮并发写 session
	recorded := true
	if record {
		if err := m.cmgr.Record(message.NewUserMessage(text), model.Usage{}); err != nil {
			recorded = false
			if program != nil {
				program.Send(agent.AgentEvent{Type: agent.EventError, Err: err})
			}
		}
	}
	if recorded && ctx.Err() == nil {
		for ev := range m.agent.Run(ctx, steer) {
			if program != nil {
				program.Send(ev)
			}
		}
	}
	runMu.Unlock()
	clearCurrentRun(id)
	if program != nil {
		program.Send(runFinishedMsg{id: id})
	}
}

func (m teaModel) applySessionSwitch(sw sessionSwitch) teaModel {
	m.agent.Registry().ResetConv()
	var ns *session.Session
	var err error
	if sw.new {
		ns, err = m.mgr.New(m.cwd)
	} else {
		ns, err = m.mgr.Switch(sw.id)
	}
	if err != nil {
		m.chatLines = append(m.chatLines, renderError(err))
		return m
	}
	artifactDir, err := m.mgr.ArtifactDir(ns)
	if err != nil {
		_ = ns.Close()
		m.chatLines = append(m.chatLines, renderError(err))
		return m
	}
	m.session.Close()
	m.session = ns
	m.cmgr.SetSession(ns)
	if m.sub != nil {
		m.sub.SetMainSession(ns.Header().ID, artifactDir)
	}
	m.chatLines = nil
	if !sw.new {
		if msgs, replayErr := ns.Replay(); replayErr == nil {
			m.chatLines = renderHistory(msgs)
		}
	}
	return m.deliverPending()
}

// finalizeStreaming 把当前流式 AI 消息(思考+正文)收尾进 chatLines，并重置流式状态。
func (m teaModel) finalizeStreaming() teaModel {
	if m.streamingThinking != "" {
		m.chatLines = append(m.chatLines, renderThinking(m.streamingThinking, m.width)...)
		m.streamingThinking = ""
	}
	if m.streaming != "" {
		m.chatLines = append(m.chatLines, renderAIMessage(m.streaming, m.width)...)
		m.streaming = ""
		m.stream = nil
	}
	return m
}

// renderAIMessage 把 AI 正文 Markdown 渲染成行，首行加 ● 前缀。
func renderAIMessage(text string, width int) []string {
	md := renderMarkdown(text, width)
	if len(md) > 0 {
		md[0] = aiPrefix + md[0]
	}
	return md
}

// renderError 渲染一行错误提示。
func renderError(err error) string {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true).Render("✗ " + err.Error())
}

// renderToolCall 渲染一行工具调用（名 + 参数）。
func renderToolCall(ts *agent.ToolStart) string {
	name := lipgloss.NewStyle().Foreground(lipgloss.Color("208")).Bold(true).Render(ts.Name)
	args := lipgloss.NewStyle().Foreground(lipgloss.Color("250")).Render(ts.Args)
	return "  " + name + " " + args
}

// renderApprovalDialog 渲染审批弹窗（子 agent 升级审批时 call.Name 带 [子 agent X] 标签）。
func renderApprovalDialog(call message.ToolCall) string {
	title := lipgloss.NewStyle().Foreground(lipgloss.Color("208")).Bold(true).Render("⚠ 审批")
	cmd := lipgloss.NewStyle().Foreground(lipgloss.Color("250")).Render(call.Name + " " + call.Args)
	return fmt.Sprintf("%s\n\n  %s\n\n  [y] 允许   [a] 本会话允许   [n] 拒绝", title, cmd)
}

// renderToolResult 渲染工具结果（头部 + 内容行，超长截断预览）。
func renderToolResult(te *agent.ToolEnd) []string {
	mark := "← "
	if te.IsError {
		mark = "✗ "
	}
	head := lipgloss.NewStyle().Foreground(lipgloss.Color("250")).Bold(true).Render(mark + te.Name)
	lines := strings.Split(te.Content, "\n")
	const maxLines = 10
	out := []string{"  " + head}
	for i, l := range lines {
		if i >= maxLines {
			out = append(out, fmt.Sprintf("  ...(%d more lines)", len(lines)-maxLines))
			break
		}
		out = append(out, "    "+l)
	}
	return out
}

// renderHistory 把历史消息渲染成终端行（user 加 ┃ 前缀，assistant 加 ● 前缀）。
func renderHistory(msgs []message.Message) []string {
	var out []string
	for _, m := range msgs {
		text := messageText(m)
		if text == "" {
			continue
		}
		lines := strings.Split(text, "\n")
		switch m.Role {
		case message.RoleUser:
			lines[0] = userPrefix + lines[0]
			out = append(out, lines...)
		case message.RoleAssistant:
			out = append(out, aiPrefix+lines[0])
			out = append(out, lines[1:]...)
		}
	}
	return out
}

// messageText 拼接消息里所有文本块。
func messageText(m message.Message) string {
	var sb strings.Builder
	for _, b := range m.Blocks {
		if b.Kind == message.BlockText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}
