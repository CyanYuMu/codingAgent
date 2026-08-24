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

// taskTool 让模型派发子 agent。一次调用 = 一个批次（tasks 多项 = 并行）。
// depth/self/spawns 决定它自己能派谁：主 agent 是 (0, "", nil)，子 agent 带着自己的定义约束。
type taskTool struct {
	mgr    *Manager
	depth  int
	self   string
	spawns []string
}

// NewTaskTool 构造 task 工具。
func NewTaskTool(mgr *Manager, depth int, self string, spawns []string) tool.Tool {
	return taskTool{mgr: mgr, depth: depth, self: self, spawns: spawns}
}

func (taskTool) Name() string { return "task" }

// Description 动态枚举可用子 agent（含只读/阻塞/schema 标记），并说明批次契约。
func (t taskTool) Description() string {
	var sb strings.Builder
	sb.WriteString("派一个或多个子 agent 完成彼此独立的任务（tasks 多项 = 并行）。子 agent 从空白上下文开始，只能看到 context + 它那一项 task。\n")
	sb.WriteString("context 必填：整批共享的 Goal（要达成什么）/ Constraints（不能碰什么）/ Contract（跨任务共享的接口与字段）。\n")
	sb.WriteString("每项 task 必须自包含：Target（涉及文件/符号与非目标）、Change（步骤）、Acceptance（可观察结果）；一句话派发会被拒绝。\n")
	sb.WriteString("子 agent 以 yield 提交结构化结论；状态 completed 只表示它结束了，不代表结果正确——你必须自己验收。\n")
	sb.WriteString("可用子 agent：\n")
	for _, d := range t.mgr.List() {
		if !spawnAllowed(t.spawns, d.Name) || (t.self != "" && d.Name == t.self) {
			continue
		}
		fmt.Fprintf(&sb, "- %s：%s", d.Name, d.Description)
		if d.WhenToUse != "" {
			fmt.Fprintf(&sb, "（用于%s）", d.WhenToUse)
		}
		if d.ReadOnly {
			sb.WriteString(" [READ-ONLY]")
		}
		if d.Blocking {
			sb.WriteString(" [BLOCKING]")
		}
		if d.OutputSchema != nil {
			sb.WriteString(" [结构化产出]")
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func (taskTool) Parameters() map[string]any {
	return map[string]any{
		"context": map[string]any{
			"type":        "string",
			"description": "整批共享的背景：Goal / Constraints / Contract。子 agent 看不到你的历史，这是唯一的共享上下文。",
		},
		"tasks": map[string]any{
			"type":        "array",
			"description": "要执行的任务列表（多项 = 并行）",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":          map[string]any{"type": "string", "description": "可选的稳定名（如 Reviewer），用于 hub 寻址与 agent:// 引用"},
					"agent":         map[string]any{"type": "string", "description": "子 agent 名"},
					"task":          map[string]any{"type": "string", "description": "自包含任务说明：Target / Change / Acceptance"},
					"output_schema": map[string]any{"type": "object", "description": "可选：覆盖该 agent 的产出 schema"},
					"schema_mode":   map[string]any{"type": "string", "enum": []string{"permissive", "strict"}, "description": "schema 校验模式，默认 permissive"},
					"effort":        map[string]any{"type": "string", "enum": []string{"lo", "med", "hi"}, "description": "投入档位（暂只记录供审计）"},
				},
				"required": []string{"agent", "task"},
			},
		},
		"background": map[string]any{
			"type":        "boolean",
			"description": "true = 立即返回作业 id，完成后结果自动送回你的会话；适合长任务。默认 false（同步等待）。",
		},
	}
}

func (taskTool) Required() []string { return []string{"context", "tasks"} }

// Tier 派发本身按 write 处理：子 agent 内部的每个工具都按继承的审批模式单独裁决，派发不是放行。
func (taskTool) Tier() permission.Tier { return permission.TierWrite }
func (taskTool) Concurrency() tool.Concurrency {
	return tool.ConcurrencyShared // 可并行
}

func (t taskTool) Execute(ctx context.Context, args map[string]any, sink *runtime.Sink) error {
	batch, legacy := parseBatch(args)
	env := t.mgr.Env(t.depth, t.self, t.spawns)
	var sb strings.Builder
	if legacy != "" {
		sb.WriteString(legacy + "\n\n")
	}

	if batch.Background && t.mgr.o.AllowBackground {
		inline, jobs, err := t.mgr.StartBackground(ctx, batch, env)
		if err != nil {
			return err
		}
		if len(jobs) > 0 {
			sb.WriteString("已转入后台，作业 id（= 子 agent 名）：\n")
			for _, j := range jobs {
				fmt.Fprintf(&sb, "- %s（%s）\n", j.ID, j.Agent)
			}
			sb.WriteString("不用轮询：完成后结果会自动送到你的会话。现在去做别的事；" +
				"需要时用 hub jobs 看状态、hub send 追问、hub cancel 取消。\n")
		}
		for _, r := range inline {
			sb.WriteString("\n" + renderResult(r) + "\n")
		}
		sink.Write([]byte(sb.String()))
		return nil
	}

	if batch.Background {
		sb.WriteString("[提示] 本实例未启用后台作业，已同步执行。\n\n")
	}
	results, err := t.mgr.RunBatch(ctx, batch, env)
	if err != nil {
		return err // 预检失败：错误文本告诉模型怎么改，没有任何子 agent 被启动
	}
	for _, r := range results {
		sb.WriteString(renderResult(r))
		sb.WriteString("\n\n")
	}
	sink.Write([]byte(sb.String()))
	return nil
}

// parseBatch 解析工具参数；同时兼容旧字段名（tasks[].subagent / tasks[].prompt），
// 返回的提示会随结果一起交给模型，让它下一次用新字段。
func parseBatch(args map[string]any) (TaskBatch, string) {
	var b TaskBatch
	b.Context, _ = args["context"].(string)
	b.Background, _ = args["background"].(bool)
	usedLegacy := false
	if raw, ok := args["tasks"].([]any); ok {
		for _, item := range raw {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			it := TaskItem{}
			it.Name, _ = m["name"].(string)
			it.Agent, _ = m["agent"].(string)
			it.Task, _ = m["task"].(string)
			if it.Agent == "" {
				if v, ok := m["subagent"].(string); ok && v != "" {
					it.Agent, usedLegacy = v, true
				}
			}
			if it.Task == "" {
				if v, ok := m["prompt"].(string); ok && v != "" {
					it.Task, usedLegacy = v, true
				}
			}
			it.OutputSchema, _ = m["output_schema"].(map[string]any)
			it.SchemaMode, _ = m["schema_mode"].(string)
			it.Effort, _ = m["effort"].(string)
			b.Tasks = append(b.Tasks, it)
		}
	}
	legacy := ""
	if usedLegacy {
		legacy = "[提示] 你用了旧字段名；本次已兼容执行。下次请用 {context, tasks:[{agent, task}]}。"
	}
	return b, legacy
}

// renderResult 把一个子 agent 的结果渲染成给父 agent 的文本。
func renderResult(r Result) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "## %s (%s) [%s] requests=%d tools=%d tokens=%d %dms\n",
		r.Name, r.Agent, StatusString(r.Status), r.Requests, r.ToolCalls, r.Usage.TotalTokens, r.DurationMs)
	if r.Warning != "" {
		fmt.Fprintf(&sb, "warning: %s\n", r.Warning)
	}
	switch {
	case r.Status == StatusCompleted && r.Data != nil:
		b, err := json.MarshalIndent(r.Data, "", "  ")
		if err != nil {
			fmt.Fprintf(&sb, "%v", r.Data)
		} else {
			sb.Write(b)
		}
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
	if r.OutputFile != "" {
		fmt.Fprintf(&sb, "\n(完整产出: agent://%s)", r.Name)
	}
	if r.SessionFile != "" {
		fmt.Fprintf(&sb, "\n(转录: history://%s → %s)", r.Name, r.SessionFile)
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
