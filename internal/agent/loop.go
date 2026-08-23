package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"einoclaw-build/internal/memory"
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
			case <-time.After(time.Second):
				// 消费者 1s 没读才丢弃；取消时也照发（保证 MessageEnd 等收尾事件持久化）
			}
		}

		emit(AgentEvent{Type: EventAgentStart})
		emit(AgentEvent{Type: EventTurnStart})

		msgs := []message.Message{message.NewSystemMessage(a.instruction)}
		// 召回记忆，注入 <memories> 背景块（让位于活状态）
		if a.memory != nil {
			if mems, err := a.memory.Recall(lastUserText(input), 5); err == nil && len(mems) > 0 {
				msgs = append(msgs, message.NewSystemMessage(renderMemories(mems)))
			}
		}
		msgs = append(msgs, input...)
		for step := 0; step < a.maxIterations; step++ {
			stream, err := a.model.Stream(ctx, msgs, a.tools.Specs())
			if err != nil {
				emit(AgentEvent{Type: EventError, Err: err})
				break
			}
			assistant, _, streamErr := consumeStream(ctx, stream, emit)
			stream.Close()
			if streamErr != nil {
				break // 流错误：不执行工具（参数可能被截断）
			}
			msgs = append(msgs, assistant)

			calls := toolCallsOf(assistant)
			if len(calls) == 0 {
				break // 无工具调用，turn 结束
			}
			// 三档中断「跳过」：未启动的工具不执行
			if ctx.Err() != nil {
				break
			}
			for _, tc := range calls {
				emit(AgentEvent{Type: EventToolStart, ToolStart: &ToolStart{ID: tc.ID, Name: tc.Name, Args: tc.Args}})
			}
			// 并行执行（Shared 并行 goroutine，Exclusive 串行）
			results := a.executor.ExecuteAll(ctx, calls)
			for i, tc := range calls {
				emit(AgentEvent{Type: EventToolEnd, ToolEnd: &ToolEnd{ID: tc.ID, Name: tc.Name, Content: results[i]}})
				msgs = append(msgs, message.NewToolMessage(tc.ID, tc.Name, results[i], false))
			}
		}

		emit(AgentEvent{Type: EventTurnEnd})
		emit(AgentEvent{Type: EventAgentEnd})
	}()
	return ch
}

// consumeStream 消费一个事件流，累积成完整消息并 emit 对应事件，返回累积消息 + 用量 + 流错误。
// 流中途出错时返回 error，让 Run 停止工具执行（参数可能被截断）。
func consumeStream(ctx context.Context, stream eventStream, emit func(AgentEvent)) (message.Message, model.Usage, error) {
	emit(AgentEvent{Type: EventMessageStart})
	acc := newStreamAccumulator()
	var streamErr error
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
			streamErr = err
			break
		}
		acc.add(ev)
		emit(AgentEvent{Type: EventMessageUpdate, Update: &MessageUpdate{Text: ev.Text, Thinking: ev.Thinking}})
	}
	m := acc.message()
	usage := stream.Usage()
	emit(AgentEvent{Type: EventMessageEnd, Ended: &MessageEnd{Message: m, Usage: usage}})
	return m, usage, streamErr
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

// lastUserText 返回最后一条 user 消息的文本（作为召回 query）。
func lastUserText(msgs []message.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == message.RoleUser {
			var sb strings.Builder
			for _, b := range msgs[i].Blocks {
				if b.Kind == message.BlockText {
					sb.WriteString(b.Text)
				}
			}
			return sb.String()
		}
	}
	return ""
}

// renderMemories 把召回的记忆渲染成 <memories> 背景块。
func renderMemories(mems []memory.Memory) string {
	var sb strings.Builder
	sb.WriteString("<memories>\n")
	for _, m := range mems {
		fmt.Fprintf(&sb, "- [%s] %s（置信 %.1f）\n", m.MemoryType, m.Content, m.Veracity)
	}
	sb.WriteString("</memories>\n（以上是背景上下文，当前用户消息和工具结果优先。）")
	return sb.String()
}
