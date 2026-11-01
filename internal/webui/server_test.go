package webui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"einoclaw-build/internal/agent"
	"einoclaw-build/internal/message"
)

func fixture(t *testing.T, run func(context.Context, <-chan message.Message, func(agent.AgentEvent)), record func(string) error) (*Server, *httptest.Server, *http.Client) {
	t.Helper()
	s := New()
	var mu sync.Mutex
	history := []message.Message{}
	if record == nil {
		record = func(text string) error {
			mu.Lock()
			defer mu.Unlock()
			history = append(history, message.NewUserMessage(text))
			return nil
		}
	}
	err := s.Configure(Options{SessionID: "one", Run: run, Record: record, History: func() ([]message.Message, error) {
		mu.Lock()
		defer mu.Unlock()
		return append([]message.Message{}, history...), nil
	}, Sessions: func() ([]SessionInfo, error) { return []SessionInfo{{ID: "one", Title: "Test"}}, nil }, Switch: func(id string) (string, error) { return "two", nil }})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(nil)
	ts.Config.Handler = s.Handler(ts.Listener.Addr().String(), nil)
	ts.Start()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 3 * time.Second}
	res, err := client.Get(ts.URL + "/api/bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Close(ctx)
		ts.Close()
	})
	return s, ts, client
}
func post(t *testing.T, c *http.Client, url, body string, status int) {
	t.Helper()
	r, err := c.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != status {
		t.Fatalf("status %d want %d: %s", r.StatusCode, status, b)
	}
}
func eventually(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out")
}
func TestRunLifecycleAndReconnect(t *testing.T) {
	entered := make(chan struct{})
	steered := make(chan string, 1)
	s, ts, c := fixture(t, func(ctx context.Context, ch <-chan message.Message, emit func(agent.AgentEvent)) {
		close(entered)
		emit(agent.AgentEvent{Type: agent.EventMessageUpdate, Update: &agent.MessageUpdate{Text: "live output"}})
		select {
		case m := <-ch:
			steered <- m.Blocks[0].Text
		case <-ctx.Done():
			return
		}
		<-ctx.Done()
	}, nil)
	post(t, c, ts.URL+"/api/runs", `{"text":"hello"}`, 200)
	<-entered
	post(t, c, ts.URL+"/api/runs", `{"text":"duplicate"}`, 409)
	post(t, c, ts.URL+"/api/sessions", `{"id":"two"}`, 409)
	// A fresh SSE connection receives the full in-flight snapshot, not only future deltas.
	r, err := c.Get(ts.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	scan := bufio.NewScanner(r.Body)
	var snap State
	for scan.Scan() {
		if strings.HasPrefix(scan.Text(), "data: ") {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(scan.Text(), "data: ")), &snap); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	r.Body.Close()
	if snap.Streaming != "live output" || snap.Status != "running" {
		t.Fatalf("bad recovered state: %+v", snap)
	}
	if s.snapshot().Status != "running" {
		t.Fatal("disconnect canceled run")
	}
	post(t, c, ts.URL+"/api/steer", `{"text":"adjust"}`, 200)
	select {
	case got := <-steered:
		if got != "adjust" {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("steering lost")
	}
	post(t, c, ts.URL+"/api/cancel", `{}`, 200)
	eventually(t, func() bool { return s.snapshot().Status == "cancelled" })
	post(t, c, ts.URL+"/api/sessions", `{}`, 200)
	if s.snapshot().SessionID != "two" {
		t.Fatal("did not switch")
	}
}
func TestApprovalRoundTripAndCancellation(t *testing.T) {
	s, ts, c := fixture(t, func(context.Context, <-chan message.Message, func(agent.AgentEvent)) {}, nil)
	for _, allow := range []bool{false, true} {
		done := make(chan bool, 1)
		go func() {
			v, err := s.Approve(context.Background(), message.ToolCall{ID: "call", Name: "bash", Args: `{"command":"pwd"}`})
			done <- v && err == nil
		}()
		eventually(t, func() bool { return len(s.snapshot().Approvals) == 1 })
		id := s.snapshot().Approvals[0].ID
		post(t, c, ts.URL+"/api/approvals/"+id, fmt.Sprintf(`{"allow":%v}`, allow), 200)
		if got := <-done; got != allow {
			t.Fatalf("decision %v want %v", got, allow)
		}
		post(t, c, ts.URL+"/api/approvals/"+id, `{"allow":true}`, 409)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := s.Approve(ctx, message.ToolCall{Name: "bash"}); done <- err }()
	eventually(t, func() bool { return len(s.snapshot().Approvals) == 1 })
	id := s.snapshot().Approvals[0].ID
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatal(err)
	}
	post(t, c, ts.URL+"/api/approvals/"+id, `{"allow":true}`, 409)
}
func TestLocalAPIAuthenticationAndValidation(t *testing.T) {
	_, ts, c := fixture(t, func(context.Context, <-chan message.Message, func(agent.AgentEvent)) {}, nil)
	r, err := http.Get(ts.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 401 {
		t.Fatal(r.StatusCode)
	}
	for _, tc := range []struct{ host, origin, site string }{{"evil.example", "", ""}, {"", "https://evil.example", ""}, {"", "", "cross-site"}} {
		req, _ := http.NewRequest("GET", ts.URL+"/api/bootstrap", nil)
		if tc.host != "" {
			req.Host = tc.host
		}
		req.Header.Set("Origin", tc.origin)
		req.Header.Set("Sec-Fetch-Site", tc.site)
		r, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != 403 {
			t.Fatalf("bad origin accepted: %+v", tc)
		}
	}
	post(t, c, ts.URL+"/api/runs", `{"text":" "}`, 400)
	post(t, c, ts.URL+"/api/runs", `{"text":"hi","unknown":true}`, 400)
	post(t, c, ts.URL+"/api/runs", `{"text":"hi"} {}`, 400)
	u, _ := url.Parse(ts.URL)
	for _, cookie := range c.Jar.Cookies(u) {
		if cookie.Name == "codeclaw_session" {
			t.Fatal("API cookie should not be scoped to root")
		}
	}
}
func TestRecordFailureDoesNotRun(t *testing.T) {
	called := false
	s, ts, c := fixture(t, func(context.Context, <-chan message.Message, func(agent.AgentEvent)) { called = true }, func(string) error { return fmt.Errorf("disk full") })
	post(t, c, ts.URL+"/api/runs", `{"text":"hello"}`, 409)
	if called || s.snapshot().Status != "idle" {
		t.Fatal("ran without durable input")
	}
}
