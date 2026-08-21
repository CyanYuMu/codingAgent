package context

import (
	"strings"

	"einoclaw-build/internal/message"
)

// findCutPoint 从最新消息往前累积 token，直到 >= keepTokens，返回保留区起始索引。
// 返回 0 表示「无更早内容可压」。
func findCutPoint(msgs []message.Message, keepTokens int) int {
	acc := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		acc += estimateTokens(msgs[i])
		if acc >= keepTokens {
			return i
		}
	}
	return 0
}

// serializeConversation 把消息序列化成纯文本（role: text 每行）。
func serializeConversation(msgs []message.Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		sb.WriteString(string(m.Role))
		sb.WriteString(": ")
		sb.WriteString(messageText(m))
		sb.WriteString("\n")
	}
	return sb.String()
}

// messageText 拼接消息里所有文本块。
func messageText(m message.Message) string {
	var sb strings.Builder
	for _, b := range m.Blocks {
		if b.Kind == message.BlockText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// sixFieldInstruction 是压缩摘要的指令：面向「未来 Agent 继续任务」的有损压缩，
// 而非泛化聊天总结。让模型按六个字段输出，保留继续任务所必需的信息。
const sixFieldInstruction = `你是一个面向编程任务的上下文压缩器。你的任务不是总结聊天，而是在有限 token 预算下，尽可能保留「未来 Agent 继续完成任务所必需的信息」。

请严格按以下 6 个字段输出（每个字段用 "# " 标题开头，字段内控制在几行以内）：

# 目标 / 任务
（当前在完成什么、最终要达成什么）

# 当前状态
（已经完成了什么、正在进行什么）

# 决策 / 约束
（已做出的关键决策、用户或环境施加的约束）

# 文件 / 产物
（涉及/创建/修改了哪些文件或产物）

# 失败 / 发现
（哪些尝试失败了、有哪些重要发现，避免重复踩坑）

# 下一步
（接下来具体要做什么，按优先级）

只保留继续任务所需的信息；不相关的内容丢弃。`

// summarizePrompt 构造摘要请求的消息：六字段系统指令 + 待压缩的对话。
func summarizePrompt(msgs []message.Message) []message.Message {
	return []message.Message{
		message.NewSystemMessage(sixFieldInstruction),
		message.NewUserMessage(serializeConversation(msgs)),
	}
}
