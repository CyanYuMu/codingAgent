package context

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
	"einoclaw-build/internal/session"
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

// --- Task 10：剪枝接进 Compact（先剪后摘） ---

// countSummarizer 记录调用次数：剪枝路径不该调它。
type countSummarizer struct{ calls int }

func (c *countSummarizer) Summarize(context.Context, []message.Message) (string, error) {
	c.calls++
	return "S", nil
}

func TestCompactPrunesBeforeSummarizing(t *testing.T) {
	s, _ := session.New("s1", &session.MemoryStorage{})
	fs := &countSummarizer{}
	cm := New(s, fs, 1000, 40, nil)
	cm.prune = PruneOpts{ProtectRecent: 1, MinSavings: 1, MinResult: 1}
	for i := range 3 {
		_ = cm.Record(ctxMsg(message.RoleUser, "u"+strings.Repeat("a", i)), model.Usage{})
		_ = cm.Record(callMsg(fmt.Sprintf("c%d", i), "bash", "{}"), model.Usage{})
		_ = cm.Record(message.NewToolMessage(fmt.Sprintf("c%d", i), "bash",
			strings.Repeat("x", 400)+fmt.Sprintf(" artifact://%d ", i), false), model.Usage{})
	}

	method, err := cm.Compact(context.Background())
	if err != nil || method != compactPrune {
		t.Fatalf("应先剪枝：method=%q err=%v", method, err)
	}
	if fs.calls != 0 {
		t.Fatalf("剪枝是零模型调用，不应调摘要器（调了 %d 次）", fs.calls)
	}
	es, _ := s.Entries()
	last := es[len(es)-1]
	if last.Type != session.EntryCustom || last.CustomType != "prune" {
		t.Fatalf("应落 prune 条目，got %+v", last)
	}

	msgs, _ := cm.Build(context.Background())
	if len(msgs) != 9 {
		t.Fatalf("剪枝不删消息，len=%d", len(msgs))
	}
	for _, i := range []int{2, 5} { // 老结果占位
		c := msgs[i].Blocks[0].ToolResult.Content
		if !strings.Contains(c, "已省略") {
			t.Fatalf("msgs[%d] 未被占位：%q", i, c)
		}
	}
	if c := msgs[8].Blocks[0].ToolResult.Content; !strings.Contains(c, "artifact://2") || strings.Contains(c, "已省略") {
		t.Fatalf("保护窗内结果应完整：%q", c)
	}

	// 剪无可剪后，同一会话再压缩应落到摘要（阶梯升级）
	method2, err := cm.Compact(context.Background())
	if err != nil || method2 != compactSummary {
		t.Fatalf("剪枝后仍应能摘要：method=%q err=%v", method2, err)
	}
	if fs.calls != 1 {
		t.Fatalf("第二次压缩应调摘要器 1 次，got %d", fs.calls)
	}
}

func TestCompactFallsBackToSummaryWhenPruneInsufficient(t *testing.T) {
	s, _ := session.New("s1", &session.MemoryStorage{})
	fs := &fakeSummarizer{out: "S"}
	cm := New(s, fs, 1000, 40, nil) // 默认剪枝参数：小结果全在保护窗内，省不够 20k
	for i := range 3 {
		_ = cm.Record(ctxMsg(message.RoleUser, "u"), model.Usage{})
		_ = cm.Record(callMsg(fmt.Sprintf("c%d", i), "read_file", "{}"), model.Usage{})
		_ = cm.Record(message.NewToolMessage(fmt.Sprintf("c%d", i), "read_file", strings.Repeat("v", 60), false), model.Usage{})
	}
	method, err := cm.Compact(context.Background())
	if err != nil || method != compactSummary {
		t.Fatalf("剪不够应走摘要：method=%q err=%v", method, err)
	}
	if fs.got == nil {
		t.Fatal("摘要器应被调用")
	}
}

func TestNilSummarizerStillPrunes(t *testing.T) {
	s, _ := session.New("s1", &session.MemoryStorage{})
	cm := New(s, nil, 1000, 40, nil) // 无摘要器，但剪枝不需要模型
	cm.prune = PruneOpts{ProtectRecent: 1, MinSavings: 1, MinResult: 1}
	_ = cm.Record(ctxMsg(message.RoleUser, "u1"), model.Usage{})
	_ = cm.Record(callMsg("c1", "bash", "{}"), model.Usage{})
	_ = cm.Record(message.NewToolMessage("c1", "bash", strings.Repeat("x", 400), false), model.Usage{})
	_ = cm.Record(ctxMsg(message.RoleUser, "u2"), model.Usage{})
	_ = cm.Record(callMsg("c2", "bash", "{}"), model.Usage{})
	_ = cm.Record(message.NewToolMessage("c2", "bash", strings.Repeat("y", 400), false), model.Usage{})

	method, err := cm.Compact(context.Background())
	if err != nil || method != compactPrune {
		t.Fatalf("nil summarizer 下剪枝仍可用：method=%q err=%v", method, err)
	}
	msgs, _ := cm.Build(context.Background())
	if c := msgs[2].Blocks[0].ToolResult.Content; !strings.Contains(c, "已省略") {
		t.Fatalf("老结果未占位：%q", c)
	}
}

