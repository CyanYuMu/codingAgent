// Package verification binds explicit, approved checks to workspace contents.
package verification

import (
	"context"
	"fmt"
	"sync"
	"time"

	"einoclaw-build/internal/runtime"
	"einoclaw-build/internal/workspace"
)

type Config struct {
	Commands []string      `yaml:"commands"`
	Timeout  time.Duration `yaml:"timeout"`
}
type Check struct {
	Command    string `json:"command"`
	Passed     bool   `json:"passed"`
	Output     string `json:"output"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"duration_ms"`
}
type Report struct {
	Passed bool    `json:"passed"`
	Before string  `json:"before_sha256"`
	After  string  `json:"after_sha256"`
	Checks []Check `json:"checks"`
	Error  string  `json:"error,omitempty"`
}
type Service struct {
	mu       sync.Mutex
	w        *workspace.Workspace
	sandbox  *runtime.ProcessSandbox
	cfg      Config
	baseline string
}

func New(w *workspace.Workspace, s *runtime.ProcessSandbox, cfg Config) (*Service, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.Timeout > 10*time.Minute {
		return nil, fmt.Errorf("verification.timeout exceeds 10m")
	}
	v := &Service{w: w, sandbox: s, cfg: cfg}
	if len(cfg.Commands) > 0 {
		var err error
		v.baseline, err = w.Fingerprint()
		if err != nil {
			return nil, err
		}
	}
	return v, nil
}
func (v *Service) Commands() []string { return append([]string(nil), v.cfg.Commands...) }
func (v *Service) Enabled() bool      { return len(v.cfg.Commands) > 0 }

// CheckCompletion does not execute commands. The model must use the verify tool
// through the ordinary permission/approval path before claiming completion.
func (v *Service) CheckCompletion(ctx context.Context) error {
	if !v.Enabled() {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	fp, err := v.w.Fingerprint()
	if err != nil {
		return err
	}
	if fp == v.baseline || v.w.Verified() {
		return nil
	}
	return fmt.Errorf("工作区已变更，当前版本尚未通过验证。调用 verify 执行配置的检查，失败则修复；审批被拒绝时说明阻塞，不能声称已验证通过")
}

func (v *Service) Run(ctx context.Context) (Report, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	r := Report{Checks: []Check{}}
	if !v.Enabled() {
		return r, fmt.Errorf("verification.commands is empty; configure repository checks first")
	}
	v.w.MarkVerified("")
	var err error
	r.Before, err = v.w.Fingerprint()
	if err != nil {
		return r, err
	}
	for _, command := range v.cfg.Commands {
		if err = ctx.Err(); err != nil {
			r.Error = err.Error()
			break
		}
		start := time.Now()
		cctx, cancel := context.WithTimeout(ctx, v.cfg.Timeout)
		sink := runtime.NewSink(4000, 4000)
		cmd, e := v.sandbox.Command(cctx, v.w.Path(), "bash", "--noprofile", "--norc", "-c", command)
		if e == nil {
			cmd.Stdout = sink
			cmd.Stderr = sink
			e = cmd.Run()
		}
		if cctx.Err() != nil {
			e = cctx.Err()
		}
		cancel()
		check := Check{Command: command, Passed: e == nil, Output: sink.Result(), DurationMs: time.Since(start).Milliseconds()}
		sink.Close()
		if e != nil {
			check.Error = e.Error()
		}
		r.Checks = append(r.Checks, check)
		if e != nil {
			r.Error = "verification command failed"
			break
		}
	}
	r.After, err = v.w.Fingerprint()
	if err != nil {
		r.Error = err.Error()
	}
	if r.Before != r.After {
		r.Error = "workspace changed while verifying; results are stale"
	}
	if ctx.Err() != nil {
		r.Error = ctx.Err().Error()
	}
	r.Passed = r.Error == "" && len(r.Checks) == len(v.cfg.Commands)
	if r.Passed {
		v.w.MarkVerified(r.After)
	}
	if err := v.w.RecordVerification(r); err != nil {
		v.w.MarkVerified("")
		r.Passed = false
		return r, fmt.Errorf("persist verification: %w", err)
	}
	return r, nil
}
