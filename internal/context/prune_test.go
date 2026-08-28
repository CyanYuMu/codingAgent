package context

import (
	"strings"
	"testing"

	"einoclaw-build/internal/message"
)

// bigTool 构造一条内容为 runes 个 'x' 的工具结果消息；估算 token = runes/2 + 4。
func bigTool(id string, runes int) message.Message {
	return message.NewToolMessage(id, "bash", strings.Repeat("x", runes), false)
}

func TestPlanPruneProtectsRecent(t *testing.T) {
	// 最近的 60004 token 结果受保护；更早的 150004 token 结果是唯一候选。
	msgs := []message.Message{
		bigTool("c0", 300000), // 150004 token，旧
		bigTool("c1", 120000), // 60004 token，最近（≥ 保护窗 40000）
	}
	idx, savings := PlanPrune(msgs, PruneOpts{})
	if len(idx) != 1 || idx[0] != 0 {
		t.Fatalf("idx = %v, want [0]（只剪旧的，最近的结果受保护）", idx)
	}
	if savings < 150000 {
		t.Fatalf("savings = %d, want ≥ 150000", savings)
	}
}

func TestPlanPruneSkipsSmallResults(t *testing.T) {
	// 旧结果 9 token < MinResult(50) 应被跳过；最近的大结果受保护 → 无候选。
	msgs := []message.Message{
		bigTool("c0", 10),     // 9 token，旧，太小
		bigTool("c1", 100000), // 50004 token，最近（保护窗）
	}
	idx, savings := PlanPrune(msgs, PruneOpts{})
	if len(idx) != 0 || savings != 0 {
		t.Fatalf("idx=%v savings=%d, want 空集（小结果不剪）", idx, savings)
	}
}

func TestPlanPruneRequiresMinSavings(t *testing.T) {
	// 唯一候选只省 15004 token，低于 MinSavings(20000) → 空集。
	msgs := []message.Message{
		bigTool("c0", 30000),  // 15004 token，旧候选
		bigTool("c1", 100000), // 50004 token，最近（保护窗）
	}
	idx, savings := PlanPrune(msgs, PruneOpts{})
	if len(idx) != 0 || savings != 0 {
		t.Fatalf("idx=%v savings=%d, want 空集（省不够不剪）", idx, savings)
	}
}

func TestPlanPruneReturnsAscendingIndices(t *testing.T) {
	// 两个候选都应被选中，且下标按从旧到新（升序）返回——Task 10 用 idx[0] 作剪枝边界。
	msgs := []message.Message{
		bigTool("c0", 200000), // 100004 token，旧候选
		bigTool("c1", 200000), // 100004 token，旧候选
		bigTool("c2", 120000), // 60004 token，最近（保护窗）
	}
	idx, _ := PlanPrune(msgs, PruneOpts{})
	if len(idx) != 2 || idx[0] != 0 || idx[1] != 1 {
		t.Fatalf("idx = %v, want [0 1]（升序）", idx)
	}
}

func TestApplyPruneKeepsArtifactPointer(t *testing.T) {
	content := "前 4000 字节…\n...[完整输出已保存: artifact://7 ；用 read_file ...]"
	out := ApplyPrune([]message.Message{message.NewToolMessage("c", "bash", content, false)}, []int{0})
	got := out[0].Blocks[0].ToolResult.Content
	if !strings.Contains(got, "[输出已省略") {
		t.Fatalf("缺少占位标记：%q", got)
	}
	if !strings.Contains(got, "artifact://7") {
		t.Fatalf("artifact 指针应保留：%q", got)
	}
	if strings.Contains(got, "前 4000 字节") {
		t.Fatalf("原内容应被省略：%q", got)
	}
}

func TestApplyPrunePlaceholderWithoutArtifact(t *testing.T) {
	out := ApplyPrune([]message.Message{message.NewToolMessage("c", "bash", strings.Repeat("y", 2000), false)}, []int{0})
	got := out[0].Blocks[0].ToolResult.Content
	if !strings.Contains(got, "[输出已省略") || strings.Contains(got, "（完整内容") {
		t.Fatalf("无 artifact 指针时占位应不带「完整内容」：%q", got)
	}
}

func TestApplyPruneDoesNotMutateInput(t *testing.T) {
	content := strings.Repeat("y", 2000)
	msgs := []message.Message{message.NewToolMessage("c", "bash", content, false)}
	orig := msgs[0].Blocks[0].ToolResult.Content
	ApplyPrune(msgs, []int{0})
	if msgs[0].Blocks[0].ToolResult.Content != orig {
		t.Fatal("ApplyPrune 不应修改输入切片")
	}
}

func TestApplyPruneKeepsPairing(t *testing.T) {
	// 工具消息仍在，只是内容变了；tool_call/tool_result 配对不拆。
	full := strings.Repeat("z", 3000)
	msgs := []message.Message{
		callMsg("c1", "bash", `{"command":"go test"}`),
		message.NewToolMessage("c1", "bash", full, false),
	}
	out := ApplyPrune(msgs, []int{1})
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2（不删消息）", len(out))
	}
	if out[0].Blocks[0].ToolCall == nil || out[0].Blocks[0].ToolCall.ID != "c1" {
		t.Fatal("tool_call 不应被改动")
	}
	tr := out[1].Blocks[0].ToolResult
	if tr == nil || tr.ToolCallID != "c1" || tr.Name != "bash" || tr.IsError {
		t.Fatalf("tool_result 元信息应保留：%+v", tr)
	}
	if tr.Content == full || !strings.Contains(tr.Content, "[输出已省略") {
		t.Fatalf("内容应被占位替换：%q", tr.Content)
	}
}
