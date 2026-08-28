package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"einoclaw-build/internal/agent"
	agentctx "einoclaw-build/internal/context"
	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
	"einoclaw-build/internal/subagent"
)

// headlessApprover：-p 模式没有弹窗，需要审批的调用一律拒绝并说明（用 --yolo 放行）。
type headlessApprover struct{}

func (headlessApprover) Approve(context.Context, message.ToolCall) (bool, error) { return false, nil }
func (headlessApprover) DenyReason() string {
	return "tool denied: headless mode cannot prompt for approval (run with --yolo or set approval_mode)"
}

// maxAutoContinue headless 下因后台作业结果自动续跑的上限：够把结果综合完，又不会在 CI 里无限循环。
const maxAutoContinue = 3

// runHeadless 执行一个提示词，并在后台作业结算后自动续跑（最多 maxAutoContinue 轮）；返回退出码。
func runHeadless(ctx context.Context, ag *agent.Agent, cmgr *agentctx.Manager, mgr *subagent.Manager,
	prompt string, waitJobs time.Duration) int {
	code := runOnce(ctx, ag, cmgr, prompt)
	for i := 0; i < maxAutoContinue; i++ {
		jobs, mails := waitDeliveries(ctx, mgr, waitJobs)
		if len(jobs) == 0 && len(mails) == 0 {
			break
		}
		for _, j := range jobs {
			fmt.Printf("[后台作业完成: %s %s]\n", j.JobID, subagent.StatusString(j.Result.Status))
		}
		for _, ml := range mails {
			fmt.Printf("[来自 %s 的消息: %s]\n", ml.From, ml.Text)
		}
		if c := runOnce(ctx, ag, cmgr, subagent.RenderAsyncResult(jobs, mails)); c != 0 {
			code = c
		}
	}
	return code
}

// waitDeliveries 等后台作业结算或消息到达（上限 wait）；没有在跑的作业就立刻返回。
func waitDeliveries(ctx context.Context, mgr *subagent.Manager, wait time.Duration) ([]subagent.JobResult, []subagent.Mail) {
	if mgr == nil {
		return nil, nil
	}
	deadline := time.Now().Add(wait)
	for {
		jobs, mails := mgr.TakeSettled(), mgr.TakeMainInbox()
		if len(jobs) > 0 || len(mails) > 0 {
			return jobs, mails
		}
		if mgr.Pending() == 0 || time.Now().After(deadline) || ctx.Err() != nil {
			return nil, nil
		}
		select {
		case <-time.After(200 * time.Millisecond):
		case <-ctx.Done():
			return nil, nil
		}
	}
}

// runOnce 记录一条用户消息并跑一轮循环，把事件打印到 stdout。
func runOnce(ctx context.Context, ag *agent.Agent, cmgr *agentctx.Manager, prompt string) int {
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
