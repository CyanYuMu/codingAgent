package agent

import (
	"context"
	"sync"
	"time"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
	"einoclaw-build/internal/tool"
)

// Context 是循环的真相源抽象：每步从它重建输入、向它记录消息、由它决定压缩与溢出恢复。
// 生产实现是 context.Manager（包着 session）；测试用 MemoryContext。
type Context interface {
	Build(ctx context.Context) ([]message.Message, error)
	Record(m message.Message, u model.Usage) error
	ShouldCompact(u model.Usage) bool
	Compact(ctx context.Context) (bool, error)
	RecoverOverflow(ctx context.Context) (bool, error)
}

// Agent 是一个可运行的编程智能体。
type Agent struct {
	name          string
	model         model.Model
	tools         *tool.Registry // 工具注册表（给模型的工具定义）
	executor      *tool.Executor // 工具执行器（审批 + 执行）
	cc            Context
	maxIterations int           // 工具循环上限，防失控
	maxRetries    int           // 瞬时错误重试上限
	retryBase     time.Duration // 退避基数：base·2^(n-1)
}

// New 创建一个 Agent。调用方负责在 Run 前把用户消息 Record 进 cc。
func New(name string, m model.Model, tools *tool.Registry, exec *tool.Executor, cc Context) *Agent {
	return &Agent{
		name: name, model: m, tools: tools, executor: exec, cc: cc,
		maxIterations: 50, maxRetries: 3, retryBase: 500 * time.Millisecond,
	}
}

// Name 返回 agent 名。
func (a *Agent) Name() string { return a.name }

// SetMaxIterations 覆盖工具循环上限（子 agent 用，0 表示不覆盖）。
func (a *Agent) SetMaxIterations(n int) {
	if n > 0 {
		a.maxIterations = n
	}
}

// MemoryContext 纯内存 Context（测试 / 无会话场景）。Compact 只做演示性截断，不保证 tool 配对，勿用于生产。
type MemoryContext struct {
	mu        sync.Mutex
	system    []message.Message
	msgs      []message.Message
	compactAt int  // >0 时 PromptTokens 超过即 ShouldCompact
	recoverOK bool // RecoverOverflow 是否成功
	compacts  int
	recovers  int
}

// NewMemoryContext 构造内存 Context；system 为固定前缀。
func NewMemoryContext(system []message.Message) *MemoryContext { return &MemoryContext{system: system} }

// Messages 返回已记录的消息副本（不含 system 前缀）。
func (c *MemoryContext) Messages() []message.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]message.Message{}, c.msgs...)
}

func (c *MemoryContext) Build(context.Context) ([]message.Message, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := append([]message.Message{}, c.system...)
	return append(out, c.msgs...), nil
}

func (c *MemoryContext) Record(m message.Message, _ model.Usage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, m)
	return nil
}

func (c *MemoryContext) ShouldCompact(u model.Usage) bool {
	return c.compactAt > 0 && u.PromptTokens > c.compactAt
}

func (c *MemoryContext) Compact(context.Context) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.compacts++
	if len(c.msgs) > 2 {
		c.msgs = append([]message.Message{message.NewUserMessage("[summary]")}, c.msgs[len(c.msgs)-1:]...)
	}
	return true, nil
}

func (c *MemoryContext) RecoverOverflow(context.Context) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recovers++
	if c.recoverOK && len(c.msgs) > 0 {
		c.msgs = c.msgs[len(c.msgs)-1:]
	}
	return c.recoverOK, nil
}
