package main

import (
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/cloudwego/eino/adk"
)

// 阶段4：在阶段3 基础上，把流式正文用 glamour 渲染成 Markdown，
// 并把思考过程(Reasoning)以灰色块显示。
// 仍是简版滚动(只显示末尾能放下的行)；虚拟滚动/折叠在阶段8。

var (
	userPrefix = lipgloss.NewStyle().Foreground(lipgloss.Color("63")).Bold(true).Render("┃ ")
	aiPrefix   = lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Render("● ")
)

type teaModel struct {
	width            int
	height           int
	chatLines        []string // 已完成的终端行(已渲染、已带前缀)
	streaming        string   // 当前流式 AI 正文(Markdown 原文)
	stream           *streamingMarkdown // 当前流式消息的增量渲染器(缓存稳定前缀)
	streamingThinking string   // 当前流式思考(原文)；第一个正文块到达后即收尾进 chatLines
	inputArea        textarea.Model
}

func newTeaModel() teaModel {
	ta := textarea.New()
	ta.Placeholder = " Type your message... (Enter=send, Ctrl+J=newline, Ctrl+C=quit)"
	ta.SetHeight(3)
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("ctrl+j"), key.WithHelp("ctrl+j", "newline"))
	ta.Focus()
	return teaModel{inputArea: ta}
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

	case aiThinkingChunkMsg:
		m.streamingThinking += msg.text
		return m, nil

	case aiTextChunkMsg:
		// 第一个正文块到达 = 思考阶段结束，把思考收尾进 chatLines(之后只重渲染正文)
		if m.streamingThinking != "" {
			m.chatLines = append(m.chatLines, renderThinking(m.streamingThinking, m.width)...)
			m.streamingThinking = ""
		}
		if m.stream == nil {
			m.stream = &streamingMarkdown{} // 新的一条 AI 消息，新建增量渲染器
		}
		m.streaming += msg.text
		return m, nil
	}
	// 光标闪烁等其余消息交给 textarea
	var cmd tea.Cmd
	m.inputArea, cmd = m.inputArea.Update(msg)
	return m, cmd
}

func (m teaModel) View() tea.View {
	chatHeight := max(1, m.height-4) // 输入框 3 行 + 1 空行分隔

	var all []string
	all = append(all, m.chatLines...)
	if m.streamingThinking != "" {
		all = append(all, renderThinking(m.streamingThinking, m.width)...)
	}
	if m.streaming != "" {
		var lines []string
		if m.stream != nil {
			lines = m.stream.Render(m.streaming, m.width) // 增量渲染：复用缓存前缀，只重渲染尾部
		} else {
			lines = renderMarkdown(m.streaming, m.width) // 兜底全量渲染
		}
		if len(lines) > 0 {
			lines[0] = aiPrefix + lines[0] // 给首行加 ● 前缀
		}
		all = append(all, lines...)
	}
	// 简版滚动：只显示能放下的最后几行(真正的虚拟滚动在阶段8)
	if len(all) > chatHeight {
		all = all[len(all)-chatHeight:]
	}
	for len(all) < chatHeight {
		all = append(all, "") // 填充，让输入框位置稳定
	}

	chatView := strings.Join(all, "\n")
	content := lipgloss.JoinVertical(lipgloss.Top, chatView, "", m.inputArea.View())
	return tea.View{Content: content, AltScreen: true}
}

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
		// 收尾当前 AI 消息(思考 + 正文)进 chatLines
		if m.streamingThinking != "" {
			m.chatLines = append(m.chatLines, renderThinking(m.streamingThinking, m.width)...)
			m.streamingThinking = ""
		}
		if m.streaming != "" {
			m.chatLines = append(m.chatLines, renderAIMessage(m.streaming, m.width)...)
			m.streaming = ""
			m.stream = nil // 收尾，下一条 AI 消息会新建渲染器
		}
		// 追加用户消息行(首行加前缀，多行原样续行)
		userLines := strings.Split(text, "\n")
		userLines[0] = userPrefix + userLines[0]
		m.chatLines = append(m.chatLines, userLines...)
		turnLoop.Push(chatItem{query: text}, adk.WithPreempt[chatItem, adk.AgenticMessage](adk.AnySafePoint))
		return m, nil
	}
	// 其余按键交给 textarea
	var cmd tea.Cmd
	m.inputArea, cmd = m.inputArea.Update(msg)
	return m, cmd
}

// renderAIMessage 把 AI 正文 Markdown 渲染成行，并给首行加 ● 前缀。
func renderAIMessage(text string, width int) []string {
	md := renderMarkdown(text, width)
	if len(md) > 0 {
		md[0] = aiPrefix + md[0]
	}
	return md
}
