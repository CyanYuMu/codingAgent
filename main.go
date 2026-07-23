package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"gopkg.in/yaml.v3"
)

// 全局状态：阶段1先放到全局变量，后续阶段会随架构演进调整。
var (
	cfg       *config
	baseModel model.BaseModel[adk.AgenticMessage]
	agent     adk.TypedAgent[adk.AgenticMessage]
	ctx       = context.Background()
)

func main() {
	loadConfig()
	loadModel(0)
	loadAgent()

	fmt.Println("einoclaw (阶段1) 已就绪。输入消息开始对话，输入 /exit 退出。")
	scanner := bufio.NewScanner(os.Stdin)
	// 单行可能较长，调大 buffer，避免超长输入被截断
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for {
		fmt.Print("\n你: ")
		if !scanner.Scan() {
			break
		}
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		if text == "/exit" {
			break
		}
		runOnce(text)
	}
	if err := scanner.Err(); err != nil {
		log.Println(err)
	}
}

// loadConfig 读取 ~/.einoclaw-build/config.yaml；不存在则提示用户先创建。
func loadConfig() {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}
	configPath := filepath.Join(home, ".einoclaw-build", "config.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Printf("未找到配置文件: %s\n请参考项目目录下的 example.yaml 创建并填入模型配置后重试。\n", configPath)
			os.Exit(0)
		}
		log.Fatal(err)
	}
	cfg = &config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		log.Fatal(err)
	}
	if len(cfg.Models) == 0 || cfg.Models[0].APIKey == "" {
		fmt.Println("请在配置文件中填入模型相关配置后重试。")
		os.Exit(0)
	}
}

// loadAgent 用全局 baseModel 装配一个最简单的聊天 Agent。
// 阶段1不带任何工具/中间件，因此它只会“调一次模型然后回答”。
func loadAgent() {
	var err error
	agent, err = adk.NewTypedChatModelAgent(ctx, &adk.TypedChatModelAgentConfig[adk.AgenticMessage]{
		Name:        "einoclaw",
		Description: "a code agent which can do many things",
		Instruction: "你是一个编程智能体, 你的名字叫做 einoclaw, 擅长解决编程问题。",
		Model:       baseModel,
	})
	if err != nil {
		log.Fatal(err)
	}
}

// runOnce 跑一轮对话：把用户消息喂给 agent，消费事件流，打印 AI 回复。
//
// 【eino 概念】agent.Run 不返回单个结果，而是返回一个 *AsyncIterator[*TypedAgentEvent]。
// 你需要循环调用 .Next()，每次拿到一个事件。事件有三种载荷：
//   - event.Err     : 出错
//   - event.Action  : 需要“动作”（后续阶段的权限审批/摘要会用到），阶段1不会出现
//   - event.Output  : 模型输出（MessageOutput）
//
// 这种“一切皆事件流”的设计，是后续支持流式输出、工具调用、中断恢复的基础。
func runOnce(query string) {
	iter := agent.Run(ctx, &adk.TypedAgentInput[adk.AgenticMessage]{
		Messages: []adk.AgenticMessage{
			schema.UserAgenticMessage(query),
		},
		EnableStreaming: false, // 阶段1先用非流式，拿到完整消息；阶段2再切流式
	})

	fmt.Print("AI: ")
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			fmt.Println("\n[错误]", event.Err)
			return
		}
		// 阶段1没有中间件，Action 不会出现；Output 为空的事件也直接跳过
		if event.Action != nil || event.Output == nil || event.Output.MessageOutput == nil {
			continue
		}
		mv := event.Output.MessageOutput
		// 非流式模式下，完整消息在 mv.Message 里
		if mv.IsStreaming || mv.Message == nil {
			continue
		}
		// 【eino 概念】AgenticMessage 不是纯字符串，而是由多个 ContentBlock 组成。
		// AI 正文文本在 AssistantGenText 块里；思考过程在 Reasoning 块里；工具调用在 FunctionToolCall 块里。
		for _, block := range mv.Message.ContentBlocks {
			if block.AssistantGenText != nil {
				fmt.Print(block.AssistantGenText.Text)
			}
		}
	}
	fmt.Println()
}
