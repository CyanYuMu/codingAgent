package session

import (
	"strings"
	"testing"

	"einoclaw-build/internal/message"
)

// entryIDOfText 返回第一条文本为 text 的 user 消息所在条目 id（测试搭场景用）。
func entryIDOfText(t *testing.T, s *Session, text string) string {
	t.Helper()
	es, err := s.Entries()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		if e.Type != EntryMessage || e.Message == nil || len(e.Message.Blocks) == 0 {
			continue
		}
		if e.Message.Blocks[0].Kind == message.BlockText && e.Message.Blocks[0].Text == text {
			return e.ID
		}
	}
	t.Fatalf("entry with text %q not found", text)
	return ""
}

// appendToolExchange 追加一对 tool_call + 大结果（结果里带 artifact 指针）。
func appendToolExchange(t *testing.T, s *Session, callID, content string) {
	t.Helper()
	if err := s.Append(toolCallMsg(callID, "bash")); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(message.NewToolMessage(callID, "bash", content, false)); err != nil {
		t.Fatal(err)
	}
}

func TestReplayAppliesPrune(t *testing.T) {
	s, _ := New("s1", &MemoryStorage{})
	big1 := strings.Repeat("x", 300) + " artifact://7"
	big2 := strings.Repeat("y", 300) + " artifact://8"
	_ = s.Append(msg(message.RoleUser, "u1"))
	appendToolExchange(t, s, "c1", big1)
	_ = s.Append(msg(message.RoleUser, "u2"))
	appendToolExchange(t, s, "c2", big2)
	_ = s.Append(msg(message.RoleUser, "u3"))

	// 边界 = u2：u2 之前的结果占位，之后完整
	if err := s.Prune(entryIDOfText(t, s, "u2"), 300); err != nil {
		t.Fatal(err)
	}

	ms, err := s.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 7 {
		t.Fatalf("剪枝只替换内容不删消息，len=%d", len(ms))
	}
	// 配对完整：assistant(tool_call) 后面仍跟着 role=tool 的结果
	if ms[1].Role != message.RoleAssistant || ms[2].Role != message.RoleTool {
		t.Fatalf("pairing broken: %+v %+v", ms[1], ms[2])
	}
	got1 := ms[2].Blocks[0].ToolResult.Content
	want1 := PrunedPlaceholder(big1, toolResultTokens(message.NewToolMessage("c1", "bash", big1, false)))
	if got1 != want1 || !strings.Contains(got1, "artifact://7") {
		t.Fatalf("pruned content = %q, want %q", got1, want1)
	}
	if got2 := ms[5].Blocks[0].ToolResult.Content; got2 != big2 {
		t.Fatalf("boundary 之后应完整，got %q", got2)
	}
	// 原始条目不被污染：JSONL 仍是真相源，审计/回溯可拿全文
	es, _ := s.Entries()
	for _, e := range es {
		if e.Message == nil || len(e.Message.Blocks) == 0 {
			continue
		}
		if b := e.Message.Blocks[0]; b.Kind == message.BlockToolResult && b.ToolResult != nil && b.ToolResult.Content == big1 {
			return // 找到原文即通过
		}
	}
	t.Fatal("原始条目里的工具结果被剪枝污染了")
}

func TestPruneIsMonotonic(t *testing.T) {
	s, _ := New("s1", &MemoryStorage{})
	big1 := strings.Repeat("x", 200) + " artifact://1"
	big2 := strings.Repeat("y", 200) + " artifact://2"
	_ = s.Append(msg(message.RoleUser, "u1"))
	appendToolExchange(t, s, "c1", big1)
	_ = s.Append(msg(message.RoleUser, "u2"))
	appendToolExchange(t, s, "c2", big2)
	_ = s.Append(msg(message.RoleUser, "u3"))

	_ = s.Prune(entryIDOfText(t, s, "u2"), 100) // 第一条边界：只剪 c1
	_ = s.Prune(entryIDOfText(t, s, "u3"), 100) // 边界前进：c1、c2 都在界内

	ms, _ := s.Replay()
	if len(ms) != 7 {
		t.Fatalf("len=%d", len(ms))
	}
	for i, want := range map[int]string{2: big1, 5: big2} {
		got := ms[i].Blocks[0].ToolResult.Content
		if got == want || !strings.Contains(got, "已省略") {
			t.Fatalf("最新边界应覆盖 msg[%d]，got %q", i, got)
		}
	}
	// 边界本身（u3）之后没有消息；u2/u3 文本不受影响
	if ms[3].Blocks[0].Text != "u2" || ms[6].Blocks[0].Text != "u3" {
		t.Fatalf("非工具消息被改动: %+v", ms)
	}
}

