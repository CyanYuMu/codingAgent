package agent

import "einoclaw-build/internal/model"

// Agent 是一个可运行的编程智能体（P1 只有名字/指令/模型；工具 P4 加）。
type Agent struct {
	name        string
	instruction string
	model       model.Model
}

// New 创建一个 Agent。
func New(name, instruction string, m model.Model) *Agent {
	return &Agent{name: name, instruction: instruction, model: m}
}

// Run 的实现见 loop.go（Task 3）。
