package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"

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
	runMu         sync.Mutex           // 保证同一时刻只有一个 run（防双 run 竞态）
)

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
}

// NewModel 构造 TUI 模型；cmgr 持有当前会话，cwd 用于新建会话，sub/b 提供 Agent Hub。
func NewModel(ag *agent.Agent, mgr *session.Manager, cmgr *agentctx.Manager, mem *memory.Store, cwd string, sub *subagent.Manager, b *bus.Bus) teaModel {
	ta := textarea.New()
	ta.Placeholder = " Type your message... (Enter=send, Ctrl+J=newline, Ctrl+E=steer, Ctrl+C=quit)"
	ta.SetHeight(3)
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("ctrl+j"), key.WithHelp("ctrl+j", "newline"))
	ta.Focus()
	s := cmgr.Session()
	m := teaModel{inputArea: ta, agent: ag, session: s, mgr: mgr, cmgr: cmgr, mem: mem, cwd: cwd,
		sub: sub, hubCh: mergeHubEvents(b)}
	// 恢复历史：replay 后渲染进聊天区
	if msgs, err := s.Replay(); err == nil {
		m.chatLines = renderHistory(msgs)
	}
	return m
}

func (m teaModel) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, tea.RequestWindowSize, waitHubEvent(m.hubCh))
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
		return m, waitHubEvent(m.hubCh) // 名册在渲染时读，这里只需重新挂等待

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
	return m.sub.Roster()
}

func (m teaModel) handleKey(msg tea.KeyPressMsg) (teaModel, tea.Cmd) {
	// 审批弹窗是 modal：待审批时只响应 y/n/esc
	if m.pendingApproval != nil {
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("y", "enter"))):
			m.pendingApproval.resp <- true
			m.pendingApproval = nil
		case key.Matches(msg, key.NewBinding(key.WithKeys("n", "esc"))):
			m.pendingApproval.resp <- false
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
		if currentSteer != nil {
			if t := strings.TrimSpace(m.inputArea.Value()); t != "" {
				currentSteer <- message.NewUserMessage(t)
				m.inputArea.Reset()
			}
		}
		return m, nil

	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+c"))):
		if currentCancel != nil {
			currentCancel() // 停当前流
		}
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
		if currentCancel != nil {
			currentCancel()
		}
		m.pendingApproval = nil
		ctx, cancel := context.WithCancel(context.Background())
		currentCancel = cancel
		steer := make(chan message.Message, 8)
		currentSteer = steer
		go m.runAgent(ctx, text, steer)
		return m, nil
	}
	var cmd tea.Cmd
	m.inputArea, cmd = m.inputArea.Update(msg)
	return m, cmd
}

// handleSlash 处理斜杠命令；返回是否已处理。
func (m teaModel) handleSlash(text string) (bool, teaModel) {
	switch {
	case text == "/clear":
		_ = m.session.Reset()
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
		if currentCancel != nil {
			currentCancel()
		}
		ns, err := m.mgr.New(m.cwd)
		if err != nil {
			m.chatLines = append(m.chatLines, renderError(err))
			return true, m
		}
		m.session.Close()
		m.session = ns
		m.cmgr.SetSession(ns)
		m.chatLines = nil
		m.inputArea.Reset()
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
		if currentCancel != nil {
			currentCancel()
		}
		ns, err := m.mgr.Switch(id)
		if err != nil {
			m.chatLines = append(m.chatLines, renderError(err))
			m.inputArea.Reset()
			return true, m
		}
		m.session.Close()
		m.session = ns
		m.cmgr.SetSession(ns)
		m.chatLines = nil
		if msgs, err := ns.Replay(); err == nil {
			m.chatLines = renderHistory(msgs)
		}
		m.inputArea.Reset()
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
			if err := m.sub.Send("Main", name, body); err != nil {
				m.chatLines = append(m.chatLines, renderError(err))
			} else {
				m.chatLines = append(m.chatLines, dimStyle.Render("→ "+name+"："+body))
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

// runAgent 在后台 goroutine 跑 agent：记录用户消息 → 跑循环（循环内记录 assistant/tool）。
func (m teaModel) runAgent(ctx context.Context, text string, steer chan message.Message) {
	runMu.Lock() // 等上一个 run 结束（cancel 后它会快速退出），避免新旧两轮并发写 session
	defer runMu.Unlock()
	defer func() { currentSteer = nil }()

	_ = m.cmgr.Record(message.NewUserMessage(text), model.Usage{})
	for ev := range m.agent.Run(ctx, steer) {
		if program != nil {
			program.Send(ev)
		}
	}
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
	return fmt.Sprintf("%s\n\n  %s\n\n  [y] 允许   [n] 拒绝", title, cmd)
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
