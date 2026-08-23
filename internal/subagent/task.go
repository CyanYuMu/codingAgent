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

// Description 动态枚举可用子 agent，让模型知道「能派谁」，并说明任务描述的格式契约。
func (t taskTool) Description() string {
	var sb strings.Builder
	sb.WriteString("派一个或多个子 agent 并行完成彼此独立的任务；子 agent 从空白上下文开始，只能看到你写的 prompt。\n")
	sb.WriteString("prompt 必须自包含：目标（Target：涉及的文件/符号与非目标）、改动/步骤（Change）、验收标准（Acceptance：可观察的结果）；禁止一句话派发。\n")
	sb.WriteString("子 agent 以 yield 提交结构化结论；状态 completed 只表示它结束了，不代表结果正确，你需要验收。\n可用子 agent：")
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
	return sb.String()
}

func (taskTool) Parameters() map[string]any {
	return map[string]any{
		"tasks": map[string]any{
			"type":        "array",
			"description": "要并行派发的任务列表",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":     map[string]any{"type": "string", "description": "可选的稳定名（如 Explorer）"},
					"subagent": map[string]any{"type": "string", "description": "子 agent 类型名"},
					"prompt":   map[string]any{"type": "string", "description": "自包含的任务说明：Target / Change / Acceptance"},
				},
				"required": []string{"subagent", "prompt"},
			},
		},
	}
}
func (taskTool) Required() []string    { return []string{"tasks"} }
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
			name, _ := m["name"].(string)
			subagent, _ := m["subagent"].(string)
			prompt, _ := m["prompt"].(string)
			if subagent == "" || prompt == "" {
				continue
			}
			tasks = append(tasks, Task{Name: name, Subagent: subagent, Prompt: prompt})
		}
	}
	if len(tasks) == 0 {
		return fmt.Errorf("tasks 必填且每项含 subagent+prompt")
	}

	results := t.mgr.RunMany(ctx, tasks)
	var sb strings.Builder
	for _, r := range results {
		sb.WriteString(renderResult(r))
		sb.WriteString("\n\n")
	}
	sink.Write([]byte(sb.String()))
	return nil
}

// renderResult 把一个子 agent 的结果渲染成给父 agent 的文本。
func renderResult(r Result) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "## %s (%s) [%s] requests=%d tokens=%d %dms\n", r.Name, r.ID, StatusString(r.Status), r.Requests, r.Usage.TotalTokens, r.DurationMs)
	switch {
	case r.Status == StatusCompleted && r.Data != nil:
		b, _ := json.MarshalIndent(r.Data, "", "  ")
		sb.Write(b)
	case r.Status == StatusCompleted && !r.Yielded:
		sb.WriteString("[未显式 yield，以下为最后输出]\n")
		sb.WriteString(r.Text)
	case r.Status == StatusCompleted:
		sb.WriteString(r.Text)
	default:
		if r.Err != nil {
			fmt.Fprintf(&sb, "error: %v\n", r.Err)
		}
		if r.Text != "" {
			sb.WriteString("[partial] " + clipText(r.Text, 2000))
		}
	}
	if r.SessionFile != "" {
		fmt.Fprintf(&sb, "\n(transcript: %s)", r.SessionFile)
	}
	return sb.String()
}

func clipText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
