package agent

import (
	"bytes"
	"context"
	"strings"
	"testing"

	ctxm "einoclaw-build/internal/context"
	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
	"einoclaw-build/internal/session"
	"einoclaw-build/internal/tool"
)

// stubSummarizer 计数的摘要器（冒烟里应只被调用一次：剪枝先行）。
type stubSummarizer struct{ calls int }

func (s *stubSummarizer) Summarize(context.Context, []message.Message) (string, error) {
	s.calls++
	return "SUMMARY", nil
}

// bigOutTool 输出 size 字节的大结果：撑上下文用。
type bigOutTool struct{ size int }

func (bigOutTool) Name() string        { return "big" }
func (bigOutTool) Description() string { return "big output" }
func (bigOutTool) Parameters() map[string]any {
	return map[string]any{"v": map[string]any{"type": "string"}}
}
func (bigOutTool) Required() []string            { return nil }
func (bigOutTool) Tier() permission.Tier         { return permission.TierRead }
func (bigOutTool) Concurrency() tool.Concurrency { return tool.ConcurrencyShared }
func (b bigOutTool) Execute(_ context.Context, _ map[string]any, sink *runtime.Sink) error {
	sink.Write(bytes.Repeat([]byte("x"), b.size))
	return nil
}

// usageCallStep 一次带用量的工具调用流：用量超阈驱动下一循环步的 mid-turn 压缩。
func usageCallStep(id, name string, usage model.Usage) func() (model.ModelStream, error) {
	return func() (model.ModelStream, error) {
		return &fakeStream{
			events: []model.ModelEvent{{ToolCalls: []model.ToolCallDelta{{Index: 0, CallID: id, Name: name, Args: "{}"}}}},
			usage:  usage,
		}, nil
	}
}

// TestMidTurnCompactionLadderSmoke 全链路冒烟：真 session + 真 Manager + 真循环 + 脚本化模型。
// 期望压缩阶梯：多轮 mid-turn 剪枝（零模型调用，重复进行直到剪无可剪）→ 最终 mid-turn 摘要；
// 全程回放保持 tool_call/tool_result 配对完整（这正是 P8 修的 400 类问题）。
func TestMidTurnCompactionLadderSmoke(t *testing.T) {
	s, _ := session.New("smoke", &session.MemoryStorage{})
	sum := &stubSummarizer{}
	// 窗口 500k → 阈值 425k；每个大结果 300k 估算 token（600k 字节 rune/2）
	cm := ctxm.New(s, sum, 500000, 40, nil)

	// 预置两轮大结果（≈600k token），再加上模型循环里产出的三轮，保证多轮剪枝
	for i := range 2 {
		_ = cm.Record(message.NewUserMessage("go"), model.Usage{})
		_ = cm.Record(message.Message{Role: message.RoleAssistant, Blocks: []message.ContentBlock{{
			Kind: message.BlockToolCall, ToolCall: &message.ToolCall{ID: "pre" + string(rune('0'+i)), Name: "big", Args: "{}"},
		}}}, model.Usage{})
		_ = cm.Record(message.NewToolMessage("pre"+string(rune('0'+i)), "big", strings.Repeat("x", 600000), false), model.Usage{})
	}
	_ = cm.Record(message.NewUserMessage("go"), model.Usage{})

	over := model.Usage{PromptTokens: 500000}  // 过阈：逼出压缩
	under := model.Usage{PromptTokens: 100000} // 压缩后回落：不再压缩
	fm := &fakeModel{steps: []func() (model.ModelStream, error){
		usageCallStep("c2", "big", over),   // 产出结果 → 下一步触发剪枝
		usageCallStep("c3", "big", over),   // 剪无可剪 → 下一步触发摘要
		usageCallStep("c4", "echo", under), // 用量回落 → 不再压缩
	}}
	reg := tool.NewRegistry()
	reg.Register(bigOutTool{size: 600000})
	reg.Register(echoTool{})
	a := New("t", fm, reg, tool.NewExecutor(reg, permission.ModeYolo, nil), cm)
	a.retryBase = 0

	var reasons []string
	for e := range a.Run(t.Context(), nil) {
		if e.Type == EventCompaction {
			reasons = append(reasons, e.Compaction.Reason)
		}
		if e.Type == EventError {
			t.Fatalf("意外错误：%v", e.Err)
		}
	}

	// 阶梯顺序：先剪枝（零模型调用）后摘要，且各一次
	if len(reasons) != 2 || reasons[0] != "mid-turn:prune" || reasons[1] != "mid-turn:summary" {
		t.Fatalf("压缩阶梯 = %v，want [mid-turn:prune mid-turn:summary]", reasons)
	}
	if sum.calls != 1 {
		t.Fatalf("摘要器应恰好调用 1 次，got %d", sum.calls)
	}

	// 会话里落了 prune 条目；回放以摘要开头、老结果为占位
	es, _ := s.Entries()
	prunes, compactions := 0, 0
	for _, e := range es {
		switch {
		case e.Type == session.EntryCustom && e.CustomType == "prune":
			prunes++
		case e.Type == session.EntryCompaction:
			compactions++
		}
	}
	if prunes != 1 || compactions != 1 {
		t.Fatalf("prune=%d compaction=%d，want 1/1", prunes, compactions)
	}
	ms, _ := s.Replay()
	if !strings.HasPrefix(ms[0].Blocks[0].Text, "SUMMARY") {
		t.Fatalf("回放应以摘要开头，got %q", ms[0].Blocks[0].Text)
	}
	if len(ms) >= len(es) { // 压缩必须让回放比原始条目数短
		t.Fatalf("回放未收缩：%d vs %d 条目", len(ms), len(es))
	}
	validateToolPairing(t, ms)
}

// validateToolPairing 断言回放里没有孤儿 tool_call：每个调用都能在紧随的 tool 消息里找到结果。
func validateToolPairing(t *testing.T, ms []message.Message) {
	t.Helper()
	for i, m := range ms {
		if m.Role != message.RoleAssistant {
			continue
		}
		for _, b := range m.Blocks {
			if b.Kind != message.BlockToolCall || b.ToolCall == nil {
				continue
			}
			found := false
			for j := i + 1; j < len(ms); j++ {
				if ms[j].Role == message.RoleUser || ms[j].Role == message.RoleAssistant {
					break
				}
				for _, rb := range ms[j].Blocks {
					if rb.Kind == message.BlockToolResult && rb.ToolResult != nil && rb.ToolResult.ToolCallID == b.ToolCall.ID {
						found = true
					}
				}
			}
			if !found {
				t.Fatalf("孤儿 tool_call %s（msg %d）——会被 API 拒绝", b.ToolCall.ID, i)
			}
		}
	}
}
