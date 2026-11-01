package agent

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
)

type auditRecorder interface{ RecordAudit(string, any) error }

var runSequence atomic.Uint64

func errorCategory(err error) string {
	if model.IsContextOverflow(err) {
		return "context_overflow"
	}
	if model.IsRetryable(err) {
		return "retryable"
	}
	return "fatal"
}

func (a *Agent) runAudited(ctx context.Context, steer <-chan message.Message, observe func(AgentEvent)) {
	recorder, _ := a.cc.(auditRecorder)
	id := fmt.Sprintf("%x-%x", time.Now().UnixNano(), runSequence.Add(1))
	started := time.Now()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var auditErr error
	requests, toolCalls, attempts := 0, 0, 0
	var usage model.Usage
	persist := func(kind string, data map[string]any) {
		if recorder == nil || auditErr != nil {
			return
		}
		data["run_id"] = id
		if err := recorder.RecordAudit(kind, data); err != nil {
			auditErr = fmt.Errorf("persist %s: %w", kind, err)
			cancel()
			observe(AgentEvent{Type: EventError, Err: auditErr})
		}
	}
	emit := func(e AgentEvent) {
		switch e.Type {
		case EventModelRequest:
			attempts++
			persist("model_request", map[string]any{"attempt": attempts})
		case EventMessageEnd:
			requests++
			usage = usage.Add(e.Ended.Usage)
		case EventToolStart:
			toolCalls++
		case EventError:
			// Provider error text can embed credentials or request contents. Keep
			// only a classification/type; the existing live observer owns display.
			persist("run_error", map[string]any{"category": errorCategory(e.Err), "error_type": fmt.Sprintf("%T", e.Err)})
		case EventRetry:
			persist("model_retry", map[string]any{"attempt": e.Retry.Attempt, "delay_ms": e.Retry.Delay.Milliseconds(), "category": errorCategory(e.Retry.Err)})
		case EventCompaction:
			persist("context_compaction", map[string]any{"reason": e.Compaction.Reason})
		}
		observe(e)
	}
	observe(AgentEvent{Type: EventAgentStart})
	persist("run_start", map[string]any{"agent": a.name})
	if auditErr == nil {
		emit(AgentEvent{Type: EventTurnStart})
		reason := a.loop(runCtx, steer, emit)
		if ctx.Err() != nil {
			reason = "canceled"
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				reason = "timeout"
			}
		}
		if auditErr != nil {
			reason = "audit_error"
		}
		persist("run_end", map[string]any{"stop_reason": reason, "requests": requests, "model_attempts": attempts, "tool_calls": toolCalls, "usage": usage, "duration_ms": time.Since(started).Milliseconds()})
		emit(AgentEvent{Type: EventTurnEnd})
	}
	observe(AgentEvent{Type: EventAgentEnd})
}
