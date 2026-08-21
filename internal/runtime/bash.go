package runtime

import (
	"context"
	"os/exec"
	"strings"
	"sync"
)

// Bash 执行 shell 命令，输出进 Sink，cwd 在会话内持久化。
type Bash struct {
	mu  sync.Mutex
	cwd string
}

// NewBash 创建 bash 执行器。
func NewBash(cwd string) *Bash {
	return &Bash{cwd: cwd}
}

// Execute 执行 command，stdout/stderr 都进 sink。
// ctx 取消 → 杀子进程（三档中断的「硬杀」）。
func (b *Bash) Execute(ctx context.Context, command string, sink *Sink) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if newCwd, rest, ok := parseCd(command); ok {
		b.cwd = newCwd
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
	rest = command[len("cd "):]
	idx := strings.Index(rest, " && ")
	if idx < 0 {
		return "", command, false
	}
	return strings.TrimSpace(rest[:idx]), strings.TrimSpace(rest[idx+len(" && "):]), true
}
