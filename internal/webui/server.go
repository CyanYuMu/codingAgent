// Package webui adapts the existing harness to a local HTTP interface. Network
// clients observe snapshots; a browser disconnect never owns a run's lifetime.
package webui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"einoclaw-build/internal/agent"
	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
)

type SessionInfo struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}
type Worker struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Tool   string `json:"tool"`
}
type Options struct {
	CWD, Model, Mode string
	Run              func(context.Context, <-chan message.Message, func(agent.AgentEvent))
	Record           func(string) error
	History          func() ([]message.Message, error)
	Sessions         func() ([]SessionInfo, error)
	Switch           func(string) (string, error) // empty id creates a session; called only while idle
	Workers          func() []Worker
	Busy             func() bool
	CancelWorkers    func()
	SessionID        string
}
type Approval struct {
	ID     string           `json:"id"`
	Call   message.ToolCall `json:"call"`
	answer chan bool
	ctx    context.Context
}
type State struct {
	SessionID string            `json:"session_id"`
	RunID     string            `json:"run_id"`
	Status    string            `json:"status"`
	Messages  []message.Message `json:"messages"`
	Streaming string            `json:"streaming"`
	Thinking  string            `json:"thinking"`
	Activity  string            `json:"activity"`
	Error     string            `json:"error"`
	Usage     model.Usage       `json:"usage"`
	Approvals []Approval        `json:"approvals"`
	Workers   []Worker          `json:"workers"`
}
type Server struct {
	mu        sync.Mutex
	opts      Options
	state     State
	approvals map[string]*Approval
	cancel    context.CancelFunc
	steer     chan message.Message
	done      chan struct{}
	token     string
	closing   bool
}

func randomID() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func New() *Server {
	return &Server{token: randomID(), approvals: map[string]*Approval{}, state: State{Status: "idle", Messages: []message.Message{}}}
}
func (s *Server) Configure(o Options) error {
	history, err := o.History()
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opts = o
	s.state.SessionID = o.SessionID
	s.state.Messages = history
	return nil
}

// Approve implements tool.Approver for both root and labeled subagent calls.
func (s *Server) Approve(ctx context.Context, call message.ToolCall) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	a := &Approval{ID: randomID(), Call: call, answer: make(chan bool, 1), ctx: ctx}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return false, context.Canceled
	}
	s.approvals[a.ID] = a
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.approvals, a.ID); s.mu.Unlock() }()
	select {
	case allow := <-a.answer:
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return allow, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}
func (s *Server) snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state
	st.Messages = append([]message.Message{}, st.Messages...)
	st.Approvals = []Approval{}
	st.Workers = []Worker{}
	for _, a := range s.approvals {
		if a.ctx.Err() == nil {
			st.Approvals = append(st.Approvals, *a)
		}
	}
	if s.opts.Workers != nil {
		st.Workers = s.opts.Workers()
	}
	return st
}
func (s *Server) observe(e agent.AgentEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch e.Type {
	case agent.EventMessageStart:
		s.state.Streaming = ""
		s.state.Thinking = ""
		s.state.Activity = "正在思考"
	case agent.EventMessageUpdate:
		if e.Update != nil {
			s.state.Streaming += e.Update.Text
			s.state.Thinking += e.Update.Thinking
		}
	case agent.EventMessageEnd:
		if e.Ended != nil {
			s.state.Messages = append(s.state.Messages, e.Ended.Message)
			s.state.Usage = s.state.Usage.Add(e.Ended.Usage)
		}
		s.state.Streaming = ""
		s.state.Thinking = ""
	case agent.EventToolStart:
		if e.ToolStart != nil {
			s.state.Activity = "正在执行 " + e.ToolStart.Name
		}
	case agent.EventToolEnd:
		if e.ToolEnd != nil {
			t := e.ToolEnd
			s.state.Messages = append(s.state.Messages, message.NewToolMessage(t.ID, t.Name, t.Content, t.IsError))
		}
	case agent.EventError:
		if e.Err != nil {
			s.state.Error = e.Err.Error()
		}
	case agent.EventRetry:
		if e.Retry != nil {
			s.state.Activity = fmt.Sprintf("模型请求重试 · 第 %d 次", e.Retry.Attempt)
		}
	case agent.EventCompaction:
		s.state.Activity = "正在整理上下文"
	}
}
func (s *Server) start(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return fmt.Errorf("服务正在关闭")
	}
	if s.cancel != nil || (s.opts.Busy != nil && s.opts.Busy()) {
		return fmt.Errorf("当前任务尚未结束")
	}
	if err := s.opts.Record(text); err != nil {
		return err
	}
	history, err := s.opts.History()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.steer = make(chan message.Message, 8)
	s.done = make(chan struct{})
	s.state.Messages = history
	s.state.RunID = randomID()
	s.state.Status = "running"
	s.state.Error = ""
	s.state.Streaming = ""
	s.state.Thinking = ""
	s.state.Activity = "正在思考"
	s.state.Usage = model.Usage{}
	go func() {
		defer close(s.done)
		s.opts.Run(ctx, s.steer, s.observe)
		s.mu.Lock()
		defer s.mu.Unlock()
		// Reload durable messages, including steering and background delivery records.
		if history, err := s.opts.History(); err == nil {
			s.state.Messages = history
		} else {
			s.state.Error = err.Error()
		}
		s.state.Status = "completed"
		if s.state.Error != "" {
			s.state.Status = "failed"
		}
		if ctx.Err() != nil {
			s.state.Status = "cancelled"
		}
		s.state.Streaming = ""
		s.state.Thinking = ""
		s.state.Activity = ""
		s.cancel = nil
		cancel()
	}()
	return nil
}
func (s *Server) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
		s.state.Status = "stopping"
	}
	if s.opts.CancelWorkers != nil {
		s.opts.CancelWorkers()
	}
}
func (s *Server) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	s.stop()
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func respond(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return fmt.Errorf("请求需要 application/json")
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return fmt.Errorf("请求包含多余内容")
	}
	return nil
}

