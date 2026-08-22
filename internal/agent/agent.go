package agent

import (
	"einoclaw-build/internal/memory"
	"einoclaw-build/internal/model"
	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/tool"
)

// Agent 是一个可运行的编程智能体。
type Agent struct {
	name          string
	instruction   string
	model         model.Model
	tools         *tool.Registry  // 工具注册表（给模型的工具定义）
	executor      *tool.Executor  // 工具执行器（审批 + 执行）
	maxIterations int             // 工具循环上限，防失控
	memory        memory.Recaller // nil = 无记忆
}

// New 创建一个 Agent。approver 为 nil 时退化为「拒绝+说明」（无 HITL）；mem 为 nil 时无记忆。
func New(name, instruction string, m model.Model, tools *tool.Registry, mode permission.Mode, approver tool.Approver, mem memory.Recaller) *Agent {
	return &Agent{
		name:          name,
		instruction:   instruction,
		model:         m,
		tools:         tools,
		executor:      tool.NewExecutor(tools, mode, approver),
		maxIterations: 50,
		memory:        mem,
	}
}

// Run 的实现见 loop.go。
