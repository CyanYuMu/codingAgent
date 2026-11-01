package subagent

import (
	"context"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/tool"
)

// denyApprover 是 headless 子 agent 的默认策略：需要审批的调用一律拒绝并说明，不阻塞。
type denyApprover struct{}

func (denyApprover) Approve(context.Context, message.ToolCall) (bool, error) { return false, nil }

func (denyApprover) DenyReason() string {
	return "headless subagent cannot prompt for approval; forced approval still applies in yolo mode. Use a scoped tool, or an interactive parent with subagent.approval_escalation enabled; do not repeat the same denied call"
}

// labeledApprover 把子 agent 的审批升级到父审批器，调用名带标签供弹窗显示「谁在请求」。
type labeledApprover struct {
	inner tool.Approver
	label string
}

func (l labeledApprover) Approve(ctx context.Context, c message.ToolCall) (bool, error) {
	c.Name = l.label + " " + c.Name
	return l.inner.Approve(ctx, c)
}

// 编译期断言
var (
	_ tool.Approver     = denyApprover{}
	_ tool.DenyReasoner = denyApprover{}
	_ tool.Approver     = labeledApprover{}
)
