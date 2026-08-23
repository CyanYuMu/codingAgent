package context

import (
	"context"
	"strings"
	"testing"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
	"einoclaw-build/internal/session"
)

func ctxMsg(role message.Role, text string) message.Message {
	return message.Message{Role: role, Blocks: []message.ContentBlock{{Kind: message.BlockText, Text: text}}}
}

func callMsg(id, name, args string) message.Message {
	return message.Message{Role: message.RoleAssistant, Blocks: []message.ContentBlock{{Kind: message.BlockToolCall, ToolCall: &message.ToolCall{ID: id, Name: name, Args: args}}}}
}

type fakeSummarizer struct {
	got []message.Message
	out string
}

func (f *fakeSummarizer) Summarize(_ context.Context, msgs []message.Message) (string, error) {
	f.got = msgs
	return f.out, nil
}

func TestEstimateTokens(t *testing.T) {
	if got := estimateTokens(ctxMsg(message.RoleUser, "hello world")); got != 9 { // 11 runes/2 + 4
		t.Fatalf("estimateTokens = %d, want 9", got)
	}
}

func TestEstimateTokensCountsToolBlocks(t *testing.T) {
	tr := message.NewToolMessage("c1", "read", strings.Repeat("x", 200), false)
	if got := EstimateTokens(tr); got < 100 {
		t.Fatalf("tool result tokens = %d, want >= 100", got)
	}
	if got := EstimateTokens(callMsg("c1", "bash", strings.Repeat("y", 100))); got < 50 {
		t.Fatalf("tool call tokens = %d", got)
	}
}

func TestFindCutPoint(t *testing.T) {
	msgs := []message.Message{
		ctxMsg(message.RoleUser, "aaaa"), ctxMsg(message.RoleAssistant, "bbbb"), ctxMsg(message.RoleUser, "cccc"),
	}
	// 每条 4 runes/2+4 = 6 token；keep=8：cccc(6)<8 → +bbbb(12)>=8 → cut=1（assistant 无 tool_call，安全）
	if got := findCutPoint(msgs, 8); got != 1 {
		t.Fatalf("findCutPoint = %d, want 1", got)
	}
}

func TestFindCutPointNeverSplitsToolPair(t *testing.T) {
	msgs := []message.Message{
		ctxMsg(message.RoleUser, "u1"),
		callMsg("c1", "read", strings.Repeat("a", 40)),
		message.NewToolMessage("c1", "read", strings.Repeat("b", 400), false),
		ctxMsg(message.RoleAssistant, "a1"),
		ctxMsg(message.RoleUser, "u2"),
	}
	// keep 很小 → 候选落在 u2；安全
	if got := findCutPoint(msgs, 1); got != 4 {
		t.Fatalf("cut = %d, want 4", got)
	}
	// keep 覆盖到 tool 结果 → 候选 2（tool 消息）不安全；1 是带 tool_call 的 assistant 也不安全 → 回退到 0
	if got := findCutPoint(msgs, 300); got != 0 {
		t.Fatalf("cut = %d, want 0 (u1)", got)
	}
	// keep 刚好覆盖 u2+a1（5+5=10 ≥ 8）→ 候选 3（纯文本 assistant）安全
	if got := findCutPoint(msgs, 8); got != 3 {
		t.Fatalf("cut = %d, want 3", got)
	}
}

func TestSerializeIncludesToolCallsAndResults(t *testing.T) {
	s := serializeConversation([]message.Message{
		callMsg("c1", "read_file", `{"file_path":"a.go"}`),
		message.NewToolMessage("c1", "read_file", "package main", false),
	})
	if !strings.Contains(s, "tool_call read_file") || !strings.Contains(s, "package main") {
		t.Fatalf("serialized = %q", s)
	}
	long := serializeConversation([]message.Message{message.NewToolMessage("c1", "bash", strings.Repeat("z", 5000), false)})
	if !strings.Contains(long, "(elided)") || len(long) > 2000 {
		t.Fatalf("long result not clipped: len=%d", len(long))
	}
}

