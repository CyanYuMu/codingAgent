package agent

import (
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
}

// New 创建一个 Agent。
func New(name, instruction string, m model.Model, tools *tool.Registry, mode permission.Mode) *Agent {
	return &Agent{
		name:          name,
		instruction:   instruction,
		model:         m,
		tools:         tools,
		executor:      tool.NewExecutor(tools, mode),
		maxIterations: 50,
	}
}

// Run 的实现见 loop.go。
