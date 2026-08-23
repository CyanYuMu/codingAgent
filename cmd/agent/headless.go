package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"einoclaw-build/internal/agent"
	agentctx "einoclaw-build/internal/context"
	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
)

// headlessApprover：-p 模式没有弹窗，需要审批的调用一律拒绝并说明（用 --yolo 放行）。
type headlessApprover struct{}

func (headlessApprover) Approve(context.Context, message.ToolCall) (bool, error) { return false, nil }
func (headlessApprover) DenyReason() string {
	return "tool denied: headless mode cannot prompt for approval (run with --yolo or set approval_mode)"
}

// runHeadless 执行一个提示词：记录用户消息 → 跑循环 → 把事件打印到 stdout；返回退出码。
func runHeadless(ctx context.Context, ag *agent.Agent, cmgr *agentctx.Manager, prompt string) int {
	_ = cmgr.Record(message.NewUserMessage(prompt), model.Usage{})
	var final strings.Builder
	code := 0
	for ev := range ag.Run(ctx, nil) {
		switch ev.Type {
		case agent.EventMessageUpdate:
			if ev.Update.Text != "" {
				final.WriteString(ev.Update.Text)
			}
		case agent.EventMessageEnd:
			if final.Len() > 0 {
				fmt.Println(final.String())
				final.Reset()
			}
			if u := ev.Ended.Usage; u.PromptTokens > 0 {
				fmt.Printf("  [usage prompt=%d completion=%d cached=%d]\n", u.PromptTokens, u.CompletionTokens, u.CachedTokens)
			}
		case agent.EventToolStart:
			fmt.Printf("▶ %s %s\n", ev.ToolStart.Name, clipLine(ev.ToolStart.Args, 200))
		case agent.EventToolEnd:
			mark := "◀"
			if ev.ToolEnd.IsError {
				mark = "✗"
			}
			fmt.Printf("%s %s %s\n", mark, ev.ToolEnd.Name, clipLine(firstLines(ev.ToolEnd.Content, 3), 300))
		case agent.EventCompaction:
			fmt.Printf("[compaction: %s]\n", ev.Compaction.Reason)
		case agent.EventRetry:
			fmt.Printf("[retry %d after %v: %v]\n", ev.Retry.Attempt, ev.Retry.Delay, ev.Retry.Err)
		case agent.EventTerminated:
			fmt.Printf("[terminated by %s]\n", ev.Terminated.ToolName)
		case agent.EventError:
			fmt.Fprintf(os.Stderr, "error: %v\n", ev.Err)
			code = 1
		}
	}
	return code
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		return strings.Join(lines[:n], "\n") + fmt.Sprintf(" …(+%d lines)", len(lines)-n)
	}
	return s
}

func clipLine(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", "⏎")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
