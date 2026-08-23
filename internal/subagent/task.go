package subagent

import (
	"context"
	"fmt"
	"strings"

	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
	"einoclaw-build/internal/tool"
)

// taskTool 让模型派发子 agent。
type taskTool struct {
	mgr *Manager
}

// NewTaskTool 构造 task 工具。
func NewTaskTool(mgr *Manager) tool.Tool {
	return taskTool{mgr: mgr}
}

func (taskTool) Name() string { return "task" }

// Description 动态枚举可用子 agent，让模型知道「能派谁」。
func (t taskTool) Description() string {
	var sb strings.Builder
	sb.WriteString("派一个子 agent 完成独立任务。可用子 agent：")
	for i, d := range t.mgr.List() {
		if i > 0 {
			sb.WriteString("、")
		}
		fmt.Fprintf(&sb, "%s(%s", d.Name, d.Description)
		if d.WhenToUse != "" {
			fmt.Fprintf(&sb, "，用于%s", d.WhenToUse)
		}
		sb.WriteString(")")
	}
	sb.WriteString("。子 agent 独立并行执行，返回结构化结论。")
	return sb.String()
}

func (taskTool) Parameters() map[string]any {
	return map[string]any{
		"subagent": map[string]any{"type": "string"},
		"prompt":   map[string]any{"type": "string"},
	}
}
func (taskTool) Tier() permission.Tier { return permission.TierExec }
func (taskTool) Concurrency() tool.Concurrency {
	return tool.ConcurrencyShared // 可并行：多个 task 同时跑
}

func (t taskTool) Execute(ctx context.Context, args map[string]any, sink *runtime.Sink) error {
	name, _ := args["subagent"].(string)
	prompt, _ := args["prompt"].(string)
	if name == "" || prompt == "" {
		return fmt.Errorf("subagent 和 prompt 必填")
	}
	result, err := t.mgr.Run(ctx, name, prompt)
	if err != nil {
		return err
	}
	sink.Write([]byte(result))
	return nil
}