func TestPruneUnknownBoundaryIsNoop(t *testing.T) {
	s, _ := New("s1", &MemoryStorage{})
	big := strings.Repeat("x", 200)
	_ = s.Append(msg(message.RoleUser, "u1"))
	appendToolExchange(t, s, "c1", big)
	_ = s.Prune("bogus-id", 100)
	ms, _ := s.Replay()
	if got := ms[2].Blocks[0].ToolResult.Content; got != big {
		t.Fatalf("未知边界不应生效，got %q", got)
	}
}

func TestPruneAfterCompactionAppliesToKeptSegment(t *testing.T) {
	s, _ := New("s1", &MemoryStorage{})
	big1 := strings.Repeat("x", 200) + " artifact://1"
	big2 := strings.Repeat("y", 200) + " artifact://2"
	_ = s.Append(msg(message.RoleUser, "u1"))
	appendToolExchange(t, s, "c1", big1)
	_ = s.Append(msg(message.RoleUser, "u2"))
	appendToolExchange(t, s, "c2", big2)
	_ = s.Append(msg(message.RoleUser, "u3"))

	// 压缩保留 u2 起，随后剪枝边界 = u3：保留段里的 c2 结果也该占位
	if err := s.Compact("SUMMARY", entryIDOfText(t, s, "u2"), 500); err != nil {
		t.Fatal(err)
	}
	if err := s.Prune(entryIDOfText(t, s, "u3"), 100); err != nil {
		t.Fatal(err)
	}
	ms, _ := s.Replay()
	if len(ms) != 5 { // 摘要 + u2 + c2 调用 + c2 结果 + u3
		t.Fatalf("len=%d: %+v", len(ms), ms)
	}
	if got := ms[3].Blocks[0].ToolResult.Content; got == big2 || !strings.Contains(got, "已省略") {
		t.Fatalf("压缩保留段内的剪枝未生效，got %q", got)
	}
}

func TestPruneStaleBoundaryAfterDeeperCompactionIsNoop(t *testing.T) {
	s, _ := New("s1", &MemoryStorage{})
	big := strings.Repeat("x", 200) + " artifact://1"
	_ = s.Append(msg(message.RoleUser, "u1"))
	appendToolExchange(t, s, "c1", big)
	_ = s.Append(msg(message.RoleUser, "u2"))
	_ = s.Append(msg(message.RoleUser, "u3"))

	// 先剪枝（边界 = u2），后压缩把保留点移到 u3：剪枝区已被摘要吃掉，prune 不应再动回放
	_ = s.Prune(entryIDOfText(t, s, "u2"), 100)
	if err := s.Compact("SUMMARY", entryIDOfText(t, s, "u3"), 500); err != nil {
		t.Fatal(err)
	}
	ms, _ := s.Replay()
	if len(ms) != 2 || ms[0].Blocks[0].Text != "SUMMARY" || ms[1].Blocks[0].Text != "u3" {
		t.Fatalf("replay = %+v", ms)
	}
}

func TestPrunedPlaceholderKeepsArtifactRef(t *testing.T) {
	got := PrunedPlaceholder("noise artifact://12 tail", 500)
	if !strings.Contains(got, "约 500 tokens") || !strings.Contains(got, "artifact://12") {
		t.Fatalf("placeholder = %q", got)
	}
	if got := PrunedPlaceholder("no ref here", 5); strings.Contains(got, "artifact") {
		t.Fatalf("无指针不应附注: %q", got)
	}
}
