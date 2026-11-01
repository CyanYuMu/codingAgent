package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"einoclaw-build/internal/agent"
	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
	"einoclaw-build/internal/subagent"
)

type progressModel struct{ release <-chan struct{} }

func (m progressModel) Stream(ctx context.Context, _ []message.Message, _ []model.ToolSpec) (model.ModelStream, error) {
	select {
	case <-m.release:
		return &progressStream{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type progressStream struct{ sent bool }

func (s *progressStream) Recv() (model.ModelEvent, error) {
	if s.sent {
		return model.ModelEvent{}, io.EOF
	}
	s.sent = true
	return model.ModelEvent{ToolCalls: []model.ToolCallDelta{{Index: 0, CallID: "done", Name: "yield", Args: `{"data":"done"}`}}}, nil
}
func (*progressStream) Usage() model.Usage { return model.Usage{TotalTokens: 1} }
func (*progressStream) Close()             {}

type progressOutput chan string

func (out progressOutput) Write(p []byte) (int, error) {
	select {
	case out <- string(p):
	default:
	}
	return len(p), nil
}

func TestHeadlessProgressWorksWithoutEventBus(t *testing.T) {
	release := make(chan struct{})
	mgr := subagent.NewManager(subagent.Options{Model: progressModel{release}, MinTaskChars: 1, SessionDir: t.TempDir(), CWD: t.TempDir(), Defs: []subagent.AgentDef{{Name: "worker"}}})
	defer mgr.Shutdown(time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err := mgr.StartBackground(ctx, subagent.TaskBatch{Context: "Goal: scoped test. Constraints: yield only. Contract: done string.", Tasks: []subagent.TaskItem{{Name: "VisibleWorker", Agent: "worker", Task: "implement the scoped test"}}}, mgr.Env(0, "", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer close(release)
	out := make(progressOutput, 16)
	stop := startHeadlessProgress(ctx, mgr, out, 10*time.Millisecond)
	defer stop()
	select {
	case line := <-out:
		if !strings.Contains(line, "VisibleWorker") || !strings.Contains(line, "requests=") {
			t.Fatalf("progress=%s", line)
		}
	case <-ctx.Done():
		t.Fatal("no progress while model is waiting")
	}
}

func TestHeadlessApprovalDoesNotSuggestYoloBypass(t *testing.T) {
	s := (headlessApprover{}).DenyReason()
	if strings.Contains(s, "use --yolo") || !strings.Contains(s, "forced approval") || !strings.Contains(s, "interactively") {
		t.Fatalf("misleading denial: %s", s)
	}
}

func TestMainHeartbeatBeforeFirstModelChunkAndDuringTools(t *testing.T) {
	s := &mainActivity{}
	s.observe(agent.AgentEvent{Type: agent.EventAgentStart})
	out := make(progressOutput, 16)
	stop := s.watch(context.Background(), out, time.Millisecond)
	select {
	case line := <-out:
		if !strings.Contains(line, "Main") || !strings.Contains(line, "requesting model") {
			t.Fatalf("line=%s", line)
		}
	case <-time.After(time.Second):
		t.Fatal("no heartbeat before first model chunk")
	}
	stop()
	s.observe(agent.AgentEvent{Type: agent.EventToolStart, ToolStart: &agent.ToolStart{ID: "call"}})
	if line := s.line(time.Now()); !strings.Contains(line, "tools=1") {
		t.Fatal(line)
	}
	s.observe(agent.AgentEvent{Type: agent.EventToolEnd, ToolEnd: &agent.ToolEnd{ID: "call"}})
	s.observe(agent.AgentEvent{Type: agent.EventAgentEnd})
	if line := s.line(time.Now()); line != "" {
		t.Fatalf("heartbeat after run ended: %s", line)
	}
}
