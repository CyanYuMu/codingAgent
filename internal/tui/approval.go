package tui

import (
	"context"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/tool"
)

// approver 实现 tool.Approver：经 program.Send 弹窗，阻塞等决定（interrupt/resume 的暂停点）。
type approver struct{}

func (approver) Approve(ctx context.Context, call message.ToolCall) (bool, error) {
	resp := make(chan bool, 1)
	if program == nil {
		return false, nil // 无 TUI，拒绝
	}
	program.Send(approvalRequestMsg{call: call, resp: resp})
	select {
	case d := <-resp:
		return d, nil
	case <-ctx.Done():
		return false, ctx.Err() // Ctrl+C 中断审批
	}
}

// NewApprover 构造审批器。
func NewApprover() tool.Approver { return approver{} }

// approvalRequestMsg 是发给 TUI 的审批请求（modal 弹窗）。
type approvalRequestMsg struct {
	call message.ToolCall
	resp chan bool
}
