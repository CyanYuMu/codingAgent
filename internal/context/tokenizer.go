package context

import "einoclaw-build/internal/message"

// estimateTokens 粗略估算一条消息的 token 数。只用于找压缩切点，真值靠 provider usage。
// 启发式：2 个 rune ≈ 1 token（中英混合的粗略近似）+ 每消息 framing 开销。
func estimateTokens(m message.Message) int {
	n := 0
	for _, b := range m.Blocks {
		if b.Kind == message.BlockText {
			n += len([]rune(b.Text)) / 2
		}
	}
	return n + 4
}