// Handler accepts only the bound loopback host and the local Vite development
// origin. Bootstrap issues an HttpOnly cookie; no credentials enter JS storage.
func (s *Server) Handler(host string, assets http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/bootstrap", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "codeclaw_session", Value: s.token, Path: "/api", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		respond(w, map[string]string{"cwd": s.opts.CWD, "model": s.opts.Model, "mode": s.opts.Mode})
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) { respond(w, s.snapshot()) })
	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			fail(w, 500, fmt.Errorf("streaming unavailable"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		last := ""
		seq := 0
		for {
			b, _ := json.Marshal(s.snapshot())
			if string(b) != last {
				seq++
				_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
				if _, err := fmt.Fprintf(w, "id: %d\nevent: state\ndata: %s\n\n", seq, b); err != nil {
					return
				}
				f.Flush()
				last = string(b)
			}
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
			}
		}
	})
	mux.HandleFunc("GET /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		v, err := s.opts.Sessions()
		if err != nil {
			fail(w, 500, err)
			return
		}
		if v == nil {
			v = []SessionInfo{}
		}
		respond(w, v)
	})
	mux.HandleFunc("POST /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID string `json:"id"`
		}
		if err := decode(w, r, &body); err != nil {
			fail(w, 400, err)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.cancel != nil || len(s.approvals) > 0 || (s.opts.Busy != nil && s.opts.Busy()) {
			fail(w, 409, fmt.Errorf("请先结束当前任务再切换会话"))
			return
		}
		id, err := s.opts.Switch(body.ID)
		if err != nil {
			fail(w, 400, err)
			return
		}
		history, err := s.opts.History()
		if err != nil {
			fail(w, 500, err)
			return
		}
		s.state = State{SessionID: id, Status: "idle", Messages: history}
		respond(w, map[string]string{"id": id})
	})
	mux.HandleFunc("POST /api/runs", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Text string `json:"text"`
		}
		if err := decode(w, r, &b); err != nil {
			fail(w, 400, err)
			return
		}
		if strings.TrimSpace(b.Text) == "" {
			fail(w, 400, fmt.Errorf("请输入任务"))
			return
		}
		if err := s.start(b.Text); err != nil {
			fail(w, 409, err)
			return
		}
		respond(w, s.snapshot())
	})
	mux.HandleFunc("POST /api/cancel", func(w http.ResponseWriter, r *http.Request) { s.stop(); respond(w, map[string]bool{"ok": true}) })
	mux.HandleFunc("POST /api/steer", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Text string `json:"text"`
		}
		if err := decode(w, r, &b); err != nil {
			fail(w, 400, err)
			return
		}
		if strings.TrimSpace(b.Text) == "" {
			fail(w, 400, fmt.Errorf("请输入补充指令"))
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.cancel == nil {
			fail(w, 409, fmt.Errorf("没有正在运行的任务"))
			return
		}
		select {
		case s.steer <- message.NewUserMessage(b.Text):
			respond(w, map[string]bool{"ok": true})
		default:
			fail(w, 409, fmt.Errorf("指令队列已满，请稍后再试"))
		}
	})
	mux.HandleFunc("POST /api/approvals/{id}", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Allow *bool `json:"allow"`
		}
		if err := decode(w, r, &b); err != nil {
			fail(w, 400, err)
			return
		}
		if b.Allow == nil {
			fail(w, 400, fmt.Errorf("缺少 allow"))
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		a := s.approvals[r.PathValue("id")]
		if a == nil || a.ctx.Err() != nil {
			fail(w, 409, fmt.Errorf("审批已结束或过期"))
			return
		}
		select {
		case a.answer <- *b.Allow:
			delete(s.approvals, a.ID)
			respond(w, map[string]bool{"ok": true})
		default:
			fail(w, 409, fmt.Errorf("审批已处理"))
		}
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { fail(w, 404, fmt.Errorf("接口不存在")) })
	if assets != nil {
		mux.Handle("/", assets)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		if r.Host != host && r.Host != "127.0.0.1:5173" {
			fail(w, 403, fmt.Errorf("不允许的 Host"))
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Scheme != "http" || (u.Host != host && u.Host != "127.0.0.1:5173") {
				fail(w, 403, fmt.Errorf("不允许的来源"))
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				fail(w, 403, fmt.Errorf("禁止跨站请求"))
				return
			}
			if r.URL.Path != "/api/bootstrap" {
				c, err := r.Cookie("codeclaw_session")
				if err != nil || c.Value != s.token {
					fail(w, 401, fmt.Errorf("请刷新页面重新连接本地服务"))
					return
				}
			}
		}
		mux.ServeHTTP(w, r)
	})
}
