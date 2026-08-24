package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"einoclaw-build/internal/bus"
	"einoclaw-build/internal/subagent"
)

// hubTickMsg 表示子 agent 那边有新状态，重绘即可（面板每次渲染直接读名册快照，
// 不在 TUI 里维护第二份状态，避免两处状态不一致）。
type hubTickMsg struct{}

var (
	hubTitleStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Bold(true)
	hubSelStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("63")).Bold(true)
	hubRunningStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	hubFailStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
)

// mergeHubEvents 把子 agent 的生命周期/进度/作业三个通道汇成一条，供 TUI 唤醒重绘。
// 满则丢：面板渲染的是名册快照，丢事件最多晚一帧，不会显示错。
func mergeHubEvents(b *bus.Bus) <-chan bus.Envelope {
	out := make(chan bus.Envelope, 256)
	if b == nil {
		return out
	}
	for _, ch := range []string{subagent.ChLifecycle, subagent.ChProgress, subagent.ChJob} {
		c, _ := b.Subscribe(ch, 128)
		go func(c <-chan bus.Envelope) {
			for e := range c {
				select {
				case out <- e:
				default:
				}
			}
		}(c)
	}
	return out
}

// waitHubEvent 等一条子 agent 事件；收到后把通道里已排队的一起排干（合并重绘）。
func waitHubEvent(ch <-chan bus.Envelope) tea.Cmd {
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		if _, ok := <-ch; !ok {
			return nil
		}
		for {
			select {
			case <-ch:
			default:
				return hubTickMsg{}
			}
		}
	}
}

// renderHub 渲染 Agent Hub 面板：表头聚合 + 每个 Run 一行。
func renderHub(rows []subagent.RunView, sel, width, maxRows int) []string {
	running, parked, tokens := 0, 0, 0
	for _, r := range rows {
		switch r.Status {
		case "running", "idle", "budget_stop", "pending":
			running++
		case "parked":
			parked++
		}
		tokens += r.Tokens
	}
	head := fmt.Sprintf("Agent Hub — 运行中 %d · 已结束 %d · 累计 %s tokens", running, parked, humanCount(tokens))
	out := []string{hubTitleStyle.Render(head) + dimStyle.Render("   [j/k 选择  x 终止  esc 关闭]")}
	if len(rows) == 0 {
		return append(out, dimStyle.Render("  （还没有派发过子 agent；用 task 工具派发后这里会实时显示）"))
	}
	start := 0
	if maxRows > 0 && len(rows) > maxRows {
		start = min(max(sel-maxRows/2, 0), len(rows)-maxRows) // 选中行居中
	}
	end := len(rows)
	if maxRows > 0 {
		end = min(start+maxRows, len(rows))
	}
	if start > 0 {
		out = append(out, dimStyle.Render(fmt.Sprintf("  ↑ 上面还有 %d 行", start)))
	}
	for i := start; i < end; i++ {
		out = append(out, renderHubRow(rows[i], i == sel, width))
	}
	if end < len(rows) {
		out = append(out, dimStyle.Render(fmt.Sprintf("  ↓ 下面还有 %d 行", len(rows)-end)))
	}
	return out
}

func renderHubRow(r subagent.RunView, selected bool, width int) string {
	mark := "  "
	if selected {
		mark = hubSelStyle.Render("▸ ")
	}
	status := r.Status
	switch r.Status {
	case "running", "idle":
		status = hubRunningStyle.Render(status)
	case "failed", "killed", "timeout", "aborted":
		status = hubFailStyle.Render(status)
	default:
		status = dimStyle.Render(status)
	}
	activity := r.CurrentTool
	if activity == "" {
		activity = "—"
	}
	line := fmt.Sprintf("%-14s %-10s %-12s req=%-3d tools=%-3d tok=%-6s %-12s %s",
		clip(r.Name, 14), clip(r.Agent, 10), status, r.Requests, r.ToolCalls,
		humanCount(r.Tokens), clip(activity, 12), shortDur(r.Age))
	if width > 4 {
		line = clipVisible(line, width-4)
	}
	return mark + line
}

// parseAgentCommand 解析 "/agent <Name> <文本>"。
func parseAgentCommand(text string) (name, msg string, ok bool) {
	rest := strings.TrimSpace(strings.TrimPrefix(text, "/agent"))
	parts := strings.SplitN(rest, " ", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", false
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), true
}

func humanCount(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%d", n)
}

func shortDur(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%.1fh", d.Hours())
	case d >= time.Minute:
		return fmt.Sprintf("%.0fm", d.Minutes())
	default:
		return fmt.Sprintf("%.0fs", d.Seconds())
	}
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:max(n-1, 1)]) + "…"
}

// clipVisible 按可见宽度截断（跳过 ANSI 转义序列，避免把颜色码切坏）。
func clipVisible(s string, width int) string {
	var b strings.Builder
	visible, inEscape := 0, false
	for _, r := range s {
		if r == '\x1b' {
			inEscape = true
		}
		b.WriteRune(r)
		if inEscape {
			if r == 'm' {
				inEscape = false
			}
			continue
		}
		if visible++; visible >= width {
			break
		}
	}
	return b.String()
}
