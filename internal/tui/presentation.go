package tui

import (
	"fmt"
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/lipgloss/v2"
	"einoclaw-build/internal/subagent"
	"github.com/charmbracelet/x/ansi"
)

var (
	accentStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("81")).Bold(true)
	violetStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("141"))
	ruleStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("60"))
)

func newComposer() textarea.Model {
	ta := textarea.New()
	ta.Placeholder = "描述任务，或输入 /help"
	ta.Prompt = "❯ "
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("ctrl+j", "shift+enter"), key.WithHelp("ctrl+j", "newline"))
	ta.SetHeight(1)
	styles := ta.Styles()
	styles.Focused.Prompt = accentStyle
	styles.Focused.Placeholder = dimStyle
	styles.Focused.CursorLine = lipgloss.NewStyle()
	ta.SetStyles(styles)
	ta.Focus()
	return ta
}

// Keep the editor responsive to both explicit newlines and visually wrapped text.
func (m *teaModel) resizeComposer() {
	m.inputArea.SetWidth(max(1, m.width))
	rows := 0
	for _, line := range strings.Split(m.inputArea.Value(), "\n") {
		rows += max(1, (lipgloss.Width(line)+max(1, m.width-2)-1)/max(1, m.width-2))
	}
	m.inputArea.SetHeight(min(max(1, rows), max(1, min(5, m.height/3))))
}

func welcomeLines(width, height int) []string {
	title := accentStyle.Render("◈ CODECLAW") + violetStyle.Render("  /  AGENT HARNESS")
	if width < 55 || height < 12 {
		return []string{title, dimStyle.Render("准备就绪 · 输入任务开始 · /help")}
	}
	return []string{
		"",
		ruleStyle.Render("  ╭── ◇ ───────────────────────────────────────╮"),
		accentStyle.Render("  │   ▄▀▀ ▄▀▄ █▀▄ █▀▀") + violetStyle.Render("  ▄▀▀ █   ▄▀▄ █   █      │"),
		accentStyle.Render("  │   ▀▄▄ ▀▄▀ █▄▀ ██▄") + violetStyle.Render("  ▀▄▄ █▄▄ █▀█ ▀▄▀▄▀      │"),
		ruleStyle.Render("  ╰───────┬─────────────────────────── ◇ ─────╯"),
		"          " + accentStyle.Render("CODECLAW") + dimStyle.Render(" / AGENT HARNESS"),
		"",
		"  " + violetStyle.Render("理解代码  →  执行工具  →  验证结果"),
		"  " + dimStyle.Render("输入任务开始 · Tab 补全命令 · /help 查看帮助"),
	}
}

func fitLine(line string, width int) string {
	return ansi.Truncate(line, max(1, width), "…")
}

// Physical terminal rows, not logical newlines, determine viewport geometry.
func wrapRows(lines []string, width int) []string {
	var out []string
	for _, line := range lines {
		out = append(out, strings.Split(ansi.Hardwrap(line, max(1, width), true), "\n")...)
	}
	return out
}

func (m teaModel) transcriptRows() []string {
	var all []string
	if m.rowCache != nil {
		all = append(all, m.rowCache.render(m.chatLines, m.width)...)
	} else {
		all = wrapRows(m.chatLines, m.width)
	}
	var live []string
	if m.streamingThinking != "" {
		live = append(live, renderThinking(m.streamingThinking, m.width)...)
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
		live = append(live, lines...)
	}
	return append(all, wrapRows(live, m.width)...)
}

