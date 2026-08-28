package session

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"einoclaw-build/internal/message"
)

// PruneData 是 prune 自定义条目的负载：剪枝边界（第一个保留的消息条目 id）与预计节省量。
// 边界只前进不后退——回放按最新边界统一占位，提示词前缀单调，不会来回抖。
type PruneData struct {
	BeforeEntryID string `json:"beforeEntryId,omitempty"`
	Savings       int    `json:"savings"`
}

// Prune 落一条剪枝边界：回放时位于边界之前的工具结果内容被替换成占位。
// 消息结构与 tool_call/tool_result 配对不变，原始条目不改动（可审计、可回溯）。
func (s *Session) Prune(beforeEntryID string, savings int) error {
	return s.AppendCustom("prune", PruneData{BeforeEntryID: beforeEntryID, Savings: savings})
}

// applyPruneBoundaries 应用路径上的剪枝边界：所有 prune 条目里最靠后的边界生效
// （单调前进时即最新一条）。位于边界之前的工具结果替换为占位。
// 边界条目不在回放里（早于压缩保留点，那一段本就不回放）或负载非法的 prune 不生效——
// 防御性跳过，绝不因坏数据丢内容。返回新切片，不改动入参。
func applyPruneBoundaries(path []Entry, out []contextMsg) []contextMsg {
	bound := -1
	for _, e := range path {
		if e.Type != EntryCustom || e.CustomType != "prune" {
			continue
		}
		var d PruneData
		if json.Unmarshal(e.Data, &d) != nil || d.BeforeEntryID == "" {
			continue
		}
		for i, cm := range out {
			if cm.id == d.BeforeEntryID {
				bound = max(bound, i)
				break
			}
		}
	}
	if bound <= 0 {
		return out
	}
	res := make([]contextMsg, len(out))
	copy(res, out)
	for i := range res[:bound] {
		res[i] = pruneContextMsg(res[i])
	}
	return res
}

// pruneContextMsg 返回占位替换后的消息副本。Blocks 深拷贝——
// 回放消息与存储条目共享 Blocks 底层数组，直接改会污染 JSONL 真相源。
func pruneContextMsg(cm contextMsg) contextMsg {
	tok := toolResultTokens(cm.msg)
	if tok == 0 {
		return cm
	}
	m := cm.msg
	blocks := make([]message.ContentBlock, len(m.Blocks))
	copy(blocks, m.Blocks)
	for i := range blocks {
		b := &blocks[i]
		if b.Kind != message.BlockToolResult || b.ToolResult == nil {
			continue
		}
		tr := *b.ToolResult
		tr.Content = PrunedPlaceholder(tr.Content, tok)
		b.ToolResult = &tr
	}
	m.Blocks = blocks
	return contextMsg{msg: m, id: cm.id}
}

// toolResultTokens 估算一条消息里工具结果的 token（rune/2，带结果的消息 +4 framing）。
// 与 context 包的估算保持一致——一致性由 context 包的对照测试钉住。
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

// artifactRefRE 匹配内容里的 artifact://<数字 id> 指针。
var artifactRefRE = regexp.MustCompile(`artifact://\d+`)

// PrunedMarker 是剪枝占位的固定前缀。PlanPrune 据此跳过已剪枝的结果——
// 占位再剪也不省 token，只会改写字节（提示词前缀 churn），剪枝必须幂等。
const PrunedMarker = "[输出已省略："

// PrunedPlaceholder 构造剪枝占位：说明省略量，并保留 artifact:// 指针（read_file 可按指针读回）。
// context 包的 ApplyPrune 复用此函数——占位格式必须单一事实源，否则计划与回放不一致。
func PrunedPlaceholder(content string, tokens int) string {
	if strings.HasPrefix(content, PrunedMarker) {
		return content // 已是占位：原样返回，保证幂等
	}
	s := fmt.Sprintf("%s约 %d tokens]", PrunedMarker, tokens)
	if ref := artifactRefRE.FindString(content); ref != "" {
		s += fmt.Sprintf("（完整内容 %s）", ref)
	}
	return s
}
