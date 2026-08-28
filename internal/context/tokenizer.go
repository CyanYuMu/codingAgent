package context

import "einoclaw-build/internal/message"

// EstimateTokens 粗估一条消息的 token：所有块按 2 rune ≈ 1 token（中英混合近似），每消息 +4 framing。
// 只用于找压缩切点与审计；预算判断的真值是 provider usage。
func EstimateTokens(m message.Message) int {
	n := 0
	for _, b := range m.Blocks {
		switch b.Kind {
		case message.BlockText:
			n += len([]rune(b.Text)) / 2
		case message.BlockThinking:
			n += len([]rune(b.Thinking)) / 2
		case message.BlockToolCall:
			if b.ToolCall != nil {
				n += (len(b.ToolCall.Name) + len([]rune(b.ToolCall.Args))) / 2
			}
		case message.BlockToolResult:
			if b.ToolResult != nil {
				n += len([]rune(b.ToolResult.Content)) / 2
			}
		}
	}
	return n + 4
}

func estimateTokens(m message.Message) int { return EstimateTokens(m) }
