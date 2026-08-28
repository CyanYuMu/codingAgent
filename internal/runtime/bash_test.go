package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestNonInteractiveEnv(t *testing.T) {
	env := strings.Join(nonInteractiveEnv(), "\n")
	for _, want := range []string{"PAGER=cat", "TERM=dumb", "NO_COLOR=1", "CI=true", "GIT_TERMINAL_PROMPT=0"} {
		if !strings.Contains(env, want+"\n") && !strings.HasSuffix(env, want) {
			t.Errorf("env 缺少 %q", want)
		}
	}
}

func TestParseCd(t *testing.T) {
	cwd, rest, ok := parseCd("cd /tmp && ls -la")
	if !ok || cwd != "/tmp" || rest != "ls -la" {
		t.Fatalf("parseCd = %q, %q, %v", cwd, rest, ok)
	}
	if _, _, ok := parseCd("ls -la"); ok {
		t.Fatal("无 cd 前缀应返回 ok=false")
	}
}

func TestBashDefaultCwdAndRelativeCd(t *testing.T) {
	dir := t.TempDir()
	dir, _ = filepath.EvalSymlinks(dir)
	_ = os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	b := NewBash(dir)
	sink := NewSink(4000, 4000)
	defer sink.Close()
	if err := b.Execute(context.Background(), "cd sub && pwd", sink); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(strings.TrimSpace(sink.Result()), "sub") || b.CWD() != filepath.Join(dir, "sub") {
		t.Fatalf("cwd = %q out = %q", b.CWD(), sink.Result())
	}
	if NewBash("").CWD() == "" {
		t.Fatal("empty cwd should default to os.Getwd")
	}
	// 两个实例互不影响
	other := NewBash(dir)
	if other.CWD() != dir {
		t.Fatalf("other cwd = %q", other.CWD())
	}
}

func TestBashExecuteEcho(t *testing.T) {
	b := NewBash(t.TempDir())
	s := NewSink(1024, 1024)
	defer s.Close()
	if err := b.Execute(context.Background(), "echo hello", s); err != nil {
		t.Fatal(err)
	}
	if got := s.Result(); !strings.Contains(got, "hello") {
		t.Fatalf("Result = %q", got)
	}
}

func TestSanitizeEnvDropsSecrets(t *testing.T) {
	base := []string{
		"PATH=/usr/bin", "HOME=/home/u", "LANG=C",
		"OPENAI_API_KEY=sk-abc", "GITHUB_TOKEN=ghp_x", "DB_PASSWORD=pw",
		"AWS_SECRET_ACCESS_KEY=ak", "NPM_CONFIG_REGISTRY=https://r",
	}
	got := strings.Join(SanitizeEnv(base), "\n")
	for _, keep := range []string{"PATH=/usr/bin", "HOME=/home/u", "LANG=C", "NPM_CONFIG_REGISTRY"} {
		if !strings.Contains(got, keep) {
			t.Errorf("应保留 %q，got %q", keep, got)
		}
	}
	for _, drop := range []string{"OPENAI_API_KEY", "GITHUB_TOKEN", "DB_PASSWORD", "AWS_SECRET_ACCESS_KEY"} {
		if strings.Contains(got, drop) {
			t.Errorf("应剔除 %q，got %q", drop, got)
		}
	}
}

func TestBashTimeoutKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	b := NewBashWithTimeout(dir, 300*time.Millisecond)
	s := NewSink(4000, 4000)
	defer s.Close()
	start := time.Now()
	err := b.Execute(context.Background(), "echo $$ > "+pidFile+"; sleep 100", s)
	if err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("应返回超时错误，got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("超时回收太慢：%v", time.Since(start))
	}
	data, rerr := os.ReadFile(pidFile)
	if rerr != nil {
		t.Fatal(rerr)
	}
	pid := 0
	fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid)
	if pid <= 0 {
		t.Fatalf("坏 pid：%q", data)
	}
	// 进程组应已被回收：kill(pid, 0) 报 ESRCH
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if err == syscall.ESRCH {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sleep 进程 %d 未被回收", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestBashTimeoutLeavesNoResidue(t *testing.T) {
	// 后台孙进程 + 管道：整个组都要被杀
	dir := t.TempDir()
	b := NewBashWithTimeout(dir, 300*time.Millisecond)
	s := NewSink(4000, 4000)
	defer s.Close()
	err := b.Execute(context.Background(), "sleep 100 & echo started; wait", s)
	if err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("应超时，got %v", err)
	}
}
