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

func TestFindCutPoint(t *testing.T) {
	msgs := []message.Message{
		ctxMsg(message.RoleUser, "aaaa"), ctxMsg(message.RoleAssistant, "bbbb"), ctxMsg(message.RoleUser, "cccc"),
	}
	// 每条 4 runes/2+4 = 6 token；keep=8：cccc(6)<8 → +bbbb(12)>=8 → cut=1
	if got := findCutPoint(msgs, 8); got != 1 {
		t.Fatalf("findCutPoint = %d, want 1", got)
	}
}

func TestThreshold(t *testing.T) {
	cm := New(nil, nil, 1000, 100)
	if got := cm.threshold(); got != 500 { // 1000 - max(150,16384)→reserve>=window→500
		t.Fatalf("threshold = %d, want 500", got)
	}
}

func TestAfterTurnCompactsWhenOverThreshold(t *testing.T) {
	s, _ := session.New("s1", &session.MemoryStorage{})
	_ = s.Append(ctxMsg(message.RoleUser, "m0"))
	_ = s.Append(ctxMsg(message.RoleAssistant, "m1"))
	_ = s.Append(ctxMsg(message.RoleUser, "m2"))
	_ = s.Append(ctxMsg(message.RoleAssistant, "m3"))

	fs := &fakeSummarizer{out: "SUMMARY"}
	cm := New(s, fs, 1000, 6) // 每条 5 token，keep=6 → 保留 m2,m3

	// usage.PromptTokens=600 > threshold=500 → 压缩
	if err := cm.AfterTurn(context.Background(), model.Usage{PromptTokens: 600}); err != nil {
		t.Fatal(err)
	}
	// 摘要输入应是更早的 [m0,m1]
	if len(fs.got) != 2 || fs.got[0].Blocks[0].Text != "m0" || fs.got[1].Blocks[0].Text != "m1" {
		t.Fatalf("summarize input = %+v", fs.got)
	}
	// replay 应是 [SUMMARY, m2, m3]
	ms, _ := s.Replay()
	if len(ms) != 3 || ms[0].Blocks[0].Text != "SUMMARY" || ms[1].Blocks[0].Text != "m2" {
		t.Fatalf("replay = %+v", ms)
	}
}

func TestAfterTurnNoCompactWhenUnderThreshold(t *testing.T) {
	s, _ := session.New("s1", &session.MemoryStorage{})
	_ = s.Append(ctxMsg(message.RoleUser, "m0"))
	fs := &fakeSummarizer{out: "X"}
	cm := New(s, fs, 1000, 6)
	if err := cm.AfterTurn(context.Background(), model.Usage{PromptTokens: 100}); err != nil {
		t.Fatal(err)
	}
	if fs.got != nil {
		t.Fatalf("summarizer should not be called, got %+v", fs.got)
	}
}

func TestSummarizePromptRequiresSixFields(t *testing.T) {
	p := summarizePrompt([]message.Message{ctxMsg(message.RoleUser, "hi")})
	if len(p) != 2 {
		t.Fatalf("len = %d, want 2", len(p))
	}
	if p[0].Role != message.RoleSystem {
		t.Fatalf("p[0] role = %q, want system", p[0].Role)
	}
	sys := p[0].Blocks[0].Text
	for _, field := range []string{"目标", "当前状态", "决策", "文件", "失败", "下一步"} {
		if !strings.Contains(sys, field) {
			t.Errorf("prompt 缺少字段 %q", field)
		}
	}
}
