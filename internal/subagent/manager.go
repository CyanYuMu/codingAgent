package subagent

import (
	"context"
	"fmt"
	"strings"

	"einoclaw-build/internal/agent"
	"einoclaw-build/internal/memory"
	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/tool"
)

// SubagentSpec 一个子 agent 的声明。
type SubagentSpec struct {
	Name         string
	Description  string
	SystemPrompt string
	WhenToUse    string // 触发场景，task 描述枚举时带上
}

// Manager 派发子 agent：借用父的模型/工具/记忆，但独立 context。
type Manager struct {
	model  model.Model
	tools  *tool.Registry
	memory memory.Recaller
	defs   []SubagentSpec // 保序（供 task 工具枚举）
}

func NewManager(m model.Model, tools *tool.Registry, mem memory.Recaller, defs []SubagentSpec) *Manager {
	return &Manager{model: m, tools: tools, memory: mem, defs: defs}
}

// List 返回子 agent 定义（确定性顺序，供 task 工具枚举可用子 agent）。
func (m *Manager) List() []SubagentSpec { return m.defs }

func (m *Manager) find(name string) (SubagentSpec, bool) {
	for _, d := range m.defs {
		if d.Name == name {
			return d, true
		}
	}
	return SubagentSpec{}, false
}

// Run 派发一个子 agent，返回最终结论文本。
// Context Isolation：只传 [prompt]，不传父的历史；父只拿 result，不拿子 agent 的中间过程。
func (m *Manager) Run(ctx context.Context, name, prompt string) (string, error) {
	def, ok := m.find(name)
	if !ok {
		return "", fmt.Errorf("unknown subagent %q", name)
	}
	// 子 agent：同模型、同工具、同记忆，headless（yolo，无审批）。
	// 去掉 task 工具，防止子 agent 再派子 agent（递归）。
	sub := agent.New(def.Name, def.SystemPrompt, m.model, m.tools.Without("task"), permission.ModeYolo, nil, m.memory)

	var result string
	var runErr error
	for ev := range sub.Run(ctx, []message.Message{message.NewUserMessage(prompt)}) {
		switch ev.Type {
		case agent.EventMessageEnd:
			result = textOf(ev.Ended.Message) // 最后一个定稿消息 = 最终结论
		case agent.EventError:
			runErr = ev.Err
		}
	}
	if runErr != nil {
		return "", runErr // 传播子 agent 的错误（如模型调用失败）
	}
	return result, nil
}

func textOf(m message.Message) string {
	var sb strings.Builder
	for _, b := range m.Blocks {
		if b.Kind == message.BlockText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}
