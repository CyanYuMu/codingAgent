package main

import (
	"context"
	"log"

	tea "charm.land/bubbletea/v2"

	"einoclaw-build/internal/agent"
	"einoclaw-build/internal/model"
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

	ag := agent.New("codeclaw", agentInstruction, m)

	program := tea.NewProgram(tui.NewModel(ag))
	tui.SetProgram(program)
	if _, err := program.Run(); err != nil {
		log.Fatal(err)
	}
}
