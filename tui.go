package main

import (
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/cloudwego/eino/adk"
)

// 阶段3 最小 TUI：只打通"输入 -> Push -> program.Send -> 显示"的闭环。
// 暂不做：Markdown 渲染(阶段4)、工具调用展示(阶段5)、虚拟滚动/展开折叠(阶段8)。

var (
	// 用户消息前缀(紫色粗体)、AI 消息前缀(青色)，和原项目保持一致
	userPrefix = lipgloss.NewStyle().Foreground(lipgloss.Color("63")).Bold(true).Render("┃ ")
	aiPrefix   = lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Render("● ")
)

// teaModel 是 BubbleTea 的 Model，持有全部 UI 状态。
type teaModel struct {
	width     int
	height    int
	chatLines []string // 已完成的行(已含前缀)
	streaming string   // 当前流式 AI 文本(不含前缀，View 时再拼)
	inputArea textarea.Model
}

func newTeaModel() teaModel {
	ta := textarea.New()
	ta.Placeholder = " Type your message... (Enter=send, Ctrl+J=newline, Ctrl+C=quit)"
	ta.SetHeight(3)
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	// 把"换行"从 Enter 改到 Ctrl+J，腾出 Enter 给"发送"
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("ctrl+j"), key.WithHelp("ctrl+j", "newline"))
	ta.Focus()
	return teaModel{inputArea: ta}
}

// Init 返回启动时要执行的 Cmd：光标闪烁 + 请求窗口尺寸。
func (m teaModel) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, tea.RequestWindowSize)
}

// Update 是事件分发中枢。注意：值接收器，返回"新的" model(不可变更新风格)。
func (m teaModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.inputArea.SetWidth(max(1, m.width-2))
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case aiTextChunkMsg:
		// 收到后台发来的流式文本块，追加到当前 AI 行
		m.streaming += msg.text
		return m, nil
	}
	// 其余消息(光标闪烁等)交给 textarea
	var cmd tea.Cmd
	m.inputArea, cmd = m.inputArea.Update(msg)
	return m, cmd
}

// View 把 model 渲染成终端画面。
func (m teaModel) View() tea.View {
	chatHeight := max(1, m.height-4) // 输入框 3 行 + 1 空行分隔

	// 拼成整段文本，再按真实换行切分(正确处理流式文本里的多段落)
	var all []string
	all = append(all, m.chatLines...)
	if m.streaming != "" {
		all = append(all, aiPrefix+m.streaming)
	}
	ls := strings.Split(strings.Join(all, "\n"), "\n")

	// 简版滚动：只显示能放下的最后几行(真正的虚拟滚动在阶段8)
	if len(ls) > chatHeight {
		ls = ls[len(ls)-chatHeight:]
	}
	for len(ls) < chatHeight {
		ls = append(ls, "") // 填充空行，让输入框位置不跳动
	}

	chatView := strings.Join(ls, "\n")
	content := lipgloss.JoinVertical(lipgloss.Top, chatView, "", m.inputArea.View())
	return tea.View{Content: content, AltScreen: true}
}

// handleKey 处理按键：Ctrl+C 退出、Enter 发送、其余交给 textarea。
func (m teaModel) handleKey(msg tea.KeyPressMsg) (teaModel, tea.Cmd) {
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+c"))):
		turnLoop.Stop(adk.WithImmediate())
		turnLoop.Wait()
		return m, tea.Quit

	case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
		text := strings.TrimSpace(m.inputArea.Value())
		if text == "" {
			return m, nil
		}
		m.inputArea.Reset()
		// 先收尾当前流式 AI 行(若有)
		if m.streaming != "" {
			m.chatLines = append(m.chatLines, aiPrefix+m.streaming)
			m.streaming = ""
		}
		// 追加用户消息行
		m.chatLines = append(m.chatLines, userPrefix+text)
		// Push 到 TurnLoop(非阻塞)。WithPreempt 让新消息可打断正在输出的旧 turn，
		// 这样"AI 还在流式时用户发送"不会把旧 turn 的尾巴接错位置。
		turnLoop.Push(chatItem{query: text}, adk.WithPreempt[chatItem, adk.AgenticMessage](adk.AnySafePoint))
		return m, nil
	}

	// 其余按键(打字、光标移动、Ctrl+J 换行等)交给 textarea
	var cmd tea.Cmd
	m.inputArea, cmd = m.inputArea.Update(msg)
	return m, cmd
}
