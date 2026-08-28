package context

import (
	"fmt"
	"strings"

	"einoclaw-build/internal/message"
)

// findCutPoint 从新到旧累计 token 到 keepTokens 得到候选切点，再向旧回退到安全切点；返回保留区起始下标。
// 安全切点：user 消息，或没有 tool_call 的 assistant 消息——绝不切在 tool 消息上，也绝不把
// 「带 tool_call 的 assistant」与它的结果拆开。返回 0 表示无可压内容。
func findCutPoint(msgs []message.Message, keepTokens int) int {
	acc := 0
	i := 0
	for j := len(msgs) - 1; j >= 0; j-- {
		acc += estimateTokens(msgs[j])
		if acc >= keepTokens {
			i = j
			break
		}
	}
	for i > 0 && !safeCut(msgs, i) {
		i--
	}
	return i
}

// safeCut 判断以 msgs[i] 开头的保留段是否合法。
func safeCut(msgs []message.Message, i int) bool {
	m := msgs[i]
	switch m.Role {
	case message.RoleUser:
		return true
	case message.RoleAssistant:
		for _, b := range m.Blocks {
			if b.Kind == message.BlockToolCall {
				return false
			}
		}
		return true
	}
	return false
}

const (
	resultHead = 1000
	resultTail = 500
	argsMax    = 300
)

// serializeConversation 把消息序列化成摘要器输入：含工具调用与（截断的）工具结果，不含 thinking。
func serializeConversation(msgs []message.Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		for _, b := range m.Blocks {
			switch b.Kind {
			case message.BlockText:
				if b.Text != "" {
					fmt.Fprintf(&sb, "%s: %s\n", m.Role, b.Text)
				}
			case message.BlockToolCall:
				if b.ToolCall != nil {
					fmt.Fprintf(&sb, "assistant→tool_call %s(%s)\n", b.ToolCall.Name, clip(b.ToolCall.Args, argsMax, 0))
				}
			case message.BlockToolResult:
				if b.ToolResult != nil {
					fmt.Fprintf(&sb, "tool(%s): %s\n", b.ToolResult.Name, clip(b.ToolResult.Content, resultHead, resultTail))
				}
			}
		}
	}
	return sb.String()
}

// clip 按 rune 截断：保留头 head 与尾 tail，中间用标记省略。
func clip(s string, head, tail int) string {
	r := []rune(s)
	if len(r) <= head+tail {
		return s
	}
	if tail == 0 {
		return string(r[:head]) + "…"
	}
	return string(r[:head]) + "\n…(elided)…\n" + string(r[len(r)-tail:])
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
（涉及/创建/修改了哪些文件或产物，尽量给出路径；区分只读过的与改过的）

# 失败 / 发现
（哪些尝试失败了、有哪些重要发现，避免重复踩坑）

# 下一步
（接下来具体要做什么，按优先级）

只保留继续任务所需的信息；不相关的内容丢弃。工具结果只保留结论，不复述原文。`

// summarizePrompt 构造摘要请求的消息：六字段系统指令 + 待压缩的对话。
func summarizePrompt(msgs []message.Message) []message.Message {
	return []message.Message{
		message.NewSystemMessage(sixFieldInstruction),
		message.NewUserMessage("<conversation>\n" + serializeConversation(msgs) + "</conversation>"),
	}
}
