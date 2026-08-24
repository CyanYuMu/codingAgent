package subagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"einoclaw-build/internal/agent"
	"einoclaw-build/internal/bus"
	agentctx "einoclaw-build/internal/context"
	"einoclaw-build/internal/memory"
	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
	"einoclaw-build/internal/session"
	"einoclaw-build/internal/tool"
)

// Options 装配 Manager 所需的一切：子 agent 借用父的模型与记忆，但每个 Run 有独立的 bash/cwd、产物存储、
// sidecar 会话与审批器，权限继承父 mode。
type Options struct {
	Model          model.Model
	WorkerTools    func(cwd string, store *runtime.ArtifactStore) *tool.Registry // 每个 Run 调用一次，返回独立工具集
	Memory         memory.Recaller
	Mode           permission.Mode // 继承父的审批模式（不是 yolo）
	Approver       tool.Approver   // 父的审批器；Escalate 时使用
	Escalate       bool            // true = headless 子 agent 的 Prompt 决策升级到父审批（弹窗带子 agent 标签）
	SessionDir     string          // 父会话产物目录；"" = 不落盘（MemoryStorage）
	CWD            string
	MaxConcurrency int
	Defs           []AgentDef
	Summarizer     agentctx.Summarizer
	ContextWindow  int
	Bus            *bus.Bus // 事件总线（可 nil：不发布）

	// 定义里没写时的兜底
	DefaultTimeout  time.Duration
	DefaultMaxTurns int
	SoftBudget      int // 全局软预算上限；0 = 关闭护栏。定义里的值只能更小

	MaxDepth        int  // 递归深度上限（0 = 默认 2）
	MinTaskChars    int  // 任务描述最短长度（0 = 默认 40）
	AllowBackground bool // false = 忽略 task 的 background 参数（全部同步执行）
}

// Manager 派发子 agent，并持有 Run 名册。
type Manager struct {
	o   Options
	sem chan struct{}
	seq atomic.Int64
}

// NewManager 构造 Manager；并发上限默认 4，窗口默认 128k。
func NewManager(o Options) *Manager {
	if o.MaxConcurrency <= 0 {
		o.MaxConcurrency = 4
	}
	if o.ContextWindow <= 0 {
		o.ContextWindow = 128000
	}
	if o.DefaultMaxTurns <= 0 {
		o.DefaultMaxTurns = 50
	}
	return &Manager{o: o, sem: make(chan struct{}, o.MaxConcurrency)}
}

// List 返回子 agent 定义（确定性顺序，供 task 工具枚举）。
func (m *Manager) List() []AgentDef { return m.o.Defs }

// Env 返回一次派发的调用者环境：主 agent 用 Env(0, "", nil)，子 agent 用 Env(depth, 自己的 agent 名, 自己的 spawns)。
func (m *Manager) Env(depth int, self string, spawns []string) Env {
	return Env{
		Defs: m.o.Defs, Depth: depth, MaxDepth: m.o.MaxDepth, Spawns: spawns, SelfAgent: self,
		MinTaskChars: m.o.MinTaskChars,
		SeqNext:      func(agent string) string { return fmt.Sprintf("%s-%d", agent, m.seq.Add(1)) },
	}
}

// resolveDef 把配置默认值填进定义：定义里写了就用定义的，没写用配置的。
// 软预算特殊：定义里的值只能比全局上限更小（避免一个自定义 agent 把护栏放大）；-1 = 显式关闭。
func (m *Manager) resolveDef(d AgentDef) AgentDef {
	if d.MaxTurns <= 0 {
		d.MaxTurns = m.o.DefaultMaxTurns
	}
	if d.Timeout <= 0 {
		d.Timeout = m.o.DefaultTimeout
	}
	switch {
	case d.SoftBudget < 0:
		d.SoftBudget = 0 // 定义显式关闭
	case d.SoftBudget == 0:
		d.SoftBudget = m.o.SoftBudget
	case m.o.SoftBudget > 0 && d.SoftBudget > m.o.SoftBudget:
		d.SoftBudget = m.o.SoftBudget
	}
	return d
}