func TestThreshold(t *testing.T) {
	cm := New(nil, nil, 1000, 100, nil)
	if got := cm.threshold(); got != 500 { // 1000 - max(150,16384)→reserve>=window→500
		t.Fatalf("threshold = %d, want 500", got)
	}
}

func TestCompactWritesFirstKeptAndRebuilds(t *testing.T) {
	s, _ := session.New("s1", &session.MemoryStorage{})
	fs := &fakeSummarizer{out: "SUMMARY"}
	cm := New(s, fs, 1000, 6, func(context.Context) []message.Message { return []message.Message{message.NewSystemMessage("SYS")} })
	_ = cm.Record(ctxMsg(message.RoleUser, "m0"), model.Usage{})
	_ = cm.Record(ctxMsg(message.RoleAssistant, "m1"), model.Usage{})
	_ = cm.Record(ctxMsg(message.RoleUser, "m2"), model.Usage{})
	_ = cm.Record(ctxMsg(message.RoleAssistant, "m3"), model.Usage{})

	if !cm.ShouldCompact(model.Usage{PromptTokens: 600}) {
		t.Fatal("600 > 500 should compact")
	}
	if cm.ShouldCompact(model.Usage{PromptTokens: 100}) {
		t.Fatal("100 < 500 should not compact")
	}
	did, err := cm.Compact(context.Background())
	if err != nil || !did {
		t.Fatalf("compact did=%v err=%v", did, err)
	}
	if len(fs.got) != 2 || fs.got[0].Blocks[0].Text != "m0" || fs.got[1].Blocks[0].Text != "m1" {
		t.Fatalf("summarize input = %+v", fs.got)
	}
	msgs, _ := cm.Build(context.Background())
	want := []string{"SYS", "SUMMARY", "m2", "m3"}
	if len(msgs) != len(want) {
		t.Fatalf("build = %+v", msgs)
	}
	for i, w := range want {
		if msgs[i].Blocks[0].Text != w {
			t.Fatalf("build[%d] = %q want %q", i, msgs[i].Blocks[0].Text, w)
		}
	}
	// 压缩条目只追加一条，不重追加保留消息
	es, _ := s.Entries()
	if es[len(es)-1].Type != session.EntryCompaction || es[len(es)-1].Compaction.TokensBefore == 0 {
		t.Fatalf("last entry = %+v", es[len(es)-1])
	}
}

func TestCompactKeepsToolPairsIntact(t *testing.T) {
	s, _ := session.New("s1", &session.MemoryStorage{})
	fs := &fakeSummarizer{out: "S"}
	cm := New(s, fs, 1000, 40, nil)
	_ = cm.Record(ctxMsg(message.RoleUser, "u1"), model.Usage{})
	_ = cm.Record(ctxMsg(message.RoleAssistant, "a1"), model.Usage{})
	_ = cm.Record(ctxMsg(message.RoleUser, "u2"), model.Usage{})
	_ = cm.Record(callMsg("c1", "read", strings.Repeat("x", 20)), model.Usage{})
	_ = cm.Record(message.NewToolMessage("c1", "read", strings.Repeat("y", 60), false), model.Usage{})
	did, err := cm.Compact(context.Background())
	if err != nil || !did {
		t.Fatalf("did=%v err=%v", did, err)
	}
	msgs, _ := cm.Build(context.Background())
	// 保留段必须从 u2 开始（tool 对不可拆）
	if len(msgs) != 4 || msgs[0].Blocks[0].Text != "S" || msgs[1].Blocks[0].Text != "u2" || msgs[3].Role != message.RoleTool {
		t.Fatalf("build = %+v", msgs)
	}
}

