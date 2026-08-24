package subagent

import (
	"time"

	"einoclaw-build/internal/model"
)

// AgentDef 一个子 agent 的声明。来源可以是内置（嵌入的 markdown）、用户级或项目级
// frontmatter 文件；同名时项目 > 用户 > 内置（见 discovery.go）。
type AgentDef struct {
	Name         string         // 唯一 id（大小写敏感）
	Description  string         // 一句话，进 task 工具描述
	WhenToUse    string         // 使用边界，进 task 工具描述
	SystemPrompt string         // frontmatter 之后的正文
	Tools        []string       // 限定工具名；空 = worker 默认集
	Spawns       []string       // 可再派发的 agent（"*" = 全部）；空 = 不给 task 工具
	Model        string         // 预留：模型别名/ID（M2 只记录，不改变路由）
	OutputSchema map[string]any // JSON Schema；yield 的 data 参数与校验器由它派生
	SchemaMode   string         // permissive（默认）| strict
	MaxTurns     int            // 单个 turn 的工具循环上限
	SoftBudget   int            // 累计模型请求软预算（0 = 关闭护栏）
	Timeout      time.Duration  // wall-clock 超时（0 = 无）
	ReadOnly     bool           // 只读 agent：工具集裁到 read_file/glob/grep
	Blocking     bool           // 后台批次里仍内联等待
	Source       string         // bundled | user | project
	FilePath     string         // 定义文件路径（bundled 为 embed 内路径）
}

// EffectiveSchemaMode 返回生效的 schema 模式（缺省 permissive）。
func (d AgentDef) EffectiveSchemaMode() string {
	if d.SchemaMode == SchemaModeStrict {
		return SchemaModeStrict
	}
	return SchemaModePermissive
}

// schema 校验模式：permissive 超重试次数后放行并告警，strict 判失败。
const (
	SchemaModePermissive = "permissive"
	SchemaModeStrict     = "strict"
)

// Status 子 agent 执行状态。
type Status int

const (
	StatusPending    Status = iota
	StatusRunning           // 运行中
	StatusIdle              // turn 结束但没 terminal yield，等提醒/唤醒
	StatusBudgetStop        // 越过 1.5× 软预算，当前 turn 已停，正在被逼 yield
	StatusCompleted         // terminal yield 且校验通过（或 permissive 放行）
	StatusFailed            // yield error / strict 校验失败 / 不可恢复错误 / 未知 agent
	StatusKilled            // 预算宽限耗尽后硬杀
	StatusTimeout           // wall-clock 超时
	StatusAborted           // 父取消 / 用户中断 / hub cancel
	StatusParked            // 已结算，sidecar 保留，可被唤醒续跑
)

// StatusString 返回状态的可读名。
func StatusString(s Status) string {
	switch s {
	case StatusPending:
		return "pending"
	case StatusRunning:
		return "running"
	case StatusIdle:
		return "idle"
	case StatusBudgetStop:
		return "budget_stop"
	case StatusCompleted:
		return "completed"
	case StatusFailed:
		return "failed"
	case StatusKilled:
		return "killed"
	case StatusTimeout:
		return "timeout"
	case StatusAborted:
		return "aborted"
	case StatusParked:
		return "parked"
	}
	return "unknown"
}

// Settled 返回该状态是否已结算（不再消耗模型请求）。
func (s Status) Settled() bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusKilled, StatusTimeout, StatusAborted, StatusParked:
		return true
	}
	return false
}

// TaskItem 批次里的一项派发。
type TaskItem struct {
	Name         string         // 稳定名：hub 寻址 / agent:// / sidecar 文件名；缺省 <Agent>-<序号>
	Agent        string         // agent 定义名
	Task         string         // 自包含任务说明：Target / Change / Acceptance
	OutputSchema map[string]any // 覆盖 agent 定义
	SchemaMode   string         // 覆盖 agent 定义
	Effort       string         // lo|med|hi（M2 只记录进 session_init 供审计）
}

// TaskBatch 一次派发：整批共享 Context，每项独立执行。
type TaskBatch struct {
	Context    string // Goal / Constraints / Contract（子 agent 看不到父历史，这是唯一的共享背景）
	Tasks      []TaskItem
	Background bool // true = 立即返回 job id，结算后按 async-result 投递
}

// Result 一个 Run 的执行结果。
type Result struct {
	ID       string // Run id
	Name     string // 运行名（hub 寻址用）
	Agent    string // agent 定义名
	Status   Status
	Yielded  bool             // 是否通过 terminal yield 结束
	Data     any              // yield 的结构化产出（对象/数组/标量）
	Sections map[string][]any // 增量分段（按提交序）
	Text     string           // 最后一段 assistant 文本（失败/未 yield 时的 partial）
	Err      error
	Warning  string // schema 放行 / 未 yield / 预算强制收尾等非致命说明

	Usage         model.Usage
	Requests      int // 模型调用次数
	ToolCalls     int
	Reminders     int  // 注入过的 idle 提醒次数
	BudgetStopped bool // 是否被软预算强制收尾
	DurationMs    int64

	SessionFile string // sidecar 转录路径（history://<Name>）
	OutputFile  string // 完整产出落盘路径（agent://<Name>）
}

// RunView 是名册里的一行快照（给 TUI Agent Hub 与 hub list 用）。
type RunView struct {
	ID, Name, Agent string
	Status          string
	CurrentTool     string
	Depth           int
	Requests        int
	ToolCalls       int
	Tokens          int
	ContextTokens   int
	Unread          int // 未读 hub 消息
	Revives         int
	Age             time.Duration
	SessionFile     string
	OutputFile      string
}
