package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"einoclaw-build/internal/agent"
	agentctx "einoclaw-build/internal/context"
	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
	rt "einoclaw-build/internal/runtime"
	"einoclaw-build/internal/session"
	"einoclaw-build/internal/skills"
	"einoclaw-build/internal/subagent"
	"einoclaw-build/internal/webui"
)

func runWeb(ui *webui.Server, addr, assets, cwd string, cfg config, ag *agent.Agent, cmgr *agentctx.Manager, sessions *session.Manager, store *rt.ArtifactStore, mgr *subagent.Manager, skillMgr *skills.Manager) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host != "127.0.0.1" {
		return fmt.Errorf("web 服务仅支持绑定 127.0.0.1")
	}
	abs, err := filepath.Abs(assets)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(abs, "index.html")); err != nil {
		return fmt.Errorf("网页尚未构建，请先运行 npm --prefix web install && npm --prefix web run build: %w", err)
	}
	err = ui.Configure(webui.Options{
		CWD: cwd, Model: cfg.Models[0].ModelID, Mode: cfg.ApprovalMode, SessionID: cmgr.Session().Header().ID,
		History: func() ([]message.Message, error) {
			// UI history is the transcript, not the compacted model context.
			entries, err := cmgr.Session().Entries()
			history := []message.Message{}
			for _, entry := range entries {
				if entry.Type == session.EntryMessage && entry.Message != nil {
					history = append(history, *entry.Message)
				}
			}
			return history, err
		},
		Record: func(text string) error {
			if skillMgr != nil && cfg.Skills.CommandsEnabled() {
				if name, args, ok := skills.ParseInvocation(text); ok {
					expanded, skill, err := skillMgr.BuildPrompt(name, args, skills.InvocationUser)
					if err != nil {
						return err
					}
					if err := cmgr.Session().AppendCustom("skill_invocation", map[string]any{"name": skill.Name, "path": skill.FilePath, "args": args, "source": "user"}); err != nil {
						return err
					}
					text = expanded
				}
			}
			return cmgr.Record(message.NewUserMessage(text), model.Usage{})
		},
		Sessions: func() ([]webui.SessionInfo, error) {
			list, err := sessions.List()
			out := []webui.SessionInfo{}
			for _, s := range list {
				out = append(out, webui.SessionInfo{ID: s.ID, Title: s.Label()})
			}
			return out, err
		},
		Switch: func(id string) (string, error) {
			var next *session.Session
			var err error
			if id == "" {
				next, err = sessions.New(cwd)
			} else {
				next, err = sessions.Switch(id)
			}
			if err != nil {
				return "", err
			}
			dir, err := sessions.ArtifactDir(next)
			if err != nil {
				next.Close()
				return "", err
			}
			old := cmgr.Session()
			cmgr.SetSession(next)
			ag.Registry().ResetConv()
			if history, err := next.Replay(); err == nil {
				ag.Registry().RestoreReadState(history)
			}
			store.SetDir(dir)
			mgr.SetMainSession(next.Header().ID, dir)
			_ = old.Close()
			return next.Header().ID, nil
		},
		Busy: func() bool { return mgr.Pending() > 0 },
		Workers: func() []webui.Worker {
			out := []webui.Worker{}
			for _, r := range mgr.RosterFor(mgr.CurrentSessionID()) {
				out = append(out, webui.Worker{Name: r.Name, Status: r.Status, Tool: r.CurrentTool})
			}
			return out
		},
		CancelWorkers: func() {
			var names []string
			for _, r := range mgr.RosterFor(mgr.CurrentSessionID()) {
				names = append(names, r.Name)
			}
			mgr.Cancel(names)
		},
		Run: func(ctx context.Context, steer <-chan message.Message, observe func(agent.AgentEvent)) {
			ag.RunObserved(ctx, steer, observe)
			// Same persist-before-ACK contract as TUI/headless, with bounded continuation.
			for i := 0; i < maxAutoContinue && ctx.Err() == nil; i++ {
				jobs, mails := waitDeliveries(ctx, mgr, cfg.Subagent.DefaultTimeout)
				if ctx.Err() != nil {
					return
				}
				if len(jobs) == 0 && len(mails) == 0 {
					return
				}
				if err := cmgr.Record(message.NewUserMessage(subagent.RenderAsyncResult(jobs, mails)), model.Usage{}); err != nil {
					observe(agent.AgentEvent{Type: agent.EventError, Err: err})
					return
				}
				var jobIDs, mailIDs []string
				for _, j := range jobs {
					jobIDs = append(jobIDs, j.DeliveryID)
				}
				for _, m := range mails {
					mailIDs = append(mailIDs, m.DeliveryID)
				}
				mgr.AckSettled(jobIDs)
				mgr.AckMainInbox(mailIDs)
				ag.RunObserved(ctx, steer, observe)
			}
		},
	})
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: ui.Handler(listener.Addr().String(), http.FileServer(http.Dir(abs))), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errorsCh := make(chan error, 1)
	go func() { errorsCh <- srv.Serve(listener) }()
	fmt.Printf("CodeClaw 网页端：http://%s\n工作目录：%s\n", listener.Addr(), cwd)
	select {
	case <-ctx.Done():
	case err = <-errorsCh:
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	closeErr := ui.Close(closeCtx)
	_ = srv.Close() // disconnect SSE clients, which otherwise keep Shutdown waiting
	_ = cmgr.Session().Close()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return closeErr
}
