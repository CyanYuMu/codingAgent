package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
