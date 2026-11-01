package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"einoclaw-build/internal/agent"
	"einoclaw-build/internal/subagent"
)

// Poll the authoritative roster, rather than relying on lossy UI events.
// The observer only renders; it never accounts usage or drives a run.
func startHeadlessProgress(ctx context.Context, mgr *subagent.Manager, out io.Writer, interval time.Duration) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if mgr == nil {
			return
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		previous := map[string]string{}
		printed := map[string]time.Time{}
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				for _, r := range mgr.RosterFor(mgr.CurrentSessionID()) {
					state := fmt.Sprintf("%s/%s/%d/%d", r.Status, r.CurrentTool, r.Requests, r.Tokens)
					active := r.Status == "running" || r.Status == "pending" || r.Status == "idle" || r.Status == "budget_stop"
					changed := state != previous[r.ID]
					if changed || (active && now.Sub(printed[r.ID]) >= 10*time.Second) {
						fmt.Fprintln(out, progressLine(r))
						previous[r.ID], printed[r.ID] = state, now
					}
				}
			}
		}
	}()
	return func() { cancel(); <-done }
}

// mainActivity covers runs without child agents, including time spent waiting
// for the first model chunk. It is display-only and never stores model content.
type mainActivity struct {
	mu               sync.Mutex
	started          time.Time
	phase            string
	requests, chunks int
	tools            map[string]bool
}

func (s *mainActivity) observe(ev agent.AgentEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch ev.Type {
	case agent.EventAgentStart:
		s.started = time.Now()
		s.phase = "requesting model"
		s.tools = map[string]bool{}
	case agent.EventMessageStart:
		s.phase = "generating"
	case agent.EventModelRequest:
		s.phase = "requesting model"
	case agent.EventMessageUpdate:
		s.chunks++
	case agent.EventMessageEnd:
		s.requests++
		s.phase = "processing response"
	case agent.EventToolStart:
		s.tools[ev.ToolStart.ID] = true
		s.phase = "executing tools"
	case agent.EventToolEnd:
		delete(s.tools, ev.ToolEnd.ID)
		if len(s.tools) == 0 {
			s.phase = "requesting model"
		}
	case agent.EventCompaction:
		s.phase = "requesting model after compaction"
	case agent.EventRetry:
		s.phase = "retrying model"
	case agent.EventAgentEnd:
		s.phase = ""
	}
}

func (s *mainActivity) line(now time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase == "" {
		return ""
	}
	return fmt.Sprintf("[agent Main · %s · requests=%d chunks=%d tools=%d · %s]", s.phase, s.requests, s.chunks, len(s.tools), now.Sub(s.started).Round(time.Second))
}

func (s *mainActivity) watch(ctx context.Context, out io.Writer, interval time.Duration) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				if line := s.line(now); line != "" {
					fmt.Fprintln(out, line)
				}
			}
		}
	}()
	return func() { cancel(); <-done }
}

func progressLine(r subagent.RunView) string {
	activity := r.CurrentTool
	if activity == "" {
		activity = "model"
		if r.Status == "parked" {
			activity = "settled"
		}
	}
	// Model-supplied names must not be able to inject terminal control codes.
	clean := func(s string) string {
		return strings.Map(func(c rune) rune {
			if c < 32 || c == 127 {
				return -1
			}
			return c
		}, s)
	}
	return fmt.Sprintf("[agent %s · %s · %s · requests=%d tokens=%d · %s]", clean(r.Name), clean(r.Status), clean(activity), r.Requests, r.Tokens, r.Age.Round(time.Second))
}