func TestRecoverOverflowCutsDeeper(t *testing.T) {
	s, _ := session.New("s1", &session.MemoryStorage{})
	fs := &fakeSummarizer{out: "S"}
	cm := New(s, fs, 100000, 1000, nil)
	for range 6 {
		_ = cm.Record(ctxMsg(message.RoleUser, strings.Repeat("u", 50)), model.Usage{})
		_ = cm.Record(ctxMsg(message.RoleAssistant, strings.Repeat("a", 50)), model.Usage{})
	}
	// keep=1000 覆盖全部（总估算 ≈ 348）→ 正常 Compact 无可压内容
	if did, _ := cm.Compact(context.Background()); did {
		t.Fatal("nothing to compact at keep=1000")
	}
	did, err := cm.RecoverOverflow(context.Background())
	if err != nil || !did {
		t.Fatalf("recover did=%v err=%v", did, err)
	}
	msgs, _ := cm.Build(context.Background())
	if len(msgs) >= 12 {
		t.Fatalf("overflow recovery should shrink context, got %d", len(msgs))
	}
}

func TestSmallWindowStillCompacts(t *testing.T) {
	// 窗口 6000 → 阈值 3000 → 保留预算 min(16384, 1500)=1500；对话估算 ~4000 时必须能压
	s, _ := session.New("s1", &session.MemoryStorage{})
	fs := &fakeSummarizer{out: "S"}
	cm := New(s, fs, 6000, 16384, nil)
	for range 4 {
		_ = cm.Record(ctxMsg(message.RoleUser, "read next"), model.Usage{})
		_ = cm.Record(callMsg("c", "read_file", `{"file_path":"x.go"}`), model.Usage{})
		_ = cm.Record(message.NewToolMessage("c", "read_file", strings.Repeat("func X() {}\n", 150), false), model.Usage{})
	}
	if !cm.ShouldCompact(model.Usage{PromptTokens: 4500}) {
		t.Fatal("4500 > 3000 should compact")
	}
	did, err := cm.Compact(context.Background())
	if err != nil || !did {
		t.Fatalf("small window must compact: did=%v err=%v", did, err)
	}
	msgs, _ := cm.Build(context.Background())
	if len(msgs) >= 12 || msgs[0].Blocks[0].Text != "S" {
		t.Fatalf("build = %d msgs", len(msgs))
	}
	// 保留段仍以 user 开头（tool 对完整）
	if msgs[1].Role != message.RoleUser {
		t.Fatalf("kept segment must start at user, got %s", msgs[1].Role)
	}
}

func TestCalibrationShrinksKeepWhenProviderCountsHigher(t *testing.T) {
	s, _ := session.New("s1", &session.MemoryStorage{})
	cm := New(s, &fakeSummarizer{out: "S"}, 100000, 2000, nil)
	var msgs []message.Message
	for range 10 {
		msgs = append(msgs, ctxMsg(message.RoleUser, strings.Repeat("中", 400))) // 每条估算 204
	}
	// 无真值：原样
	if got := cm.keepInEstimateUnits(1000, msgs); got != 1000 {
		t.Fatalf("no calibration expected, got %d", got)
	}
	// 真值是估算的 4 倍 → 预算按估算单位缩到 1/4
	cm.ShouldCompact(model.Usage{PromptTokens: 2040 * 4})
	if got := cm.keepInEstimateUnits(1000, msgs); got < 240 || got > 260 {
		t.Fatalf("calibrated keep = %d, want ~250", got)
	}
}

func TestNilSummarizerNeverCompacts(t *testing.T) {
	s, _ := session.New("s1", &session.MemoryStorage{})
	cm := New(s, nil, 1000, 1, nil)
	_ = cm.Record(ctxMsg(message.RoleUser, "u"), model.Usage{})
	_ = cm.Record(ctxMsg(message.RoleAssistant, "a"), model.Usage{})
	if did, err := cm.Compact(context.Background()); did || err != nil {
		t.Fatalf("did=%v err=%v", did, err)
	}
}

func TestSummarizePromptRequiresSixFields(t *testing.T) {
	p := summarizePrompt([]message.Message{ctxMsg(message.RoleUser, "hi")})
	if len(p) != 2 || p[0].Role != message.RoleSystem {
		t.Fatalf("prompt = %+v", p)
	}
	sys := p[0].Blocks[0].Text
	for _, field := range []string{"目标", "当前状态", "决策", "文件", "失败", "下一步"} {
		if !strings.Contains(sys, field) {
			t.Errorf("prompt 缺少字段 %q", field)
		}
	}
}
