package context

import (
	"slices"
	"strings"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/session"
)

// PruneOpts 剪枝参数：先保护最近一段工具结果，再剪掉更早的大块结果（零模型调用）。
// 这是长上下文收缩里最便宜的一级：只替换内容、不调模型、不删消息，配对永不被拆。
type PruneOpts struct {
	ProtectRecent int // 保护最近 N 估算 token 的工具结果（默认 40000）
	MinSavings    int // 至少省这么多估算 token 才执行（默认 20000）
	MinResult     int // 单条估算 token 小于这个的结果不剪（默认 50）
}

// defaultPruneOpts 填默认值。
func defaultPruneOpts(o PruneOpts) PruneOpts {
	if o.ProtectRecent <= 0 {
		o.ProtectRecent = 40000
	}
	if o.MinSavings <= 0 {
		o.MinSavings = 20000
	}
	if o.MinResult <= 0 {
		o.MinResult = 50
	}
	return o
}

// PlanPrune 从新到旧扫描，返回应被省略的工具结果所在的**消息下标集合**（升序）与预计节省量。
// 规则：最近 ProtectRecent 估算 token 的工具结果受保护；更早的、单条 ≥ MinResult 的进候选；
// 候选总节省 < MinSavings 时返回空集（省不够就不剪，避免前缀 churn）。
// 只替换内容不删消息，所以 tool_call/tool_result 配对天然不拆。
func PlanPrune(msgs []message.Message, o PruneOpts) ([]int, int) {
	o = defaultPruneOpts(o)
	protected := 0
	var idx []int
	savings := 0
	for j := len(msgs) - 1; j >= 0; j-- {
		tok := toolResultTokens(msgs[j])
		if tok == 0 || allPruned(msgs[j]) {
			continue // 非工具结果消息，或已是占位（剪枝幂等：占位再剪只会改写字节）
		}
		if protected < o.ProtectRecent {
			protected += tok
			continue
		}
		if tok < o.MinResult {
			continue
		}
		idx = append(idx, j)
		savings += tok
	}
	if savings < o.MinSavings {
		return nil, 0
	}
	slices.Reverse(idx) // 从新到旧扫描得到降序下标，翻回升序：idx[0] 是最旧的剪枝边界
	return idx, savings
}

// ApplyPrune 把指定下标的工具结果内容替换成占位，返回新切片（不改原切片）。
// 消息结构与 tool_call/tool_result 配对保持不变，只有内容被省略。
func ApplyPrune(msgs []message.Message, idx []int) []message.Message {
	set := make(map[int]bool, len(idx))
	for _, i := range idx {
		set[i] = true
	}
	out := make([]message.Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		if set[i] {
			out[i] = pruneMessage(out[i])
		}
	}
	return out
}

// toolResultTokens 返回一条消息里所有工具结果的估算 token（rune/2 + framing）。
// 不含工具结果的消息返回 0。
func toolResultTokens(m message.Message) int {
	total, has := 0, false
	for _, b := range m.Blocks {
		if b.Kind == message.BlockToolResult && b.ToolResult != nil {
			total += len([]rune(b.ToolResult.Content)) / 2
			has = true
		}
	}
	if !has {
		return 0
	}
	return total + 4
}

// allPruned 判断消息里的工具结果是否已全部是剪枝占位。
func allPruned(m message.Message) bool {
	seen := false
	for _, b := range m.Blocks {
		if b.Kind != message.BlockToolResult || b.ToolResult == nil {
			continue
		}
		seen = true
		if !strings.HasPrefix(b.ToolResult.Content, session.PrunedMarker) {
			return false
		}
	}
	return seen
}

// pruneMessage 深拷贝一条消息，并把其中所有工具结果块的内容替换成占位。
// 占位文本复用 session.PrunedPlaceholder：计划（这里）与回放（session）必须产出同一字节。
func pruneMessage(m message.Message) message.Message {
	tok := toolResultTokens(m)
	blocks := make([]message.ContentBlock, len(m.Blocks))
	copy(blocks, m.Blocks)
	for i := range blocks {
		b := &blocks[i]
		if b.Kind != message.BlockToolResult || b.ToolResult == nil {
			continue
		}
		tr := *b.ToolResult
		tr.Content = session.PrunedPlaceholder(b.ToolResult.Content, tok)
		b.ToolResult = &tr
	}
	m.Blocks = blocks
	return m
}
