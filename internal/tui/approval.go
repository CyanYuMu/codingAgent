package tui

import (
	"context"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/tool"
)

// approver 实现 tool.Approver：经 program.Send 弹窗，阻塞等决定（interrupt/resume 的暂停点）。
type approver struct {
	onSessionAllow func(name string) // 「本会话允许」按钮的回调（由装配层接到 Executor.AllowSession）
}

func (a approver) Approve(ctx context.Context, call message.ToolCall) (bool, error) {
	resp := make(chan approvalAnswer, 1)
	if program == nil {
		return false, nil // 无 TUI，拒绝
	}
	program.Send(approvalRequestMsg{call: call, resp: resp})
	select {
	case d := <-resp:
		if d.sessionAllow && a.onSessionAllow != nil {
			a.onSessionAllow(call.Name)
		}
		return d.allow, nil
	case <-ctx.Done():
		return false, ctx.Err() // Ctrl+C 中断审批
	}
}

// NewApprover 构造审批器；onSessionAllow 可为 nil（无「本会话允许」按钮）。
func NewApprover(onSessionAllow func(name string)) tool.Approver {
	return approver{onSessionAllow: onSessionAllow}
}

// approvalAnswer 三态回答：allow=是否执行；sessionAllow=是否记住本会话允许。
type approvalAnswer struct {
	allow        bool
	sessionAllow bool
}

// approvalRequestMsg 是发给 TUI 的审批请求（modal 弹窗）。
type approvalRequestMsg struct {
	call message.ToolCall
	resp chan approvalAnswer
}
