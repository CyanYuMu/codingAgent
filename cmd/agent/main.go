package main

import (
	"context"
	"log"
	"os"

	tea "charm.land/bubbletea/v2"

	"einoclaw-build/internal/agent"
	agentctx "einoclaw-build/internal/context"
	"einoclaw-build/internal/model"
	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
	"einoclaw-build/internal/session"
	"einoclaw-build/internal/tool"
	"einoclaw-build/internal/tui"
)

const agentInstruction = "你是一个编程智能体, 你的名字叫做 codeclaw, 擅长解决编程问题。"

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

	// 工具注册表 + bash 运行时
	registry := tool.NewRegistry()
	bash := runtime.NewBash(".")
	for _, t := range tool.Builtins(bash) {
		registry.Register(t)
	}

	// 审批模式（默认 yolo）
	mode := permission.ModeYolo
	switch cfg.ApprovalMode {
	case "always-ask":
		mode = permission.ModeAlwaysAsk
	case "write":
		mode = permission.ModeWrite
	}

	ag := agent.New("codeclaw", agentInstruction, m, registry, mode)

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
