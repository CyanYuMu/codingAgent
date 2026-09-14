package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
	"einoclaw-build/internal/verification"
)

type verifyTool struct{ v *verification.Service }

func NewVerifyTool(v *verification.Service) Tool { return verifyTool{v} }
func (verifyTool) Name() string                  { return "verify" }
func (t verifyTool) Description() string {
	return "执行仓库配置的验证命令，并将结果绑定到当前工作区指纹。修改后必须重新验证。命令：" + strings.Join(t.v.Commands(), "; ")
}
func (verifyTool) Parameters() map[string]any { return map[string]any{} }
func (verifyTool) Tier() permission.Tier      { return permission.TierExec }
func (verifyTool) Concurrency() Concurrency   { return ConcurrencyExclusive }
func (t verifyTool) ApprovalPreview() any     { return map[string]any{"commands": t.v.Commands()} }
func (t verifyTool) Execute(ctx context.Context, _ map[string]any, sink *runtime.Sink) error {
	r, err := t.v.Run(ctx)
	if e := json.NewEncoder(sink).Encode(r); e != nil {
		return e
	}
	if err != nil {
		return err
	}
	if !r.Passed {
		return fmt.Errorf("verification failed: %s", r.Error)
	}
	return nil
}
