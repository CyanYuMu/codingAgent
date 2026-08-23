package agent

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
	"einoclaw-build/internal/tool"
)

// fakeStream 是 ModelStream 的测试替身：按序吐出预置事件，最后 EOF 或错误。
type fakeStream struct {
	events []model.ModelEvent
	err    error
	i      int
	usage  model.Usage
}

func (f *fakeStream) Recv() (model.ModelEvent, error) {
	if f.i < len(f.events) {
		ev := f.events[f.i]
		f.i++
		return ev, nil
	}
	if f.err != nil {
		return model.ModelEvent{}, f.err
	}
	return model.ModelEvent{}, io.EOF
}
func (f *fakeStream) Usage() model.Usage { return f.usage }
func (f *fakeStream) Close()             {}

// fakeModel 按步返回脚本：每步要么一个 stream，要么一个 Stream() 错误；脚本耗尽后返回纯文本 "done"。
type fakeModel struct {
	steps []func() (model.ModelStream, error)
	calls [][]message.Message
}

func (f *fakeModel) Stream(_ context.Context, msgs []message.Message, _ []model.ToolSpec) (model.ModelStream, error) {
	f.calls = append(f.calls, msgs)
	if len(f.steps) == 0 {
		return &fakeStream{events: []model.ModelEvent{{Text: "done"}}}, nil
	}
	s := f.steps[0]
	f.steps = f.steps[1:]
	return s()
}

func textStep(t string) func() (model.ModelStream, error) {
	return func() (model.ModelStream, error) { return &fakeStream{events: []model.ModelEvent{{Text: t}}}, nil }
}

func callStep(id, name, args string) func() (model.ModelStream, error) {
	return func() (model.ModelStream, error) {
		return &fakeStream{events: []model.ModelEvent{{ToolCalls: []model.ToolCallDelta{{Index: 0, CallID: id, Name: name, Args: args}}}}}, nil
	}
}

func errStep(err error) func() (model.ModelStream, error) {
	return func() (model.ModelStream, error) { return nil, err }
}

// echoTool 回显参数；terminal=true 时是终止型工具 finish。
type echoTool struct{ terminal bool }

func (e echoTool) Name() string {
	if e.terminal {
		return "finish"
	}
	return "echo"
}
func (echoTool) Description() string            { return "" }
func (echoTool) Parameters() map[string]any     { return map[string]any{"v": map[string]any{"type": "string"}} }
func (echoTool) Tier() permission.Tier          { return permission.TierRead }
func (echoTool) Concurrency() tool.Concurrency  { return tool.ConcurrencyShared }
func (e echoTool) IsTerminal() bool             { return e.terminal }
func (echoTool) Execute(_ context.Context, args map[string]any, sink *runtime.Sink) error {
	v, _ := args["v"].(string)
	sink.Write([]byte("echo:" + v))
	return nil
}

func newTestAgent(fm *fakeModel, cc Context) *Agent {
	reg := tool.NewRegistry()
	reg.Register(echoTool{})
	reg.Register(echoTool{terminal: true})
	a := New("t", fm, reg, tool.NewExecutor(reg, permission.ModeYolo, nil), cc)
	a.retryBase = 0 // 测试不等待
	return a
}

func drain(ch <-chan AgentEvent) []AgentEvent {
	var out []AgentEvent
	for e := range ch {
		out = append(out, e)
	}
	return out
}

func hasEvent(evs []AgentEvent, pred func(AgentEvent) bool) bool {
	return slices.ContainsFunc(evs, pred)
}

func TestLoopRecordsToolRoundTrip(t *testing.T) {
	fm := &fakeModel{steps: []func() (model.ModelStream, error){callStep("c1", "echo", `{"v":"hi"}`), textStep("ok")}}
	cc := NewMemoryContext([]message.Message{message.NewSystemMessage("SYS")})
	_ = cc.Record(message.NewUserMessage("go"), model.Usage{})
	evs := drain(newTestAgent(fm, cc).Run(context.Background(), nil))
	msgs, _ := cc.Build(context.Background())
	// SYS, user, assistant(call), tool(result), assistant(ok)
	if len(msgs) != 5 || msgs[3].Role != message.RoleTool || msgs[3].Blocks[0].ToolResult.Content != "echo:hi" {
		t.Fatalf("context = %+v", msgs)
	}
	if len(fm.calls) != 2 || len(fm.calls[1]) != 4 {
		t.Fatalf("second model call should see 4 msgs, got %d", len(fm.calls[1]))
	}
	if !hasEvent(evs, func(e AgentEvent) bool { return e.Type == EventAgentEnd }) {
		t.Fatal("no agent_end")
	}
	if !hasEvent(evs, func(e AgentEvent) bool { return e.Type == EventToolEnd && e.ToolEnd.Content == "echo:hi" && !e.ToolEnd.IsError }) {
		t.Fatal("no tool_end with content")
	}
}

