package permission

// Tier 工具的危险等级（工具自身声明）。
type Tier string

const (
	TierRead  Tier = "read"  // 只读
	TierWrite Tier = "write" // 改状态，不执行代码
	TierExec  Tier = "exec"  // 执行代码/命令
)

// Mode 审批模式（用户配置）。
type Mode string

const (
	ModeAlwaysAsk Mode = "always-ask" // 只有 read 自动放行
	ModeWrite     Mode = "write"      // read/write 放行，exec 询问
	ModeYolo      Mode = "yolo"       // 全放行（默认）
)

// Decision 审批结果。
type Decision string

const (
	DecisionAllow  Decision = "allow"
	DecisionPrompt Decision = "prompt"
	DecisionDeny   Decision = "deny"
)

// Resolve 纯函数：tier + mode → 决策。
func Resolve(tier Tier, mode Mode) Decision {
	switch mode {
	case ModeYolo:
		return DecisionAllow
	case ModeWrite:
		if tier == TierExec {
			return DecisionPrompt
		}
		return DecisionAllow
	case ModeAlwaysAsk:
		if tier == TierRead {
			return DecisionAllow
		}
		return DecisionPrompt
	}
	return DecisionPrompt // 未知 mode 保守：询问
}
