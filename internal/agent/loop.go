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
	Usage() model.Usage
}

// Run 执行一次 run：把 system 指令 + 输入消息发给模型，流式吐出 AgentEvent。
// P4 起支持工具循环：模型返回工具调用 → 逐个执行 → 结果喂回 → 继续，直到无工具调用或达到上限。
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
		for step := 0; step < a.maxIterations; step++ {
			stream, err := a.model.Stream(ctx, msgs, a.tools.Specs())
			if err != nil {
				emit(AgentEvent{Type: EventError, Err: err})
				break
			}
			assistant, _ := consumeStream(ctx, stream, emit)
			stream.Close()
			msgs = append(msgs, assistant)

			calls := toolCallsOf(assistant)
			if len(calls) == 0 {
				break // 无工具调用，turn 结束
			}
			for _, tc := range calls {
				if ctx.Err() != nil {
					break // 三档中断「跳过」：未启动的工具不执行
				}
				emit(AgentEvent{Type: EventToolStart, ToolStart: &ToolStart{ID: tc.ID, Name: tc.Name, Args: tc.Args}})
				result := a.executor.Execute(ctx, tc)
				emit(AgentEvent{Type: EventToolEnd, ToolEnd: &ToolEnd{ID: tc.ID, Name: tc.Name, Content: result}})
				msgs = append(msgs, message.NewToolMessage(tc.ID, tc.Name, result, false))
			}
		}

		emit(AgentEvent{Type: EventTurnEnd})
		emit(AgentEvent{Type: EventAgentEnd})
	}()
	return ch
}

// consumeStream 消费一个事件流，累积成完整消息并 emit 对应事件，返回累积消息 + 用量。
func consumeStream(ctx context.Context, stream eventStream, emit func(AgentEvent)) (message.Message, model.Usage) {
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
	m := acc.message()
	usage := stream.Usage()
	emit(AgentEvent{Type: EventMessageEnd, Ended: &MessageEnd{Message: m, Usage: usage}})
	return m, usage
}

// toolCallsOf 提取消息里的工具调用块。
func toolCallsOf(m message.Message) []message.ToolCall {
	var calls []message.ToolCall
	for _, b := range m.Blocks {
		if b.Kind == message.BlockToolCall && b.ToolCall != nil {
			calls = append(calls, *b.ToolCall)
		}
	}
	return calls
}
