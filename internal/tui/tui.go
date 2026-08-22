package tui

import (
	"context"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"einoclaw-build/internal/agent"
	agentctx "einoclaw-build/internal/context"
	"einoclaw-build/internal/message"
	"einoclaw-build/internal/session"
)

// P1：从阶段5的 tui.go 重建 + 改造。
// 事件源从「eino 的 OnAgentEvents 消息」换成「我们自己的 agent.AgentEvent」；
// 移除 eino 依赖与工具渲染（工具展示 P4 加）。

var (
	userPrefix = lipgloss.NewStyle().Foreground(lipgloss.Color("63")).Bold(true).Render("┃ ")
	aiPrefix   = lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Render("● ")

	// 双协程桥接用（沿用旧「全局 program + program.Send」模式）
	program       *tea.Program
	currentCancel context.CancelFunc
)

// SetProgram 注入 BubbleTea program，供后台 goroutine 把事件塞回 TUI 主循环。
func SetProgram(p *tea.Program) { program = p }

type teaModel struct {
	width            int
	height           int
	chatLines        []string // 已完成的终端行(已渲染、已带前缀)
	streaming        string   // 当前流式 AI 正文(Markdown 原文)
	stream           *streamingMarkdown
	streamingThinking string // 当前流式思考(原文)
	inputArea        textarea.Model
	agent            *agent.Agent
	session          *session.Session
	cmgr             *agentctx.ContextManager
	pendingApproval  *approvalRequestMsg // nil = 无待审批
}

func NewModel(ag *agent.Agent, s *session.Session, cmgr *agentctx.ContextManager) teaModel {
	ta := textarea.New()
	ta.Placeholder = " Type your message... (Enter=send, Ctrl+J=newline, Ctrl+C=quit)"
	ta.SetHeight(3)
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("ctrl+j"), key.WithHelp("ctrl+j", "newline"))
	ta.Focus()
	m := teaModel{inputArea: ta, agent: ag, session: s, cmgr: cmgr}
	// 恢复历史：replay 后渲染进聊天区
	if msgs, err := s.Replay(); err == nil {
		m.chatLines = renderHistory(msgs)
	}
	return m
}

func (m teaModel) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, tea.RequestWindowSize)
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
	case agent.EventError:
		m.chatLines = append(m.chatLines, renderError(ev.Err))
	}
	return m, nil
}

func (m teaModel) View() tea.View {
	chatHeight := max(1, m.height-4)

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
	if len(all) > chatHeight {
		all = all[len(all)-chatHeight:]
	}
	for len(all) < chatHeight {
		all = append(all, "")
	}

	chatView := strings.Join(all, "\n")
	bottom := m.inputArea.View()
	if m.pendingApproval != nil {
		bottom = renderApprovalDialog(m.pendingApproval.call)
	}
	content := lipgloss.JoinVertical(lipgloss.Top, chatView, "", bottom)
	return tea.View{Content: content, AltScreen: true}
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

	switch {
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
		if text == "/clear" {
			_ = m.session.Reset()
			m.chatLines = nil
			m.inputArea.Reset()
			return m, nil
		}
		m.inputArea.Reset()
		m = m.finalizeStreaming() // 收尾当前 AI 消息(若有)
		// 追加用户消息行(首行加前缀)
		userLines := strings.Split(text, "\n")
		userLines[0] = userPrefix + userLines[0]
		m.chatLines = append(m.chatLines, userLines...)

		// 取消上一轮，起新一轮 agent run
		if currentCancel != nil {
			currentCancel()
		}
		ctx, cancel := context.WithCancel(context.Background())
		currentCancel = cancel
		go m.runAgent(ctx, text)
		return m, nil
	}
	var cmd tea.Cmd
	m.inputArea, cmd = m.inputArea.Update(msg)
	return m, cmd
}

// runAgent 在后台 goroutine 跑 agent：记录 user → 跑 agent → 记录 assistant。
func (m teaModel) runAgent(ctx context.Context, text string) {
	userMsg := message.NewUserMessage(text)

	// 1. 加载历史（到最后一个 reset_boundary）
	history, err := m.session.Replay()
	if err != nil {
		history = nil
	}
	// 2. 记录用户消息
	_ = m.session.Append(userMsg)
	// 3. 跑 agent：输入 = 历史 + 用户消息
	input := append(history, userMsg)
	for ev := range m.agent.Run(ctx, input) {
		if program != nil {
			program.Send(ev)
		}
		switch ev.Type {
		case agent.EventMessageEnd:
			// 4. 定稿后记录 assistant 消息
			_ = m.session.Append(ev.Ended.Message)
			_ = m.cmgr.AfterTurn(ctx, ev.Ended.Usage) // P3：超阈值则压缩
		case agent.EventToolEnd:
			// 记录工具结果，否则 replay 时 tool_calls 缺配对 tool 消息，API 报 insufficient tool messages
			_ = m.session.Append(message.NewToolMessage(ev.ToolEnd.ID, ev.ToolEnd.Name, ev.ToolEnd.Content, false))
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

// renderApprovalDialog 渲染审批弹窗。
func renderApprovalDialog(call message.ToolCall) string {
	title := lipgloss.NewStyle().Foreground(lipgloss.Color("208")).Bold(true).Render("⚠ 审批")
	cmd := lipgloss.NewStyle().Foreground(lipgloss.Color("250")).Render(call.Name + " " + call.Args)
	return fmt.Sprintf("%s\n\n  %s\n\n  [y] 允许   [n] 拒绝", title, cmd)
}

// renderToolResult 渲染工具结果（头部 + 内容行，超长截断预览）。
func renderToolResult(te *agent.ToolEnd) []string {
	head := lipgloss.NewStyle().Foreground(lipgloss.Color("250")).Bold(true).Render("← " + te.Name)
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
		lines := strings.Split(messageText(m), "\n")
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