func TestLoopStopsOnTerminalTool(t *testing.T) {
	fm := &fakeModel{steps: []func() (model.ModelStream, error){callStep("c1", "finish", `{"v":"x"}`), textStep("SHOULD NOT RUN")}}
	cc := NewMemoryContext(nil)
	_ = cc.Record(message.NewUserMessage("go"), model.Usage{})
	evs := drain(newTestAgent(fm, cc).Run(context.Background(), nil))
	if len(fm.calls) != 1 {
		t.Fatalf("model called %d times, want 1", len(fm.calls))
	}
	if !hasEvent(evs, func(e AgentEvent) bool { return e.Type == EventTerminated && e.Terminated.ToolName == "finish" }) {
		t.Fatal("no EventTerminated")
	}
	// 终止型工具的结果也被记录（保持配对）
	if msgs := cc.Messages(); len(msgs) != 3 || msgs[2].Role != message.RoleTool {
		t.Fatalf("messages = %+v", msgs)
	}
}

func TestLoopRecoversFromOverflow(t *testing.T) {
	fm := &fakeModel{steps: []func() (model.ModelStream, error){errStep(errors.New("context_length_exceeded")), textStep("after")}}
	cc := NewMemoryContext(nil)
	cc.recoverOK = true
	_ = cc.Record(message.NewUserMessage("go"), model.Usage{})
	evs := drain(newTestAgent(fm, cc).Run(context.Background(), nil))
	if cc.recovers != 1 || len(fm.calls) != 2 {
		t.Fatalf("recovers=%d calls=%d", cc.recovers, len(fm.calls))
	}
	if !hasEvent(evs, func(e AgentEvent) bool { return e.Type == EventCompaction && e.Compaction.Reason == "overflow" }) {
		t.Fatal("no overflow compaction event")
	}
	if hasEvent(evs, func(e AgentEvent) bool { return e.Type == EventError }) {
		t.Fatal("recovered overflow should not surface EventError")
	}
}

func TestLoopOverflowUnrecoverableIsError(t *testing.T) {
	fm := &fakeModel{steps: []func() (model.ModelStream, error){errStep(errors.New("maximum context length exceeded"))}}
	cc := NewMemoryContext(nil) // recoverOK=false
	_ = cc.Record(message.NewUserMessage("go"), model.Usage{})
	evs := drain(newTestAgent(fm, cc).Run(context.Background(), nil))
	if len(fm.calls) != 1 || !hasEvent(evs, func(e AgentEvent) bool { return e.Type == EventError }) {
		t.Fatalf("calls=%d evs=%+v", len(fm.calls), evs)
	}
}

func TestLoopRetriesTransientError(t *testing.T) {
	fm := &fakeModel{steps: []func() (model.ModelStream, error){errStep(errors.New("429 Too Many Requests")), textStep("after")}}
	cc := NewMemoryContext(nil)
	_ = cc.Record(message.NewUserMessage("go"), model.Usage{})
	evs := drain(newTestAgent(fm, cc).Run(context.Background(), nil))
	if !hasEvent(evs, func(e AgentEvent) bool { return e.Type == EventRetry && e.Retry.Attempt == 1 }) || len(fm.calls) != 2 {
		t.Fatalf("calls=%d evs=%+v", len(fm.calls), evs)
	}
}

func TestLoopGivesUpAfterMaxRetries(t *testing.T) {
	e := errors.New("503 service unavailable")
	fm := &fakeModel{steps: []func() (model.ModelStream, error){errStep(e), errStep(e), errStep(e), errStep(e)}}
	cc := NewMemoryContext(nil)
	_ = cc.Record(message.NewUserMessage("go"), model.Usage{})
	evs := drain(newTestAgent(fm, cc).Run(context.Background(), nil))
	if len(fm.calls) != 4 || !hasEvent(evs, func(ev AgentEvent) bool { return ev.Type == EventError }) {
		t.Fatalf("calls=%d", len(fm.calls))
	}
}