// RunBatch 预检后并行执行一批任务，结果按输入序返回。预检失败整批拒绝（不起任何子进程）。
func (m *Manager) RunBatch(ctx context.Context, b TaskBatch, env Env) ([]Result, error) {
	items, err := Preflight(b, env)
	if err != nil {
		return nil, err
	}
	results := make([]Result, len(items))
	var wg sync.WaitGroup
	for i, it := range items {
		if err := m.acquire(ctx); err != nil {
			results[i] = Result{Name: it.Item.Name, Agent: it.Item.Agent, Status: StatusAborted, Err: err}
			continue
		}
		wg.Add(1)
		go func(i int, it Resolved) {
			defer wg.Done()
			defer m.release()
			defer func() {
				if r := recover(); r != nil {
					results[i] = Result{Name: it.Item.Name, Agent: it.Item.Agent, Status: StatusFailed,
						Err: fmt.Errorf("子 agent panic: %v", r)}
				}
			}()
			results[i] = m.Run(ctx, b.Context, it, env.Depth+1)
		}(i, it)
	}
	wg.Wait()
	return results, nil
}

// Run 执行一项已预检的任务并等待其结束。
// Context Isolation：子 agent 只看到 system + batch context + task，不看父历史；父只拿结构化产出/最后文本 + 指针。
func (m *Manager) Run(ctx context.Context, batchContext string, r Resolved, depth int) Result {
	def := m.resolveDef(r.Def)
	name := r.Item.Name
	start := time.Now()

	// 运行时：独立产物存储 / 工具集（独立 bash） / 审批
	var store *runtime.ArtifactStore
	if m.o.SessionDir != "" {
		store = runtime.NewArtifactStore(m.o.SessionDir)
	}
	tools, _ := m.buildTools(def, depth, store)
	var approver tool.Approver = denyApprover{}
	if m.o.Escalate && m.o.Approver != nil {
		approver = labeledApprover{inner: m.o.Approver, label: "[子 agent " + name + "]"}
	}
	exec := tool.NewExecutor(tools, m.o.Mode, approver)
	if store != nil {
		exec.SetArtifactStore(store)
	}

	// sidecar 会话
	sess, file, err := m.openSidecar(name, def, r, tools, depth)
	if err != nil {
		return Result{Name: name, Agent: def.Name, Status: StatusFailed, Err: err}
	}
	defer sess.Close()

	system := func(context.Context) []message.Message {
		return []message.Message{message.NewSystemMessage(def.SystemPrompt + subagentCompletionNote)}
	}
	cc := agentctx.New(sess, m.o.Summarizer, m.o.ContextWindow, 16384, system)
	_ = cc.Record(message.NewUserMessage(buildTaskPrompt(batchContext, r.Item)), model.Usage{})

	sub := agent.New(def.Name, m.o.Model, tools, exec, cc)
	sub.SetMaxIterations(def.MaxTurns)

	runCtx := ctx
	cancel := context.CancelFunc(func() {})
	if def.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, def.Timeout)
	}
	defer cancel()

	res := Result{ID: name, Name: name, Agent: def.Name, SessionFile: file}
	var runErr error
	for ev := range sub.Run(runCtx, nil) {
		switch ev.Type {
		case agent.EventMessageEnd:
			res.Requests++
			res.Usage = res.Usage.Add(ev.Ended.Usage)
			if txt := textOf(ev.Ended.Message); txt != "" {
				res.Text = txt
			}
		case agent.EventToolStart:
			res.ToolCalls++
			if ev.ToolStart.Name == "yield" {
				var args map[string]any
				if json.Unmarshal([]byte(ev.ToolStart.Args), &args) == nil {
					if d, ok := args["data"]; ok {
						res.Data = d
					}
				}
			}
		case agent.EventTerminated:
			if ev.Terminated.ToolName == "yield" {
				res.Yielded = true
			}
		case agent.EventError:
			runErr = ev.Err
		}
	}
	res.DurationMs = time.Since(start).Milliseconds()

	switch {
	case ctx.Err() != nil:
		res.Status, res.Err = StatusAborted, ctx.Err()
	case runCtx.Err() != nil:
		res.Status, res.Err = StatusTimeout, fmt.Errorf("子 agent 超时（%s）", def.Timeout)
	case runErr != nil:
		res.Status, res.Err = StatusFailed, runErr
	case r.Schema != nil && res.Data == nil:
		res.Status, res.Err = StatusFailed, errors.New("子 agent 未通过 yield 产出符合 schema 的结构化数据")
	default:
		res.Status = StatusCompleted
	}
	_ = sess.AppendCustom("session_exit", map[string]any{
		"status": StatusString(res.Status), "requests": res.Requests, "toolCalls": res.ToolCalls,
		"yielded": res.Yielded, "durationMs": res.DurationMs,
	})
	return res
}