// TestSessionPruneReplayMatchesApplyPrune 钉住一致性不变量：
// 回放（session 侧边界应用）与 ApplyPrune（计划侧按下标应用）对同一消息必须产出同一占位字节。
func TestSessionPruneReplayMatchesApplyPrune(t *testing.T) {
	s, _ := session.New("s1", &session.MemoryStorage{})
	cm := New(s, nil, 100000, 1000, nil)
	for i := range 4 {
		_ = cm.Record(ctxMsg(message.RoleUser, fmt.Sprintf("u%d", i)), model.Usage{})
		_ = cm.Record(callMsg(fmt.Sprintf("c%d", i), "bash", "{}"), model.Usage{})
		_ = cm.Record(message.NewToolMessage(fmt.Sprintf("c%d", i), "bash",
			strings.Repeat("z", 300)+fmt.Sprintf(" artifact://%d", i), false), model.Usage{})
	}
	before, _ := s.Replay()
	idx, _ := PlanPrune(before, PruneOpts{ProtectRecent: 1, MinSavings: 1, MinResult: 1})
	if len(idx) == 0 {
		t.Fatal("应存在可剪内容")
	}
	ids, _ := s.ContextEntryIDs()
	boundary := idx[len(idx)-1] + 1
	if boundary >= len(ids) {
		t.Fatalf("保护窗非空时边界必然存在：boundary=%d len=%d", boundary, len(ids))
	}
	if err := s.Prune(ids[boundary], 0); err != nil {
		t.Fatal(err)
	}

	after, _ := s.Replay()
	want := ApplyPrune(before, indexRange(0, boundary))
	if len(after) != len(want) {
		t.Fatalf("len mismatch: %d vs %d", len(after), len(want))
	}
	for i := range want {
		if after[i].Role != want[i].Role || len(after[i].Blocks) != len(want[i].Blocks) {
			t.Fatalf("msg[%d] 结构不一致", i)
		}
		for j := range want[i].Blocks {
			a, w := after[i].Blocks[j], want[i].Blocks[j]
			if a.Kind != w.Kind {
				t.Fatalf("msg[%d].Blocks[%d] kind mismatch", i, j)
			}
			if a.Kind == message.BlockToolResult && a.ToolResult.Content != w.ToolResult.Content {
				t.Fatalf("msg[%d].Blocks[%d] 占位不一致：\n got %q\nwant %q", i, j, a.ToolResult.Content, w.ToolResult.Content)
			}
		}
	}
}

func indexRange(from, to int) []int {
	out := make([]int, 0, to-from)
	for i := from; i < to; i++ {
		out = append(out, i)
	}
	return out
}

func TestPlanPruneSkipsPlaceholders(t *testing.T) {
	// 占位不再是候选：剪枝幂等，避免同一边界反复重写占位字节（前缀 churn）
	msgs := []message.Message{
		bigTool("c0", 300000), // 旧：真候选
		message.NewToolMessage("c1", "bash", PrunedPlaceholderOf("artifact://1", 204), false), // 中：占位，跳过
		bigTool("c2", 300000), // 新：受保护
	}
	idx, _ := PlanPrune(msgs, PruneOpts{ProtectRecent: 1, MinSavings: 1, MinResult: 1})
	if len(idx) != 1 || idx[0] != 0 {
		t.Fatalf("idx = %v, want [0]（占位跳过）", idx)
	}
	// 全是占位时根本没有可剪内容
	idx2, savings := PlanPrune([]message.Message{msgs[0]}, PruneOpts{ProtectRecent: 1, MinSavings: 1, MinResult: 1})
	if len(idx2) != 0 || savings != 0 {
		t.Fatalf("全占位不应再剪：idx=%v savings=%d", idx2, savings)
	}
}

// PrunedPlaceholderOf 便捷构造占位内容（与 session.PrunedPlaceholder 同源）。
func PrunedPlaceholderOf(ref string, tokens int) string {
	return session.PrunedPlaceholder("x "+ref, tokens)
}
