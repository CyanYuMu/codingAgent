package agent

import (
	"context"
	"errors"
	"io"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
)

// eventStream 是 Run 依赖的最小流接口；*model.Stream 满足它。
// 抽成接口是为了让 consumeStream 可单测（测试注入 fakeStream）。
type eventStream interface {
	Recv() (model.ModelEvent, error)
}

// Run 执行一次 run：把 system 指令 + 输入消息发给模型，流式吐出 AgentEvent。
// input 是 system 之后的消息（P1 传单条 user；P2 传历史+user）。
func (a *Agent) Run(ctx context.Context, input []message.Message) <-chan AgentEvent {
	ch := make(chan AgentEvent, 16)
	go func() {
		defer close(ch)
		emit := func(e AgentEvent) {
			select {
			case ch <- e:
			case <-ctx.Done():
			}
		}

		emit(AgentEvent{Type: EventAgentStart})
		emit(AgentEvent{Type: EventTurnStart})

		msgs := append([]message.Message{message.NewSystemMessage(a.instruction)}, input...)
		stream, err := a.model.Stream(ctx, msgs, nil)
		if err != nil {
			emit(AgentEvent{Type: EventError, Err: err})
		} else {
			defer stream.Close()
			consumeStream(ctx, stream, emit)
		}

		emit(AgentEvent{Type: EventTurnEnd})
		emit(AgentEvent{Type: EventAgentEnd})
	}()
	return ch
}

// consumeStream 消费一个事件流，累积成完整消息并 emit 对应事件。
func consumeStream(ctx context.Context, stream eventStream, emit func(AgentEvent)) {
	emit(AgentEvent{Type: EventMessageStart})
	acc := newStreamAccumulator()
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				break // 取消：正常收尾，不报错
			}
			emit(AgentEvent{Type: EventError, Err: err})
			break
		}
		acc.add(ev)
		emit(AgentEvent{Type: EventMessageUpdate, Update: &MessageUpdate{Text: ev.Text, Thinking: ev.Thinking}})
	}
	emit(AgentEvent{Type: EventMessageEnd, Ended: &MessageEnd{Message: acc.message()}})
}
