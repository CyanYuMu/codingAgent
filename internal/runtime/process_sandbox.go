package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// SandboxConfig is a host capability, loaded only from user configuration.
// "required" never falls back to host execution. "off" is explicit legacy mode.
type SandboxConfig struct {
	Mode      string            `yaml:"mode"`
	ReadRoots []string          `yaml:"read_roots"`
	Env       map[string]string `yaml:"env"`
}

type ProcessSandbox struct {
	root     string
	temp     string
	backend  string
	reads    []string
	readOnly bool
	env      map[string]string
}

func NewProcessSandbox(root string, cfg SandboxConfig) (*ProcessSandbox, error) {
	if cfg.Mode == "" {
		cfg.Mode = "required"
	}
	if cfg.Mode != "required" && cfg.Mode != "off" {
		return nil, fmt.Errorf("sandbox.mode must be required or off")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	s := &ProcessSandbox{root: root, backend: "off", env: map[string]string{}}
	for k, v := range cfg.Env {
		switch k {
		case "PATH", "GOROOT", "GOPATH", "GOMODCACHE", "CGO_ENABLED":
			s.env[k] = v
		default:
			return nil, fmt.Errorf("unsupported sandbox environment key: %s", k)
		}
	}
	if cfg.Mode == "off" {
		return s, nil
	}
	switch runtime.GOOS {
	case "darwin":
		s.backend = "seatbelt"
	case "linux":
		s.backend = "bubblewrap"
	default:
		return nil, fmt.Errorf("native sandbox unsupported on %s", runtime.GOOS)
	}
	bin := "sandbox-exec"
	if s.backend == "bubblewrap" {
		bin = "bwrap"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return nil, fmt.Errorf("sandbox required but %s unavailable: %w", bin, err)
	}
	s.reads = []string{root}
	for _, p := range cfg.ReadRoots {
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("sandbox.read_roots must be absolute: %s", p)
		}
		p, err = filepath.EvalSymlinks(p)
		if err != nil {
			return nil, err
		}
		if p == string(filepath.Separator) {
			return nil, fmt.Errorf("sandbox cannot grant read access to filesystem root")
		}
		s.reads = append(s.reads, p)
	}
	s.temp, err = os.MkdirTemp("", "codeclaw-process-")
	if err != nil {
		return nil, err
	}
	s.temp, err = filepath.EvalSymlinks(s.temp)
	if err != nil {
		return nil, err
	}
	for _, dir := range []string{"home", "tmp", "cache"} {
		if err = os.Mkdir(filepath.Join(s.temp, dir), 0o700); err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}

func (s *ProcessSandbox) Backend() string { return s.backend }

// ReadOnly derives a capability for language servers; it cannot widen access.
func (s *ProcessSandbox) ReadOnly() *ProcessSandbox { c := *s; c.readOnly = true; return &c }
func (s *ProcessSandbox) Close() error {
	if s.temp != "" {
		return os.RemoveAll(s.temp)
	}
	return nil
}

// Command constructs an argv-based process in the same sandbox for bash,
// verification and LSP. No model argument is interpolated into a host command.
func (s *ProcessSandbox) Command(ctx context.Context, cwd, program string, args ...string) (*exec.Cmd, error) {
	if s.backend == "off" {
		cmd := exec.CommandContext(ctx, program, args...)
		cmd.Dir = cwd
		cmd.Env = SanitizeEnv(nonInteractiveEnv())
		configureProcess(cmd)
		return cmd, nil
	}
	cwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(s.root, cwd)
	if err != nil || !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("process cwd outside workspace")
	}
	exe, err := exec.LookPath(program)
	if err != nil {
		return nil, err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return nil, err
	}
	var cmd *exec.Cmd
	if s.backend == "seatbelt" {
		argv := append([]string{"-p", s.profile(exe), exe}, args...)
		cmd = exec.CommandContext(ctx, "/usr/bin/sandbox-exec", argv...)
	} else {
		argv := []string{"--die-with-parent", "--new-session", "--unshare-all", "--cap-drop", "ALL", "--proc", "/proc", "--dev", "/dev"}
		for _, p := range s.systemReads() {
			if _, err := os.Stat(p); err == nil {
				argv = append(argv, "--ro-bind", p, p)
			}
		}
		for _, p := range s.reads {
			if p != s.root {
				argv = append(argv, "--ro-bind", p, p)
			}
		}
		mount := "--bind"
		if s.readOnly {
			mount = "--ro-bind"
		}
		argv = append(argv, mount, s.root, s.root, "--bind", s.temp, s.temp)
		argv = append(argv, "--ro-bind", exe, exe)
		// Existing metadata is mounted read-only. Missing metadata remains protected
		// by file tools; Linux path-level process deny rules need a later Landlock layer.
		for _, p := range []string{".git", ".codeclaw", ".agents", ".codex", ".claude"} {
			full := filepath.Join(s.root, p)
			if _, err := os.Lstat(full); err == nil {
				argv = append(argv, "--ro-bind", full, full)
			}
		}
		argv = append(argv, "--chdir", cwd, "--", exe)
		argv = append(argv, args...)
		cmd = exec.CommandContext(ctx, "bwrap", argv...)
	}
	cmd.Dir = cwd
	cmd.Env = s.environment()
	configureProcess(cmd)
	return cmd, nil
}

