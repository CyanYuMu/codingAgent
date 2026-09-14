package runtime

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSandboxRejectsInvalidConfiguration(t *testing.T) {
	for _, cfg := range []SandboxConfig{{Mode: "auto"}, {Mode: "required", ReadRoots: []string{"relative"}}, {Mode: "required", ReadRoots: []string{"/"}}, {Env: map[string]string{"BASH_ENV": "/tmp/startup.sh"}}} {
		if s, err := NewProcessSandbox(t.TempDir(), cfg); err == nil {
			s.Close()
			t.Fatalf("accepted %+v", cfg)
		}
	}
}

// Explicit opt-in: native sandbox nesting can be prohibited by a CI sandbox.
// Once opted in, unavailable/denied backends are failures, never silent skips.
func TestNativeSandbox(t *testing.T) {
	if os.Getenv("CODECLAW_NATIVE_TEST") != "1" {
		t.Skip("set CODECLAW_NATIVE_TEST=1 for OS enforcement test")
	}
	w, outer := t.TempDir(), t.TempDir()
	w, _ = filepath.EvalSymlinks(w)
	outer, _ = filepath.EvalSymlinks(outer)
	secret := filepath.Join(outer, "probe")
	if err := os.WriteFile(secret, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewProcessSandbox(w, SandboxConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	run := func(script string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd, err := s.Command(ctx, w, "/bin/sh", "-c", script)
		if err != nil {
			return "", err
		}
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		err = cmd.Run()
		return buf.String(), err
	}
	if out, err := run("printf allowed > local; cat local"); err != nil || out != "allowed" {
		t.Fatalf("in-root command: %v %s", err, out)
	}
	if out, err := run("cat '" + secret + "'"); err == nil || strings.Contains(out, "outside") {
		t.Fatalf("outside read: %v %s", err, out)
	}
	if out, err := run("printf changed > '" + secret + "'"); err == nil {
		t.Fatalf("outside write allowed: %s", out)
	}
	if data, err := os.ReadFile(secret); err != nil || string(data) != "outside" {
		t.Fatalf("external probe changed: %q (%v)", data, err)
	}
	t.Setenv("CODECLAW_TEST_SECRET", "must-not-be-inherited")
	if out, err := run(`test -z "${CODECLAW_TEST_SECRET+x}"`); err != nil {
		t.Fatalf("inherited non-allowlisted environment: %s %v", out, err)
	}
	if runtime.GOOS == "darwin" {
		for _, name := range []string{".codeclaw", ".GiT", ".ClAuDe"} {
			if out, err := run("mkdir " + name); err == nil {
				t.Fatalf("metadata write allowed: %s", out)
			}
		}
	}
	ctxRO, cancelRO := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRO()
	readOnly, err := s.ReadOnly().Command(ctxRO, w, "/bin/sh", "-c", "printf forbidden > local")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := readOnly.CombinedOutput(); err == nil {
		t.Fatalf("read-only process wrote workspace: %s", out)
	}
	if out, err := run("cat local"); err != nil || out != "allowed" {
		t.Fatalf("readonly process altered local file: %s %v", out, err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd, err := s.Command(ctx, w, "/bin/bash", "-c", "echo probe > /dev/tcp/127.0.0.1/"+strings.Split(ln.Addr().String(), ":")[1])
	if err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("network allowed: %s", out)
	}
	if _, err := s.Command(ctx, outer, "/bin/sh", "-c", "true"); err == nil {
		t.Fatal("external cwd accepted")
	}
	// Both shell and child hold the output pipe open. Killing only the shell
	// would wait for WaitDelay (2s), so this also exercises process-group cleanup.
	ctxTimeout, cancelTimeout := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelTimeout()
	timed, err := s.Command(ctxTimeout, w, "/bin/sh", "-c", "sleep 30 & wait")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if out, err := timed.CombinedOutput(); err == nil || ctxTimeout.Err() == nil {
		t.Fatalf("expected timeout: %s %v", out, err)
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("process group was not promptly cancelled: %s", elapsed)
	}
}
