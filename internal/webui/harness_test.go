package webui

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"einoclaw-build/internal/agent"
	agentctx "einoclaw-build/internal/context"
	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
	"einoclaw-build/internal/permission"
	rt "einoclaw-build/internal/runtime"
	"einoclaw-build/internal/session"
	"einoclaw-build/internal/tool"
	"einoclaw-build/internal/workspace"
)

// Scripted model, real Agent loop, real permission executor, real scoped file
// tool, and real JSONL persistence. No provider calls or user workspace writes.
type scriptedModel struct{}

func (scriptedModel) Stream(ctx context.Context, msgs []message.Message, _ []model.ToolSpec) (model.ModelStream, error) {
	if len(msgs) > 0 && msgs[len(msgs)-1].Role == message.RoleTool {
		return &oneEvent{event: model.ModelEvent{Text: "已完成隔离测试。\n\n已通过真实 `write_file` 工具创建文件，工具结果和会话均已记录。\n\n```text\nhello from the web harness\n```"}}, nil
	}
	return &oneEvent{event: model.ModelEvent{ToolCalls: []model.ToolCallDelta{{Index: 0, CallID: fmt.Sprintf("call-%d", time.Now().UnixNano()), Name: "write_file", Args: `{"file_path":"hello.txt","content":"hello from the web harness\n"}`}}}}, nil
}

type oneEvent struct {
	event model.ModelEvent
	sent  bool
}

func (s *oneEvent) Recv() (model.ModelEvent, error) {
	if s.sent {
		return model.ModelEvent{}, io.EOF
	}
	s.sent = true
	return s.event, nil
}
func (*oneEvent) Usage() model.Usage { return model.Usage{PromptTokens: 12, CompletionTokens: 20} }
func (*oneEvent) Close()             {}

func harnessFixture(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	ws, err := workspace.Open(root, filepath.Join(t.TempDir(), "changes"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	sm, err := session.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := sm.New(root)
	if err != nil {
		t.Fatal(err)
	}
	cc := agentctx.New(sess, nil, 128000, 16000, func(context.Context) []message.Message { return nil })
	t.Cleanup(func() { cc.Session().Close() })
	registry := tool.NewRegistry()
	for _, tool := range tool.ScopedBuiltins(rt.NewBash(root), nil, ws) {
		registry.Register(tool)
	}
	ui := New()
	exec := tool.NewExecutor(registry, permission.ModeAlwaysAsk, ui)
	ag := agent.New("test", scriptedModel{}, registry, exec, cc)
	err = ui.Configure(Options{CWD: root, Model: "隔离测试 · 脚本模型", Mode: "always-ask", SessionID: sess.Header().ID, Run: ag.RunObserved,
		Record: func(text string) error { return cc.Record(message.NewUserMessage(text), model.Usage{}) }, History: func() ([]message.Message, error) { return cc.Session().Replay() },
		Sessions: func() ([]SessionInfo, error) {
			list, err := sm.List()
			out := []SessionInfo{}
			for _, s := range list {
				out = append(out, SessionInfo{ID: s.ID, Title: s.Label()})
			}
			return out, err
		},
		Switch: func(id string) (string, error) {
			var next *session.Session
			var err error
			if id == "" {
				next, err = sm.New(root)
			} else {
				next, err = sm.Switch(id)
			}
			if err != nil {
				return "", err
			}
			old := cc.Session()
			cc.SetSession(next)
			registry.ResetConv()
			old.Close()
			return next.Header().ID, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		ui.Close(ctx)
	})
	return ui, root
}
func TestHTTPRunsRealHarnessAndWritesAfterApproval(t *testing.T) {
	ui, root := harnessFixture(t)
	ts := httptest.NewUnstartedServer(nil)
	ts.Config.Handler = ui.Handler(ts.Listener.Addr().String(), nil)
	ts.Start()
	defer ts.Close()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, Timeout: 3 * time.Second}
	res, err := c.Get(ts.URL + "/api/bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	post(t, c, ts.URL+"/api/runs", `{"text":"create a file"}`, 200)
	eventually(t, func() bool { return len(ui.snapshot().Approvals) == 1 })
	if _, err := os.Stat(filepath.Join(root, "hello.txt")); !os.IsNotExist(err) {
		t.Fatal("file existed before approval")
	}
	id := ui.snapshot().Approvals[0].ID
	post(t, c, ts.URL+"/api/approvals/"+id, `{"allow":true}`, 200)
	eventually(t, func() bool { return ui.snapshot().Status == "completed" })
	b, err := os.ReadFile(filepath.Join(root, "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello from the web harness\n" {
		t.Fatal(string(b))
	}
	state := ui.snapshot()
	if len(state.Messages) != 4 {
		t.Fatalf("want user/call/result/final, got %+v", state.Messages)
	}
	if state.Messages[2].Blocks[0].ToolResult.IsError {
		t.Fatal("tool failed")
	}
	// Switching away and back must restore the real persisted transcript.
	sessionID := state.SessionID
	post(t, c, ts.URL+"/api/sessions", `{}`, 200)
	post(t, c, ts.URL+"/api/sessions", fmt.Sprintf(`{"id":%q}`, sessionID), 200)
	if len(ui.snapshot().Messages) != 4 {
		t.Fatal("history was not restored")
	}
}

// Opt-in browser QA fixture. Never enabled in the application or normal tests.
func TestBrowserQA(t *testing.T) {
	if os.Getenv("CODECLAW_WEB_BROWSER_QA") != "1" {
		t.Skip("set CODECLAW_WEB_BROWSER_QA=1 for isolated browser QA")
	}
	ui, root := harnessFixture(t)
	listener, err := net.Listen("tcp", "127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	assets, err := filepath.Abs("../../web/dist")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: ui.Handler(listener.Addr().String(), http.FileServer(http.Dir(assets))), ReadHeaderTimeout: 5 * time.Second}
	defer srv.Close()
	t.Logf("Isolated browser QA http://%s; workspace=%s", listener.Addr(), root)
	if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
}
