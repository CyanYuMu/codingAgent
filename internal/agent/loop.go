package agent

import (
	"context"
	"errors"
	"io"
	"time"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
	"einoclaw-build/internal/tool"
)

// Run 执行一次 run：以 cc 为真相源循环「重建输入 → 模型 → 记录 → 工具 → 记录」，流式吐出 AgentEvent。
// steer 通道非阻塞接收中途注入的修正消息（记录为用户消息，下一步模型调用会看到）。
// 调用方须在 Run 前把本轮用户消息 Record 进 cc。
func (a *Agent) Run(ctx context.Context, steer <-chan message.Message) <-chan AgentEvent {
	ch := make(chan AgentEvent, 16)
	go func() {
		defer close(ch)
		emit := func(e AgentEvent) {
			select {
			case ch <- e:
			case <-time.After(time.Second):
				// 消费者 1s 没读才丢弃；持久化已在循环内完成，丢事件只影响渲染
			}
		}
		emit(AgentEvent{Type: EventAgentStart})
		emit(AgentEvent{Type: EventTurnStart})
		a.loop(ctx, steer, emit)
		emit(AgentEvent{Type: EventTurnEnd})
		emit(AgentEvent{Type: EventAgentEnd})
	}()
	return ch
}

func (a *Agent) loop(ctx context.Context, steer <-chan message.Message, emit func(AgentEvent)) {
	var lastUsage model.Usage
	retries := 0
	for step := 0; step < a.maxIterations; step++ {
		// steering：非阻塞取修正，记录为用户消息
		if steer != nil {
			select {
			case sm := <-steer:
				_ = a.cc.Record(sm, model.Usage{})
			default:
			}
		}
		// mid-turn 压缩：上一步 usage 超阈值，在下一次模型调用前压缩
		if lastUsage.PromptTokens > 0 && a.cc.ShouldCompact(lastUsage) {
			if did, err := a.cc.Compact(ctx); err == nil && did {
				emit(AgentEvent{Type: EventCompaction, Compaction: &CompactionInfo{Reason: "mid-turn"}})
				lastUsage = model.Usage{}
			}
		}
		msgs, err := a.cc.Build(ctx)
		if err != nil {
			emit(AgentEvent{Type: EventError, Err: err})
			return
		}
		stream, err := a.model.Stream(ctx, msgs, a.tools.Specs())
		if err != nil {
			if a.handleModelError(ctx, err, &retries, emit) {
				step-- // 恢复/重试不计步
				continue
			}
			return
		}
		assistant, usage, streamErr := consumeStream(ctx, stream, emit)
		stream.Close()
		if streamErr != nil {
			if a.handleModelError(ctx, streamErr, &retries, emit) {
				step--
				continue
			}
			return
		}
		if ctx.Err() != nil && len(assistant.Blocks) == 0 {
			return // 取消且没有任何内容：不记录空消息
		}
		retries = 0
		lastUsage = usage
		if err := a.cc.Record(assistant, usage); err != nil {
			emit(AgentEvent{Type: EventError, Err: err})
			return
		}
		calls := toolCallsOf(assistant)
		if len(calls) == 0 {
			return // 无工具调用，turn 结束
		}
		// 三档中断「跳过」：已取消则不启动工具（回放时悬空调用会被合成 interrupted 结果）
		if ctx.Err() != nil {
			return
		}
		for _, tc := range calls {
			emit(AgentEvent{Type: EventToolStart, ToolStart: &ToolStart{ID: tc.ID, Name: tc.Name, Args: tc.Args}})
		}
		results := a.executor.ExecuteAll(ctx, calls)
		terminated := ""
		for i, tc := range calls {
			r := results[i]
			emit(AgentEvent{Type: EventToolEnd, ToolEnd: &ToolEnd{ID: tc.ID, Name: tc.Name, Content: r.Content, IsError: r.IsError}})
			if err := a.cc.Record(message.NewToolMessage(tc.ID, tc.Name, r.Content, r.IsError), model.Usage{}); err != nil {
				emit(AgentEvent{Type: EventError, Err: err})
				return
			}
			if r.IsError {
				continue
			}
			if t, ok := a.tools.Get(tc.Name); ok {
				if term, ok := t.(tool.Terminal); ok && term.IsTerminal() {
					terminated = tc.Name
				}
			}
		}
		if terminated != "" {
			emit(AgentEvent{Type: EventTerminated, Terminated: &TerminatedInfo{ToolName: terminated}})
			return
		}
	}
}

// handleModelError 分流模型错误：溢出 → 压缩恢复；瞬时 → 退避重试；其它 → EventError。
// 返回 true 表示应继续循环（本步不计数）。
func (a *Agent) handleModelError(ctx context.Context, err error, retries *int, emit func(AgentEvent)) bool {
	if ctx.Err() != nil {
		return false // 取消：安静退出
	}
	if model.IsContextOverflow(err) {
		did, cerr := a.cc.RecoverOverflow(ctx)
		if cerr == nil && did {
			emit(AgentEvent{Type: EventCompaction, Compaction: &CompactionInfo{Reason: "overflow"}})
			return true
		}
		emit(AgentEvent{Type: EventError, Err: err})
		return false
	}
	if model.IsRetryable(err) && *retries < a.maxRetries {
		*retries++
		delay := a.retryBase * time.Duration(1<<(*retries-1))
		emit(AgentEvent{Type: EventRetry, Retry: &RetryInfo{Attempt: *retries, Delay: delay, Err: err}})
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return false
		}
		return true
	}
	emit(AgentEvent{Type: EventError, Err: err})
	return false
}

// consumeStream 消费一个事件流，累积成完整消息并 emit 对应事件，返回累积消息 + 用量 + 流错误。
// 流中途出错时返回 error，让 Run 停止工具执行（参数可能被截断）；错误本身由调用方分流后再 emit。
func consumeStream(ctx context.Context, stream model.ModelStream, emit func(AgentEvent)) (message.Message, model.Usage, error) {
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
