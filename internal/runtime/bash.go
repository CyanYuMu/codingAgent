package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Bash 执行 shell 命令，输出进 Sink，cwd 在实例内持久化（每个 agent / 子 agent 各持一个实例）。
type Bash struct {
	mu  sync.Mutex
	cwd string
}

// NewBash 创建 bash 执行器；cwd 为空时用进程当前目录，相对路径转绝对。
func NewBash(cwd string) *Bash {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}
	return &Bash{cwd: cwd}
}

// CWD 返回当前工作目录。
func (b *Bash) CWD() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cwd
}

// Execute 执行 command，stdout/stderr 都进 sink。
// ctx 取消 → 杀子进程（三档中断的「硬杀」）。
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

	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Dir = b.cwd
	cmd.Env = nonInteractiveEnv()
	cmd.Stdout = sink
	cmd.Stderr = sink
	return cmd.Run()
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
