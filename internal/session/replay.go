package session

import "einoclaw-build/internal/message"

// pathToLeaf 从 leaf 沿 ParentID 回溯，返回 root→leaf 顺序的条目（遇重复 id 终止，防环）。
func pathToLeaf(entries []Entry, leafID string) []Entry {
	byID := make(map[string]Entry, len(entries))
	for _, e := range entries {
		if e.ID != "" && e.Type != EntrySession {
			byID[e.ID] = e
		}
	}
	var rev []Entry
	seen := map[string]bool{}
	for id := leafID; id != "" && !seen[id]; {
		seen[id] = true
		e, ok := byID[id]
		if !ok {
			break
		}
		rev = append(rev, e)
		id = e.ParentID
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}

// contextMsg 是回放出的一条模型消息及其来源条目 id（合成的修复消息沿用所属 assistant 的 id）。
type contextMsg struct {
	msg message.Message
	id  string
}

// buildContext 把 root→leaf 路径展开成模型上下文：
//  1. 最新 reset_boundary 之后才进入上下文；
//  2. 最新 compaction 展开为 [摘要] + 从 FirstKeptEntryID 起的 message 条目（v1 无起点：其后全部）；
//  3. header / session_init / custom / title_change 不产生消息；
//  4. 修复悬空 tool_call：没有配对结果的调用合成一条 error 结果。
func buildContext(path []Entry) []contextMsg {
	start := 0
	for i, e := range path {
		if e.Type == EntryReset {
			start = i + 1
		}
	}
	path = path[start:]

	cmpIdx := -1
	for i, e := range path {
		if e.Type == EntryCompaction && e.Compaction != nil {
			cmpIdx = i
		}
	}
	var out []contextMsg
	if cmpIdx >= 0 {
		c := path[cmpIdx]
		out = append(out, contextMsg{msg: message.NewUserMessage(c.Compaction.Summary), id: c.ID})
		keptFrom := cmpIdx + 1
		if c.Compaction.FirstKeptEntryID != "" {
			for i := 0; i < cmpIdx; i++ {
				if path[i].ID == c.Compaction.FirstKeptEntryID {
					keptFrom = i
					break
				}
			}
		}
		for i := keptFrom; i < len(path); i++ {
			if i == cmpIdx {
				continue
			}
			if e := path[i]; e.Type == EntryMessage && e.Message != nil {
				out = append(out, contextMsg{msg: *e.Message, id: e.ID})
			}
		}
	} else {
		for _, e := range path {
			if e.Type == EntryMessage && e.Message != nil {
				out = append(out, contextMsg{msg: *e.Message, id: e.ID})
			}
		}
	}
	return repairDangling(out)
}

// repairDangling 为没有配对 tool 结果的 tool_call 合成一条 error 结果（仅回放，不落盘）。
func repairDangling(in []contextMsg) []contextMsg {
	out := make([]contextMsg, 0, len(in))
	for i, cm := range in {
		out = append(out, cm)
		if cm.msg.Role != message.RoleAssistant {
			continue
		}
		for _, b := range cm.msg.Blocks {
			if b.Kind != message.BlockToolCall || b.ToolCall == nil {
				continue
			}
			if !hasResultAfter(in, i, b.ToolCall.ID) {
				out = append(out, contextMsg{
					msg: message.NewToolMessage(b.ToolCall.ID, b.ToolCall.Name, "[interrupted: tool did not run]", true),
					id:  cm.id,
				})
			}
		}
	}
	return out
}

// hasResultAfter 检查 from 之后、下一条非 tool 消息之前，是否出现 callID 的结果。
func hasResultAfter(in []contextMsg, from int, callID string) bool {
	for j := from + 1; j < len(in); j++ {
		m := in[j].msg
		if m.Role == message.RoleUser || m.Role == message.RoleAssistant {
			return false
		}
		for _, b := range m.Blocks {
			if b.Kind == message.BlockToolResult && b.ToolResult != nil && b.ToolResult.ToolCallID == callID {
				return true
			}
		}
	}
	return false
}