// buildTools 构造一个 Run 的工具集，并返回「只含 yield」的备用注册表（强制收尾那一 turn 用）。
// 规则：默认集（或定义指定的子集）→ 只读 agent 裁到读工具 → 加 yield → 满足 spawn policy 与深度才加 task。
func (m *Manager) buildTools(def AgentDef, depth int, store *runtime.ArtifactStore) (all, yieldOnly *tool.Registry) {
	base := tool.NewRegistry()
	if m.o.WorkerTools != nil {
		base = m.o.WorkerTools(m.o.CWD, store)
	}
	allowed := map[string]bool{}
	for _, n := range def.Tools {
		allowed[n] = true
	}
	readOnly := map[string]bool{"read_file": true, "glob": true, "grep": true}

	all = tool.NewRegistry()
	for _, t := range base.List() {
		if t.Name() == "task" { // 递归派发只能由 spawn policy 显式打开
			continue
		}
		if len(allowed) > 0 && !allowed[t.Name()] {
			continue
		}
		if def.ReadOnly && !readOnly[t.Name()] {
			continue
		}
		all.Register(t)
	}
	y := NewYieldTool()
	all.Register(y)

	maxDepth := m.o.MaxDepth
	if maxDepth <= 0 {
		maxDepth = defaultMaxDepth
	}
	if len(def.Spawns) > 0 && depth < maxDepth {
		all.Register(NewTaskTool(m, depth, def.Name, def.Spawns))
	}

	yieldOnly = tool.NewRegistry()
	yieldOnly.Register(y)
	return all, yieldOnly
}

// buildTaskPrompt 把批次共享背景与本项任务拼成子 agent 的第一条用户消息。
func buildTaskPrompt(batchContext string, item TaskItem) string {
	var sb strings.Builder
	if c := strings.TrimSpace(batchContext); c != "" {
		sb.WriteString("<batch-context>\n")
		sb.WriteString(c)
		sb.WriteString("\n</batch-context>\n\n")
	}
	sb.WriteString("<task>\n")
	sb.WriteString(item.Task)
	sb.WriteString("\n</task>")
	return sb.String()
}

// subagentCompletionNote 附在子 agent 系统提示后：说明 yield 协议与禁止事项。
const subagentCompletionNote = `

## 完成协议
你是被主 agent 派来完成一项子任务的执行者。工作期间只用工具推进，不做进度汇报。
任务完成时必须调用 yield 工具提交结构化产出 data（这是返回结果的唯一方式，调用后运行立即结束）。
如果确实无法完成，也要调用 yield，在 error 参数里说明尝试过什么、卡在哪里。`

// openSidecar 为子 agent 建独立会话：有 SessionDir 则落盘为 agent-<name>-<rand>.jsonl，否则内存。
func (m *Manager) openSidecar(name string, def AgentDef, r Resolved, tools *tool.Registry, depth int) (*session.Session, string, error) {
	var st session.Storage
	file := ""
	if m.o.SessionDir != "" {
		if err := os.MkdirAll(m.o.SessionDir, 0o755); err != nil {
			return nil, "", err
		}
		// 文件名带随机后缀：同名任务重跑 / 会话 resume 后再派发都不会追加进旧转录
		file = filepath.Join(m.o.SessionDir, "agent-"+sanitizeName(name)+"-"+randSuffix()+".jsonl")
		fs, err := session.NewFileStorage(file)
		if err != nil {
			return nil, "", err
		}
		st = fs
	} else {
		st = &session.MemoryStorage{}
	}
	sess, err := session.NewWithHeader(session.Header{ID: "agent-" + name, CWD: m.o.CWD, ParentSession: filepath.Base(m.o.SessionDir)}, st)
	if err != nil {
		return nil, "", err
	}
	names := make([]string, 0, 8)
	for _, t := range tools.List() {
		names = append(names, t.Name())
	}
	_ = sess.AppendInit(session.SessionInit{
		Agent: def.Name, SystemPrompt: def.SystemPrompt, Task: r.Item.Task, Tools: names,
		OutputSchema: r.Schema, Depth: depth,
	})
	return sess, file, nil
}

func randSuffix() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (m *Manager) acquire(ctx context.Context) error {
	select {
	case m.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) release() { <-m.sem }

func textOf(m message.Message) string {
	var sb strings.Builder
	for _, b := range m.Blocks {
		if b.Kind == message.BlockText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}