// Chrome is measured before allocating space to the transcript. Approval actions
// always remain visible, even when tool arguments are several screens long.
func (m teaModel) chrome() (string, []string, []string) {
	status := "● READY"
	if runActive() {
		status = "◉ RUNNING"
	}
	if m.pendingApproval != nil {
		status = "◆ APPROVAL"
	}
	header := accentStyle.Render("◈ CODECLAW") + dimStyle.Render(" / "+filepath.Base(m.cwd)) + "  " + violetStyle.Render(status)
	if m.session != nil && m.width >= 80 {
		id := m.session.Header().ID
		header += dimStyle.Render("  session:" + id[:min(8, len(id))])
	}
	hint := "Enter 发送 · Ctrl+J 换行 · Tab 补全 · /help"
	if runActive() {
		hint = "Esc 中断 · Ctrl+E 注入修正 · Ctrl+C 退出"
	}
	if m.scrollOffset > 0 {
		hint = fmt.Sprintf("↑ 历史阅读 +%d 行 · Ctrl+End 回到底部", m.scrollOffset)
	}
	bottom := []string{ruleStyle.Render(strings.Repeat("─", max(1, m.width)))}
	if m.pendingApproval == nil {
		bottom = append(bottom, strings.Split(m.inputArea.View(), "\n")...)
	} else {
		call := m.pendingApproval.call
		bottom = append(bottom, violetStyle.Render("◆ 工具审批 · "+call.Name))
		preview := wrapRows([]string{call.Args}, m.width)
		limit := max(1, min(6, m.height/3))
		start := min(m.approvalOffset, max(0, len(preview)-limit))
		preview = preview[start:min(start+limit, len(preview))]
		bottom = append(bottom, preview...)
		hint = "[y] 允许 · [a] 会话允许 · [n] 拒绝 · PgUp/Dn 查看参数"
		if m.width < 60 {
			hint = "[y] 允许 [n] 拒绝 [a] 会话"
		}
	}
	bottom = append(bottom, dimStyle.Render(hint))
	var hub []string
	if m.hubOpen && m.height >= 12 {
		hub = renderHub(m.roster(), m.hubSel, m.width, max(1, m.height/4-3))
	} else if m.height >= 14 {
		var active []subagent.RunView
		for _, row := range m.roster() {
			if row.Status == "running" || row.Status == "pending" || row.Status == "idle" || row.Status == "budget_stop" {
				active = append(active, row)
			}
		}
		if len(active) > 0 {
			hub = renderHub(active, -1, m.width, min(3, max(1, m.height/5)))
		}
	}
	return fitLine(header, m.width), hub, bottom
}

func (m teaModel) chatHeight() int {
	_, hub, bottom := m.chrome()
	return max(1, m.height-1-len(hub)-len(bottom))
}

func (m *teaModel) scrollBy(delta int) {
	m.scrollOffset = min(max(0, m.scrollOffset+delta), max(0, len(m.transcriptRows())-m.chatHeight()))
}

var slashCommands = []string{"/help", "/new", "/sessions", "/resume ", "/title ", "/agents", "/agent ", "/skills", "/skills list", "/skills show ", "/skills reload", "/clear", "/forget"}

func (m teaModel) completions() []string {
	prefix := m.inputArea.Value()
	if !strings.HasPrefix(prefix, "/") {
		return nil
	}
	candidates := append([]string(nil), slashCommands...)
	if m.skillCommands && m.skillCatalog != nil {
		for _, s := range m.skillCatalog.List() {
			if s.UserInvocable {
				candidates = append(candidates, "/skill:"+s.Name+" ")
			}
		}
	}
	var matches []string
	for _, c := range candidates {
		if strings.HasPrefix(c, prefix) {
			matches = append(matches, c)
		}
	}
	return matches
}

var helpLines = []string{
	"◈ CODECLAW / 快捷操作",
	"Enter 发送 · Ctrl+J / Shift+Enter 换行 · Ctrl+E 注入修正",
	"Esc 中断当前运行 · Ctrl+C 退出 · Ctrl+A 切换 Agent Hub",
	"PgUp/PgDown 或滚轮翻阅 · Ctrl+End 回到底部 · Tab 补全命令",
	"/new 新会话 · /sessions 列表 · /resume <id> 恢复 · /title <标题>",
	"/agents 子 agent 列表 · /agent <名称> <消息> · /skills 查看技能",
	"/clear 清空当前上下文 · /forget 清空项目长期记忆",
}

func (m *teaModel) scrollApproval(delta int) {
	if m.pendingApproval == nil {
		return
	}
	rows := len(wrapRows([]string{m.pendingApproval.call.Args}, m.width))
	limit := max(1, min(6, m.height/3))
	m.approvalOffset = min(max(0, m.approvalOffset+delta), max(0, rows-limit))
}

// Finalized chat lines are append-only between session resets. Cache their
// physical rows so cursor blinks and hub polling never rewrap the transcript.
type transcriptCache struct {
	source []string
	width  int
	rows   []string
}

func (c *transcriptCache) render(lines []string, width int) []string {
	same := len(lines) > 0 && len(c.source) > 0 && &lines[0] == &c.source[0]
	if c.width != width || !same || len(lines) < len(c.source) {
		c.source, c.rows = nil, nil
		c.width = width
	}
	c.rows = append(c.rows, wrapRows(lines[len(c.source):], width)...)
	c.source = lines
	return c.rows
}