func TestLoopMidTurnCompaction(t *testing.T) {
	fm := &fakeModel{steps: []func() (model.ModelStream, error){
		func() (model.ModelStream, error) {
			return &fakeStream{
				events: []model.ModelEvent{{ToolCalls: []model.ToolCallDelta{{Index: 0, CallID: "c1", Name: "echo", Args: `{"v":"1"}`}}}},
				usage:  model.Usage{PromptTokens: 999},
			}, nil
		},
		textStep("end"),
	}}
	cc := NewMemoryContext(nil)
	cc.compactAt = 500
	_ = cc.Record(message.NewUserMessage("go"), model.Usage{})
	evs := drain(newTestAgent(fm, cc).Run(context.Background(), nil))
	if !hasEvent(evs, func(e AgentEvent) bool { return e.Type == EventCompaction && e.Compaction.Reason == "mid-turn" }) || cc.compacts != 1 {
		t.Fatalf("compacts=%d evs=%+v", cc.compacts, evs)
	}
}

func TestLoopCancelSkipsTools(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fm := &fakeModel{steps: []func() (model.ModelStream, error){
		func() (model.ModelStream, error) {
			cancel() // 模型返回后立即取消
			return &fakeStream{events: []model.ModelEvent{{ToolCalls: []model.ToolCallDelta{{Index: 0, CallID: "c1", Name: "echo", Args: `{"v":"1"}`}}}}}, nil
		},
	}}
	cc := NewMemoryContext(nil)
	_ = cc.Record(message.NewUserMessage("go"), model.Usage{})
	evs := drain(newTestAgent(fm, cc).Run(ctx, nil))
	if hasEvent(evs, func(e AgentEvent) bool { return e.Type == EventToolStart }) {
		t.Fatal("tools must not start after cancel")
	}
	if len(fm.calls) != 1 {
		t.Fatalf("calls=%d", len(fm.calls))
	}
}

func TestSteeringIsRecordedBeforeNextStep(t *testing.T) {
	fm := &fakeModel{steps: []func() (model.ModelStream, error){callStep("c1", "echo", `{"v":"1"}`), textStep("end")}}
	cc := NewMemoryContext(nil)
	_ = cc.Record(message.NewUserMessage("go"), model.Usage{})
	steer := make(chan message.Message, 1)
	steer <- message.NewUserMessage("also do X")
	drain(newTestAgent(fm, cc).Run(context.Background(), steer))
	if !hasText(cc.Messages(), "also do X") {
		t.Fatalf("steering not recorded: %+v", cc.Messages())
	}
}

func hasText(msgs []message.Message, text string) bool {
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Kind == message.BlockText && b.Text == text {
				return true
			}
		}
	}
	return false
}

func TestConsumeStreamEventSequence(t *testing.T) {
	fs := &fakeStream{events: []model.ModelEvent{{Text: "Hello"}, {Thinking: "think"}}}
	var got []AgentEvent
	_, _, streamErr := consumeStream(context.Background(), fs, func(e AgentEvent) { got = append(got, e) })
	if streamErr != nil {
		t.Fatalf("正常流不应返回错误，got %v", streamErr)
	}
	wantTypes := []EventType{EventMessageStart, EventMessageUpdate, EventMessageUpdate, EventMessageEnd}
	if len(got) != len(wantTypes) {
		t.Fatalf("got %d events, want %d", len(got), len(wantTypes))
	}
	for i, wt := range wantTypes {
		if got[i].Type != wt {
			t.Fatalf("event[%d].Type = %v, want %v", i, got[i].Type, wt)
		}
	}
	m := got[3].Ended.Message
	if len(m.Blocks) != 2 || m.Blocks[0].Kind != message.BlockThinking || m.Blocks[1].Text != "Hello" {
		t.Fatalf("ended = %+v", m)
	}
}

func TestConsumeStreamErrorReturnsError(t *testing.T) {
	fs := &fakeStream{err: errors.New("boom")}
	var got []AgentEvent
	_, _, streamErr := consumeStream(context.Background(), fs, func(e AgentEvent) { got = append(got, e) })
	if streamErr == nil {
		t.Fatal("流错误应返回 error")
	}
	// 出错也发 message_end（定稿已累积内容）；错误事件由循环分流后发出
	if len(got) != 2 || got[0].Type != EventMessageStart || got[1].Type != EventMessageEnd {
		t.Fatalf("events = %+v", got)
	}
}

func TestToolCallsOf(t *testing.T) {
	m := message.Message{
		Role: message.RoleAssistant,
		Blocks: []message.ContentBlock{
			{Kind: message.BlockText, Text: "let me check"},
			{Kind: message.BlockToolCall, ToolCall: &message.ToolCall{ID: "c1", Name: "read", Args: "{}"}},
			{Kind: message.BlockToolCall, ToolCall: &message.ToolCall{ID: "c2", Name: "bash", Args: "{}"}},
		},
	}
	calls := toolCallsOf(m)
	if len(calls) != 2 || calls[0].Name != "read" || calls[1].Name != "bash" {
		t.Fatalf("calls = %+v", calls)
	}
}
