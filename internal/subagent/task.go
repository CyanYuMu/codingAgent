package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
	"einoclaw-build/internal/tool"
)

// taskTool 让模型派发子 agent（tasks[] 批量，一次多个 = 并行）。
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
	sb.WriteString("派一个或多个子 agent 完成独立任务。可用子 agent：")
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
		"tasks": map[string]any{
			"type":  "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"subagent": map[string]any{"type": "string"},
					"prompt":   map[string]any{"type": "string"},
				},
			},
		},
	}
}
func (taskTool) Tier() permission.Tier { return permission.TierExec }
func (taskTool) Concurrency() tool.Concurrency {
	return tool.ConcurrencyShared // 可并行
}

func (t taskTool) Execute(ctx context.Context, args map[string]any, sink *runtime.Sink) error {
	var tasks []Task
	if raw, ok := args["tasks"].([]any); ok {
		for _, item := range raw {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			subagent, _ := m["subagent"].(string)
			prompt, _ := m["prompt"].(string)
			if subagent == "" || prompt == "" {
				continue
			}
			tasks = append(tasks, Task{Subagent: subagent, Prompt: prompt})
		}
	}
	if len(tasks) == 0 {
		return fmt.Errorf("tasks 必填且每项含 subagent+prompt")
	}

	results := t.mgr.RunMany(ctx, tasks)
	var sb strings.Builder
	for _, r := range results {
		fmt.Fprintf(&sb, "## 子 agent %s [%s]\n", r.ID, statusString(r.Status))
		switch {
		case r.Status == StatusCompleted && r.Data != nil:
			b, _ := json.MarshalIndent(r.Data, "", "  ")
			sb.Write(b)
		case r.Status == StatusCompleted:
			sb.WriteString(r.Text)
		default:
			sb.WriteString(fmt.Sprintf("failed: %v", r.Err))
		}
		sb.WriteString("\n\n")
	}
	sink.Write([]byte(sb.String()))
	return nil
}

func statusString(s Status) string {
	switch s {
	case StatusPending:
		return "pending"
	case StatusRunning:
		return "running"
	case StatusCompleted:
		return "completed"
	case StatusFailed:
		return "failed"
	case StatusKilled:
		return "killed"
	}
	return "unknown"
}
