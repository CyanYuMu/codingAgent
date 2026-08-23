package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"einoclaw-build/internal/agent"
	"einoclaw-build/internal/memory"
	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/tool"
)

// Manager 派发子 agent：借用父的模型/工具/记忆，但独立 context。
type Manager struct {
	model  model.Model
	tools  *tool.Registry
	memory memory.Recaller
	defs   []SubagentSpec
	sem    chan struct{} // 并发上限（Semaphore）
}

func NewManager(m model.Model, tools *tool.Registry, mem memory.Recaller, defs []SubagentSpec) *Manager {
	return &Manager{
		model: m, tools: tools, memory: mem, defs: defs,
		sem: make(chan struct{}, 4), // 默认并发上限 4
	}
}

// List 返回子 agent 定义（确定性顺序，供 task 工具枚举）。
func (m *Manager) List() []SubagentSpec { return m.defs }

func (m *Manager) find(name string) (SubagentSpec, bool) {
	for _, d := range m.defs {
		if d.Name == name {
			return d, true
		}
	}
	return SubagentSpec{}, false
}

// Run 派发一个子 agent，返回 Result。
// Context Isolation：只传 [prompt]，不传父历史；拦截 yield 的结构化产出。
func (m *Manager) Run(ctx context.Context, name, prompt string) Result {
	def, ok := m.find(name)
	if !ok {
		return Result{ID: name, Status: StatusFailed, Err: fmt.Errorf("unknown subagent %q", name)}
	}

	// 子 agent 工具：worker 工具（无 task）+ yield
	subTools := m.tools.Without("task")
	subTools.Register(NewYieldTool())
	sub := agent.New(def.Name, def.SystemPrompt, m.model, subTools, permission.ModeYolo, nil, m.memory)
	sub.SetMaxIterations(def.MaxTurns)

	// wall-clock 超时
	runCtx := ctx
	var cancel context.CancelFunc
	if def.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, def.Timeout)
		defer cancel()
	}

	var data map[string]any
	var text string
	var runErr error
	status := StatusCompleted
	for ev := range sub.Run(runCtx, []message.Message{message.NewUserMessage(prompt)}) {
		switch ev.Type {
		case agent.EventToolStart:
			if ev.ToolStart.Name == "yield" {
				var args map[string]any
				if json.Unmarshal([]byte(ev.ToolStart.Args), &args) == nil {
					if d, ok := args["data"].(map[string]any); ok {
						data = d
					}
				}
			}
		case agent.EventMessageEnd:
			text = textOf(ev.Ended.Message)
		case agent.EventError:
			runErr = ev.Err
			status = StatusFailed
		}
	}

	// outputSchema 校验（基本）：要求了 schema 却没产出 data → failed
	if def.OutputSchema != nil && data == nil {
		status = StatusFailed
		if runErr == nil {
			runErr = fmt.Errorf("子 agent 未产出符合 schema 的结构化数据")
		}
	}
	return Result{ID: name, Status: status, Data: data, Text: text, Err: runErr}
}

// RunMany 并行派发多个子 agent（Semaphore 限并发），结果按序返回。
func (m *Manager) RunMany(ctx context.Context, tasks []Task) []Result {
	results := make([]Result, len(tasks))
	var wg sync.WaitGroup
	for i, t := range tasks {
		if err := m.acquire(ctx); err != nil {
			results[i] = Result{ID: t.Subagent, Status: StatusFailed, Err: err}
			continue
		}
		wg.Add(1)
		go func(i int, t Task) {
			defer wg.Done()
			defer m.release()
			results[i] = m.Run(ctx, t.Subagent, t.Prompt)
		}(i, t)
	}
	wg.Wait()
	return results
}

func (m *Manager) acquire(ctx context.Context) error {
	select {
	case m.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) release() { <-m.sem }

func textOf(m message.Message) string {
	var sb strings.Builder
	for _, b := range m.Blocks {
		if b.Kind == message.BlockText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}
