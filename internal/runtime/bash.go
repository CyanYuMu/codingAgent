package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// defaultBashTimeout 单条 bash 命令的 wall-clock 上限（可配置到 600s）。
const defaultBashTimeout = 120 * time.Second

// Bash 执行 shell 命令，输出进 Sink，cwd 在实例内持久化（每个 agent / 子 agent 各持一个实例）。
type Bash struct {
	mu      sync.Mutex
	cwd     string
	timeout time.Duration
}

// NewBash 创建 bash 执行器（默认 120s 超时）；cwd 为空时用进程当前目录，相对路径转绝对。
func NewBash(cwd string) *Bash { return NewBashWithTimeout(cwd, defaultBashTimeout) }

// NewBashWithTimeout 创建带自定义超时的 bash 执行器。
func NewBashWithTimeout(cwd string, timeout time.Duration) *Bash {
	if timeout <= 0 {
		timeout = defaultBashTimeout
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}
	return &Bash{cwd: cwd, timeout: timeout}
}

// CWD 返回当前工作目录。
func (b *Bash) CWD() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cwd
}

// Execute 执行 command，stdout/stderr 都进 sink。
// 超时（b.timeout）→ SIGTERM 整个进程组，5s 后 SIGKILL 补刀；返回明确的超时错误。
func (b *Bash) Execute(ctx context.Context, command string, sink *Sink) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if newCwd, rest, ok := parseCd(command); ok {
		if !filepath.IsAbs(newCwd) {
			newCwd = filepath.Join(b.cwd, newCwd)
		}
		b.cwd = filepath.Clean(newCwd)
		command = rest
	}

	ctx2, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx2, "bash", "-c", command)
	cmd.Dir = b.cwd
	cmd.Env = SanitizeEnv(nonInteractiveEnv())
	cmd.Stdout = sink
	cmd.Stderr = sink
	// 独立进程组：超时/取消时按组回收，管道与后台孙进程一起被杀
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		time.AfterFunc(5*time.Second, func() {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		})
		return nil
	}
	cmd.WaitDelay = 6 * time.Second

	err := cmd.Run()
	if errors.Is(ctx2.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		return fmt.Errorf("命令超时（%s），已终止进程组", b.timeout)
	}
	return err
}

// parseCd 解析前缀 "cd <path> && "，返回新 cwd + 剩余命令。
func parseCd(command string) (cwd, rest string, ok bool) {
	if !strings.HasPrefix(command, "cd ") {
		return "", command, false
	}
	dir, after, found := strings.Cut(command[len("cd "):], " && ")
	if !found {
		return "", command, false
	}
	return strings.TrimSpace(dir), strings.TrimSpace(after), true
}