func (s *ProcessSandbox) systemReads() []string {
	if runtime.GOOS == "darwin" {
		return []string{"/bin", "/sbin", "/usr", "/System", "/Library", "/opt/homebrew", "/private/var/db/dyld", "/private/preboot"}
	}
	return []string{"/bin", "/sbin", "/usr", "/lib", "/lib64", "/etc/ld.so.cache", "/etc/alternatives"}
}

func (s *ProcessSandbox) profile(exe string) string {
	var b strings.Builder
	b.WriteString("(version 1)\n(deny default)\n(allow process-exec process-fork)\n(allow signal (target same-sandbox))\n(allow sysctl-read)\n(allow file-read-metadata)\n")
	// macOS dyld opens the root directory during startup; this literal does
	// not authorize reading file contents beneath it.
	b.WriteString("(allow file-read-data (literal \"/\"))\n")
	b.WriteString("(allow mach-lookup (global-name \"com.apple.dyld\") (global-name \"com.apple.system.logger\"))\n")
	for _, p := range append(append(s.systemReads(), s.reads...), s.temp) {
		fmt.Fprintf(&b, "(allow file-read* file-map-executable (subpath %s))\n", strconv.Quote(p))
	}
	fmt.Fprintf(&b, "(allow file-read* file-map-executable (literal %s))\n", strconv.Quote(exe))
	writes := []string{s.temp}
	if !s.readOnly {
		writes = append(writes, s.root)
	}
	for _, p := range writes {
		fmt.Fprintf(&b, "(allow file-write* (subpath %s))\n", strconv.Quote(p))
	}
	for _, p := range []string{"/dev/null", "/dev/zero", "/dev/random", "/dev/urandom"} {
		fmt.Fprintf(&b, "(allow file-read* file-write* (literal %s))\n", strconv.Quote(p))
	}
	b.WriteString("(allow file-read* file-write* (subpath \"/dev/fd\"))\n")
	// Regex protects metadata at every depth, including paths created after launch.
	var names []string
	for _, name := range []string{"git", "codeclaw", "agents", "codex", "claude"} {
		var expanded strings.Builder
		for _, r := range name {
			fmt.Fprintf(&expanded, "[%c%c]", r, r-'a'+'A')
		}
		names = append(names, expanded.String())
	}
	fmt.Fprintf(&b, "(deny file-write* (regex #\"/[.](%s)(/|$)\"))\n", strings.Join(names, "|"))
	return b.String()
}

func (s *ProcessSandbox) environment() []string {
	var out []string
	for _, k := range []string{"PATH", "LANG", "LC_ALL", "GOROOT", "GOPATH", "GOMODCACHE", "CGO_ENABLED"} {
		v, ok := s.env[k]
		if !ok {
			v, ok = os.LookupEnv(k)
		}
		if ok {
			out = append(out, k+"="+v)
		}
	}
	return append(out, "HOME="+filepath.Join(s.temp, "home"), "TMPDIR="+filepath.Join(s.temp, "tmp"), "GOCACHE="+filepath.Join(s.temp, "cache"), "GOENV=off", "GOTELEMETRY=off", "GOPROXY=off", "GOSUMDB=off", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "PAGER=cat", "TERM=dumb", "CI=true")
}

// Cancellation kills the process group immediately, without a delayed signal
// timer that could later target a reused process id.
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second
}
