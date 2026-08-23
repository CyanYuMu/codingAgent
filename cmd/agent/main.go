package main

import (
	"context"
	"log"
	"os"

	tea "charm.land/bubbletea/v2"

	"einoclaw-build/internal/agent"
	agentctx "einoclaw-build/internal/context"
	"einoclaw-build/internal/memory"
	"einoclaw-build/internal/model"
	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
	"einoclaw-build/internal/session"
	"einoclaw-build/internal/subagent"
	"einoclaw-build/internal/tool"
	"einoclaw-build/internal/tui"
)

const baseInstruction = "你是一个编程智能体, 你的名字叫做 codeclaw, 擅长解决编程问题。当用户表达偏好、关键事实或重要决策时，调用 remember 工具记录。"

const alwaysDelegation = `
你是协调者（coordinator），不是执行者。你的工作是：理解任务 → 分解 → 派发子 agent → 综合结果 → 验收。

何时必须委派（MUST delegate）：
- 3+ 文件或跨模块改动 → 分解并委派
- 多个互相独立的调查/验证问题 → 并行派多个子 agent
- 探索未知代码库 → 派 explorer，禁止自己逐文件读
- 非平凡实现/改动后 → 派 reviewer 验收
- 长耗时验证/测试 → 派 worker

唯一例外（可自己做）：约 30 行内单文件编辑、直接回答、用户明确要求你执行某命令。

反例（禁止）：
- 委派后不要自己又读一遍文件
- 不要派子 agent 后自己 idle 等
- 不要「派了又自己做一遍」
- 顶层计划必须自己拆，不能外包给子 agent
- 必须拆成真正独立的 slice，禁止假并行
- 只有严格依赖才串行，否则并行
`

const preferredDelegation = `
多文件改动、独立调查、验证、测试是委派的 strong candidate，优先委派并行；小任务可自己做。
`

// buildInstruction 按委派模式生成系统提示词。
func buildInstruction(mode string) string {
	switch mode {
	case "always":
		return baseInstruction + alwaysDelegation
	case "conservative":
		return baseInstruction + "\n除非用户明确要求，否则不要派子 agent，自己完成任务。"
	default: // preferred
		return baseInstruction + preferredDelegation
	}
}

// 双协程架构：
//   主 goroutine —— program.Run()（BubbleTea 事件循环）
//   后台 goroutine —— agent.Run(ctx, input)，事件经 program.Send 桥接回 TUI
func main() {
	cfg := loadConfig()
	m, err := model.New(context.Background(), model.Config{
		Provider: string(cfg.Models[0].Provider),
		APIKey:   cfg.Models[0].APIKey,
		BaseURL:  cfg.Models[0].BaseURL,
		Model:    cfg.Models[0].ModelID,
	})
	if err != nil {
		log.Fatal(err)
	}

	bash := runtime.NewBash(".")

	// worker 工具（子 agent 用，无 task → 防递归）
	workerRegistry := tool.NewRegistry()
	for _, t := range tool.Builtins(bash) {
		workerRegistry.Register(t)
	}

	// 记忆库（跨会话持久）
	mem, err := memory.Open("memory.db")
	if err != nil {
		log.Printf("记忆库不可用，禁用记忆: %v", err)
		mem = nil
	}
	if mem != nil {
		defer mem.Close()
		workerRegistry.Register(tool.NewRememberTool(mem))
	}

	// MCP servers（worker 工具，故障隔离）
	for _, srv := range cfg.MCPServers {
		if err := tool.ConnectMCP(context.Background(), workerRegistry, srv); err != nil {
			log.Printf("MCP server %s 连接失败: %v", srv.Name, err)
		}
	}

	// 子 agent manager（子 agent 用 workerRegistry）
	mgr := subagent.NewManager(m, workerRegistry, mem, []subagent.SubagentSpec{
		{Name: "reviewer", Description: "代码审查", WhenToUse: "非平凡实现/改动后验收、代码审查", SystemPrompt: "你是代码审查专家，分析代码问题并给出结构化结论。"},
		{Name: "explorer", Description: "探索项目", WhenToUse: "探索未知代码库、定位相关代码", SystemPrompt: "你是项目探索专家，梳理项目结构、定位相关代码，给出简明结论。"},
		{Name: "planner", Description: "任务规划", WhenToUse: "把复杂任务拆解成步骤", SystemPrompt: "你是任务规划专家，把复杂任务拆解成清晰的步骤。"},
	})

	// orchestrator 工具（always 模式主 agent 用：只能 task + remember）
	orchestratorRegistry := tool.NewRegistry()
	orchestratorRegistry.Register(subagent.NewTaskTool(mgr))
	if mem != nil {
		orchestratorRegistry.Register(tool.NewRememberTool(mem))
	}

	// 主 agent 工具集（按委派模式）
	mainRegistry := orchestratorRegistry
	if cfg.DelegationMode != "always" {
		mainRegistry = tool.NewRegistry()
		for _, t := range workerRegistry.List() {
			mainRegistry.Register(t)
		}
		mainRegistry.Register(subagent.NewTaskTool(mgr))
	}

	// 审批模式（默认 yolo）
	mode := permission.ModeYolo
	switch cfg.ApprovalMode {
	case "always-ask":
		mode = permission.ModeAlwaysAsk
	case "write":
		mode = permission.ModeWrite
	}

	instr := buildInstruction(cfg.DelegationMode)
	ag := agent.New("codeclaw", instr, m, mainRegistry, mode, tui.NewApprover(), mem)

	// 固定会话文件，重启即恢复历史（多会话 /resume 在 P9）
	os.MkdirAll("sessions", 0755)
	st, err := session.NewFileStorage("sessions/default.jsonl")
	if err != nil {
		log.Fatal(err)
	}
	s, err := session.New("default", st)
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()

	// 上下文管理器：超阈值自动压缩
	cmgr := agentctx.New(s, agentctx.NewModelSummarizer(m), cfg.Models[0].ContextWindow, 16384)

	program := tea.NewProgram(tui.NewModel(ag, s, cmgr))
	tui.SetProgram(program)
	if _, err := program.Run(); err != nil {
		log.Fatal(err)
	}
}
