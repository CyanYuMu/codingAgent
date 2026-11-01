package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"einoclaw-build/internal/agent"
	"einoclaw-build/internal/message"
	"github.com/charmbracelet/x/ansi"
)

func displayModel(w, h int) teaModel {
	m := teaModel{width: w, height: h, cwd: "/workspace/codeclaw", inputArea: newComposer(), rowCache: &transcriptCache{}}
	m.resizeComposer()
	return m
}

func TestViewportGeometry(t *testing.T) {
	for _, size := range [][2]int{{100, 30}, {60, 18}, {32, 12}, {12, 6}, {1, 1}} {
		for _, mode := range []string{"welcome", "chat", "approval", "hub"} {
			t.Run(fmt.Sprintf("%dx%d/%s", size[0], size[1], mode), func(t *testing.T) {
				m := displayModel(size[0], size[1])
				if mode == "chat" {
					m.chatLines = []string{strings.Repeat("中文🙂长行 ", 50)}
				}
				if mode == "approval" {
					m.pendingApproval = &approvalRequestMsg{call: message.ToolCall{Name: "bash", Args: strings.Repeat("命令参数 ", 100)}}
				}
				m.hubOpen = mode == "hub"
				view := m.View().Content
				lines := strings.Split(view, "\n")
				if len(lines) != m.height {
					t.Fatalf("height=%d want %d", len(lines), m.height)
				}
				for i, line := range lines {
					if lipgloss.Width(line) > m.width {
						t.Fatalf("row %d width=%d > %d: %q", i, lipgloss.Width(line), m.width, line)
					}
				}
				if mode == "approval" && m.width >= 32 && !strings.Contains(ansi.Strip(view), "[n] 拒绝") {
					t.Fatal("approval controls hidden")
				}
			})
		}
	}
}

func TestScrollClampsAndAnchorsNewOutput(t *testing.T) {
	m := displayModel(60, 18)
	for i := 0; i < 80; i++ {
		m.chatLines = append(m.chatLines, fmt.Sprintf("line %03d", i))
	}
	m.scrollBy(10000)
	maxOffset := len(m.transcriptRows()) - m.chatHeight()
	if m.scrollOffset != maxOffset {
		t.Fatalf("offset=%d want %d", m.scrollOffset, maxOffset)
	}
	before := strings.Split(ansi.Strip(m.View().Content), "\n")[1]
	updated, _ := m.Update(agent.AgentEvent{Type: agent.EventError, Err: fmt.Errorf("new output")})
	m = updated.(teaModel)
	after := strings.Split(ansi.Strip(m.View().Content), "\n")[1]
	if before != after {
		t.Fatalf("reading position moved: %q -> %q", before, after)
	}
	m.scrollBy(-10000)
	if m.scrollOffset != 0 {
		t.Fatal("did not return to live output")
	}
}

func TestComposerCompletionAndNewlines(t *testing.T) {
	m := displayModel(60, 24)
	m.inputArea.SetValue("/sess")
	m, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyTab})
	if m.inputArea.Value() != "/sessions" {
		t.Fatalf("completion=%q", m.inputArea.Value())
	}
	m.inputArea.SetValue("第一行")
	m, _ = m.handleKey(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
	if !strings.Contains(m.inputArea.Value(), "\n") || m.inputArea.Height() != 2 {
		t.Fatalf("newline not inserted: %q", m.inputArea.Value())
	}
	m.inputArea.SetValue(strings.Repeat("内容", 100))
	m.resizeComposer()
	if m.inputArea.Height() != 5 {
		t.Fatal("editor should cap at 5 rows")
	}
}

func TestApprovalScrollAndEscape(t *testing.T) {
	m := displayModel(60, 18)
	resp := make(chan approvalAnswer, 1)
	m.pendingApproval = &approvalRequestMsg{call: message.ToolCall{Name: "bash", Args: strings.Repeat("args\n", 40) + "LAST"}, resp: resp}
	m.scrollApproval(1000)
	if !strings.Contains(m.View().Content, "LAST") {
		t.Fatal("cannot inspect final arguments")
	}
	m, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.pendingApproval != nil || (<-resp).allow {
		t.Fatal("escape must deny approval")
	}
}

func TestEscapeCancelsWithoutQuitting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	id := setCurrentRun(cancel, make(chan message.Message, 1))
	defer clearCurrentRun(id)
	m := displayModel(60, 18)
	_, cmd := m.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if ctx.Err() == nil || cmd != nil {
		t.Fatal("escape should cancel and keep UI open")
	}
}

func TestStreamingCacheDoesNotLeakPrefixMutation(t *testing.T) {
	s := &streamingMarkdown{}
	first := s.Render("Hello **world**", 60)
	want := strings.Join(first, "\n")
	first[0] = "modified"
	if got := strings.Join(s.Render("Hello **world**", 60), "\n"); got != want {
		t.Fatal("cached render was mutated")
	}
	changed := strings.Join(s.Render("Different", 30), "\n")
	if !strings.Contains(changed, "Different") {
		t.Fatal("stale cache after content/width change")
	}
}

func TestTranscriptCacheAppendResizeAndReset(t *testing.T) {
	c := &transcriptCache{}
	lines := make([]string, 1, 4)
	lines[0] = "1234567890"
	if len(c.render(lines, 5)) != 2 {
		t.Fatal("wrap")
	}
	lines = append(lines, "next")
	if len(c.render(lines, 5)) != 3 {
		t.Fatal("append")
	}
	if len(c.render(lines, 20)) != 2 {
		t.Fatal("resize")
	}
	if got := c.render([]string{"new session"}, 20); len(got) != 1 || got[0] != "new session" {
		t.Fatal("session reset")
	}
	if len(c.render(nil, 20)) != 0 {
		t.Fatal("clear")
	}
}

func BenchmarkStreamingCachedFrame(b *testing.B) {
	m := displayModel(100, 30)
	for i := 0; i < 1000; i++ {
		m.chatLines = append(m.chatLines, strings.Repeat("history ", 10))
	}
	m.streaming = strings.Repeat("A paragraph with **code**.\n\n", 40)
	m.stream = &streamingMarkdown{}
	m.View()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.View()
	}
}
