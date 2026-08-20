package agent

import (
	"context"
	"errors"
	"io"
	"testing"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
)

// fakeStream 是 eventStream 的测试替身：按序吐出预置事件，最后 EOF 或错误。
type fakeStream struct {
	events []model.ModelEvent
	err    error
	i      int
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

func TestConsumeStreamEventSequence(t *testing.T) {
	fs := &fakeStream{events: []model.ModelEvent{
		{Text: "Hello"},
		{Thinking: "think"},
	}}
	var got []AgentEvent
	consumeStream(context.Background(), fs, func(e AgentEvent) { got = append(got, e) })

	wantTypes := []EventType{EventMessageStart, EventMessageUpdate, EventMessageUpdate, EventMessageEnd}
	if len(got) != len(wantTypes) {
		t.Fatalf("got %d events, want %d", len(got), len(wantTypes))
	}
	for i, wt := range wantTypes {
		if got[i].Type != wt {
			t.Fatalf("event[%d].Type = %v, want %v", i, got[i].Type, wt)
		}
	}
	if got[1].Update == nil || got[1].Update.Text != "Hello" {
		t.Fatalf("event[1] update = %+v", got[1].Update)
	}
	if got[2].Update == nil || got[2].Update.Thinking != "think" {
		t.Fatalf("event[2] update = %+v", got[2].Update)
	}
	// 定稿消息：块顺序 thinking → text
	m := got[3].Ended.Message
	if len(m.Blocks) != 2 {
		t.Fatalf("ended blocks = %d, want 2", len(m.Blocks))
	}
	if m.Blocks[0].Kind != message.BlockThinking || m.Blocks[0].Thinking != "think" {
		t.Fatalf("ended thinking = %+v", m.Blocks[0])
	}
	if m.Blocks[1].Kind != message.BlockText || m.Blocks[1].Text != "Hello" {
		t.Fatalf("ended text = %+v", m.Blocks[1])
	}
}

func TestConsumeStreamErrorEmitsError(t *testing.T) {
	fs := &fakeStream{err: errors.New("boom")}
	var got []AgentEvent
	consumeStream(context.Background(), fs, func(e AgentEvent) { got = append(got, e) })

	// 出错也发 message_end（定稿已累积内容）
	if len(got) != 3 { // message_start, error, message_end
		t.Fatalf("got %d events, want 3", len(got))
	}
	if got[0].Type != EventMessageStart || got[1].Type != EventError || got[1].Err == nil || got[2].Type != EventMessageEnd {
		t.Fatalf("events = %+v", got)
	}
}
