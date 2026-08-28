package subagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
// NoteSink 是项目笔记沉淀接口（生产实现是 memory.Store）。explorer 类子 agent 的
// 结构化产出在结算时确定性 upsert 成项目知识，不调模型。
type NoteSink interface {
	UpsertNote(path, summary, symbols string) error
}

type Options struct {
	Model          model.Model
	WorkerTools    func(cwd string, store *runtime.ArtifactStore) *tool.Registry // 每个 Run 调用一次，返回独立工具集
	Memory         memory.Recaller
	Mode           permission.Mode  // 继承父的审批模式（不是 yolo）
	Rules          permission.Rules // 继承父的审批规则（deny 在任何模式都生效）
	Approver       tool.Approver    // 父的审批器；Escalate 时使用
	Escalate       bool             // true = headless 子 agent 的 Prompt 决策升级到父审批（弹窗带子 agent 标签）
	SessionDir     string           // 父会话产物目录；"" = 不落盘（MemoryStorage）
	CWD            string
	MaxConcurrency int
	Defs           []AgentDef
	Summarizer     agentctx.Summarizer
	ContextWindow  int
	Bus            *bus.Bus // 事件总线（可 nil：不发布）
	Notes          NoteSink // 项目笔记沉淀（explorer 产出 → file_notes；nil = 不沉淀）

	// 定义里没写时的兜底
	DefaultTimeout  time.Duration
	DefaultMaxTurns int
	SoftBudget      int // 全局软预算上限；0 = 关闭护栏。定义里的值只能更小

	MaxDepth        int  // 递归深度上限（0 = 默认 2）
	MinTaskChars    int  // 任务描述最短长度（0 = 默认 40）
	AllowBackground bool // false = 忽略 task 的 background 参数（全部同步执行）
}

// Manager 派发子 agent，并持有 Run 名册。
// 名册按运行名索引（运行名同时是 hub 地址、agent:// 地址与作业 id，预检保证唯一）；
// 结束的 Run 留在名册里（parked），这样父 agent 与 TUI 事后还能读它的产出与转录。
type Manager struct {
	o    Options
	gate *gate
	seq  atomic.Int64

	// 后台作业挂在这个根 ctx 上：父 turn 结束、用户 Esc 都不该带走后台任务，只有进程退出才收。
	root       context.Context
	rootCancel context.CancelFunc
	wg         sync.WaitGroup

	mu      sync.Mutex
	runs    map[string]*Run
	order   []string // 启动顺序，名册展示用
	pending []JobResult
	boxes   map[string]*mailbox
}

// maxParkedRuns 名册里保留的已结束 Run 上限：超出后丢最早的（磁盘上的转录与产出不删）。
const maxParkedRuns = 100

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
	root, cancel := context.WithCancel(context.Background())
	return &Manager{o: o, gate: newGate(o.MaxConcurrency), runs: map[string]*Run{},
		boxes: map[string]*mailbox{}, root: root, rootCancel: cancel}
}

// register 把 Run 放进名册（同名覆盖：预检已保证同时只有一个活的同名 Run）。
func (m *Manager) register(r *Run) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runs[r.name]; !ok {
		m.order = append(m.order, r.name)
	}
	m.runs[r.name] = r
	m.pruneLocked()
}

// pruneLocked 名册超限时丢最早的已结束 Run。
func (m *Manager) pruneLocked() {
	settled := 0
	for _, n := range m.order {
		if r := m.runs[n]; r != nil && r.settled() {
			settled++
		}
	}
	for i := 0; i < len(m.order) && settled > maxParkedRuns; i++ {
		n := m.order[i]
		if r := m.runs[n]; r != nil && r.settled() {
			delete(m.runs, n)
			m.order = append(m.order[:i], m.order[i+1:]...)
			i--
			settled--
		}
	}
}

// lookup 按运行名取 Run（含已结束的 parked）。
func (m *Manager) lookup(name string) *Run {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runs[name]
}

// Roster 返回名册快照（按启动顺序）。
func (m *Manager) Roster() []RunView {
	m.mu.Lock()
	runs := make([]*Run, 0, len(m.order))
	for _, n := range m.order {
		if r := m.runs[n]; r != nil {
			runs = append(runs, r)
		}
	}
	m.mu.Unlock()
	out := make([]RunView, 0, len(runs))
	for _, r := range runs {
		out = append(out, r.View())
	}
	return out
}

// names 返回名册里的运行名（错误提示用）。
func (m *Manager) names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.order))
	for _, n := range m.order {
		if _, ok := m.runs[n]; ok {
			out = append(out, n)
		}
	}
	return out
}

// Cancel 按运行名终止若干 Run，返回实际终止数（已结束的不计）。
func (m *Manager) Cancel(names []string) int {
	n := 0
	for _, name := range names {
		if r := m.lookup(name); r != nil && !r.settled() {
			r.Cancel()
			n++
		}
	}
	return n
}

// Deliver 投递一条 hub 消息，返回给发送方的送达回执。三种去向：
//   - Main：进主信箱，由 TUI/headless 取走并注入主会话
//   - 运行中的子 agent：作为 steering 注入它的下一步
//   - 已结束的子 agent：唤醒续跑（后台作业），结果按 async-result 回投
func (m *Manager) Deliver(from, to, text, replyTo string) (string, error) {
	mail := Mail{From: from, Text: text, ReplyTo: replyTo, At: time.Now()}
	if to == MainName {
		m.box(MainName).push(mail)
		if m.o.Bus != nil {
			m.o.Bus.Publish(ChMailbox, MailArrived{To: MainName, From: from, Text: text})
		}
		return "已送达 Main", nil
	}
	r := m.lookup(to)
	if r == nil {
		return "", fmt.Errorf("没有名为 %q 的 peer；当前名册：%s", to, strings.Join(append([]string{MainName}, m.names()...), ", "))
	}
	if r.settled() {
		job, err := m.Revive(to, formatMail(mail))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s 已结束，已唤醒它续跑（作业 %s）；结果完成后会自动送到你这里", to, job.ID), nil
	}
	if !r.Steer(message.NewUserMessage(formatMail(mail))) {
		return "", fmt.Errorf("%s 的消息队列已满，稍后再试", to)
	}
	return "已送达 " + to, nil
}

// Send 是 Deliver 的简化入口（TUI 的 /agent 用）。
func (m *Manager) Send(from, to, text string) (string, error) { return m.Deliver(from, to, text, "") }

// putBack 把不该由本次调用消费的作业结果放回待投递队列。
func (m *Manager) putBack(rs []JobResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending = append(rs, m.pending...)
}

// formatMail 把消息渲染成注入目标会话的一句话（可审计：转录里能看出是谁说的）。
func formatMail(ml Mail) string {
	s := "[hub from " + ml.From + "]"
	if ml.ReplyTo != "" {
		s += "（回复 " + ml.ReplyTo + "）"
	}
	return s + " " + ml.Text
}

// MailArrived 是「有消息送到 Main」的广播（TUI 用它触发取件）。
type MailArrived struct{ To, From, Text string }

// RegisterSchemes 把 agent:// 与 history:// 挂到一个产物存储上，
// 让 read_file 成为读回子 agent 产出与转录的唯一入口。
func (m *Manager) RegisterSchemes(store *runtime.ArtifactStore) { m.registerSchemes(store) }

func (m *Manager) registerSchemes(store *runtime.ArtifactStore) {
	if store == nil {
		return
	}
	store.AddScheme("agent", m.ResolveAgentURL)
	store.AddScheme("history", m.ResolveHistoryURL)
}

// ResolveAgentURL 解析 agent://<Name> → 该 Run 的完整产出文件。
func (m *Manager) ResolveAgentURL(name string) (string, error) {
	if r := m.lookup(name); r != nil {
		if f := r.View().OutputFile; f != "" {
			return f, nil
		}
	}
	// 名册里没有（例如 resume 之后）：回落到产物目录里按名字找
	p := filepath.Join(m.o.SessionDir, sanitizeName(name)+".md")
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("找不到 %q 的产出；当前名册：%s", name, strings.Join(m.names(), ", "))
}

// ResolveHistoryURL 解析 history://<Name> → 该 Run 的 sidecar 转录。
func (m *Manager) ResolveHistoryURL(name string) (string, error) {
	if r := m.lookup(name); r != nil {
		if f := r.View().SessionFile; f != "" {
			return f, nil
		}
	}
	matches, _ := filepath.Glob(filepath.Join(m.o.SessionDir, "agent-"+sanitizeName(name)+"-*.jsonl"))
	if len(matches) == 0 {
		return "", fmt.Errorf("找不到 %q 的转录；当前名册：%s", name, strings.Join(m.names(), ", "))
	}
	newest, newestAt := matches[0], time.Time{}
	for _, p := range matches {
		if st, err := os.Stat(p); err == nil && st.ModTime().After(newestAt) {
			newest, newestAt = p, st.ModTime()
		}
	}
	return newest, nil
}

// List 返回子 agent 定义（确定性顺序，供 task 工具枚举）。
func (m *Manager) List() []AgentDef { return m.o.Defs }

// Env 返回一次派发的调用者环境：主 agent 用 Env(0, "", nil)，子 agent 用 Env(depth, 自己的 agent 名, 自己的 spawns)。
func (m *Manager) Env(depth int, self string, spawns []string) Env {
	return Env{
		Defs: m.o.Defs, Depth: depth, MaxDepth: m.o.MaxDepth, Spawns: spawns, SelfAgent: self,
		MinTaskChars: m.o.MinTaskChars,
		SeqNext:      func(agent string) string { return fmt.Sprintf("%s-%d", agent, m.seq.Add(1)) },
		NameTaken:    func(name string) bool { return m.lookup(name) != nil }, // 名册里的名字（含 parked）不复用
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
		if err := m.gate.acquire(ctx); err != nil {
			results[i] = Result{Name: it.Item.Name, Agent: it.Item.Agent, Status: StatusAborted, Err: err}
			continue
		}
		wg.Add(1)
		go func(i int, it Resolved) {
			defer wg.Done()
			defer m.gate.release()
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
	run := newRun(r.Item.Name, def.Name, depth)
	m.register(run)
	rs, err := m.setup(def, r, batchContext, depth, run)
	if err != nil {
		run.setStatus(StatusFailed)
		return Result{ID: run.name, Name: run.name, Agent: def.Name, Status: StatusFailed, Err: err}
	}
	defer rs.sess.Close()
	return m.drive(ctx, run, rs)
}

// setup 装配一个 Run 的运行时：新建 sidecar 会话、写 session_init、记录任务，
// 并把重建所需的信息挂到 Run 上（唤醒续跑时用）。
func (m *Manager) setup(def AgentDef, r Resolved, batchContext string, depth int, run *Run) (*runtimeSet, error) {
	sess, file, err := m.openSidecar(r.Item.Name, def, r, depth)
	if err != nil {
		return nil, err
	}
	rs := m.buildRuntime(def, r.Item.Name, depth, sess, file, r.Schema, r.SchemaMode)
	if err := rs.cc.Record(message.NewUserMessage(buildTaskPrompt(batchContext, r.Item)), model.Usage{}); err != nil {
		sess.Close()
		return nil, err
	}
	if run != nil {
		run.mu.Lock()
		run.sessionFile = file
		run.spawn = spawnSpec{def: def, item: r.Item, batchContext: batchContext,
			schema: r.Schema, mode: r.SchemaMode, depth: depth, file: file}
		run.mu.Unlock()
	}
	// session_init 里带上工具集，转录本身就能回答「它当时能做什么」
	names := make([]string, 0, 8)
	for _, t := range rs.tools.List() {
		names = append(names, t.Name())
	}
	_ = sess.AppendInit(session.SessionInit{
		Agent: def.Name, SystemPrompt: def.SystemPrompt, Task: r.Item.Task, Tools: names,
		OutputSchema: r.Schema, Depth: depth,
	})
	return rs, nil
}

// setupResume 打开已有 sidecar 续跑：不写 session_init、不重记任务，历史由回放带回来。
func (m *Manager) setupResume(spec spawnSpec) (*runtimeSet, error) {
	st, err := session.NewFileStorage(spec.file)
	if err != nil {
		return nil, err
	}
	sess, err := session.Open(st)
	if err != nil {
		st.Close()
		return nil, err
	}
	return m.buildRuntime(spec.def, spec.item.Name, spec.depth, sess, spec.file, spec.schema, spec.mode), nil
}

// buildRuntime 组装工具集（含只含 yield 的备用集）、继承父审批的执行器与上下文管理器。
func (m *Manager) buildRuntime(def AgentDef, name string, depth int, sess *session.Session, file string,
	schema map[string]any, mode string) *runtimeSet {
	var store *runtime.ArtifactStore
	if m.o.SessionDir != "" {
		store = runtime.NewArtifactStore(m.o.SessionDir)
		m.registerSchemes(store) // 子 agent 之间也能 read_file agent://<Name>
	}
	ys := NewYieldState()
	tools, yieldOnly := m.buildTools(def, name, depth, store, ys, schema, mode)

	var approver tool.Approver = denyApprover{}
	if m.o.Escalate && m.o.Approver != nil {
		approver = labeledApprover{inner: m.o.Approver, label: "[子 agent " + name + "]"}
	}
	exec := tool.NewExecutor(tools, m.o.Mode, approver)
	yieldExec := tool.NewExecutor(yieldOnly, m.o.Mode, approver)
	exec.SetRules(m.o.Rules)      // 子 agent 继承父的审批规则（deny 在任何模式都生效）
	yieldExec.SetRules(m.o.Rules) // yield 不受规则影响（Read tier），但保持一致以防未来改动
	if store != nil {
		exec.SetArtifactStore(store)
		yieldExec.SetArtifactStore(store)
	}
	system := func(context.Context) []message.Message {
		return []message.Message{message.NewSystemMessage(def.SystemPrompt + subagentCompletionNote)}
	}
	return &runtimeSet{
		def: def, tools: tools, yieldOnly: yieldOnly, exec: exec, yieldExec: yieldExec,
		cc:   agentctx.New(sess, m.o.Summarizer, m.o.ContextWindow, 16384, system),
		sess: sess, file: file, ys: ys, schema: schema, mode: mode,
	}
}

// buildTools 构造一个 Run 的工具集，并返回「只含 yield」的备用注册表（强制收尾那一 turn 用）。
// 规则：默认集（或定义指定的子集）→ 只读 agent 裁到读工具 → 加 yield → 满足 spawn policy 与深度才加 task。
func (m *Manager) buildTools(def AgentDef, name string, depth int, store *runtime.ArtifactStore, ys *YieldState, schema map[string]any, mode string) (all, yieldOnly *tool.Registry) {
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
	y := NewYieldTool(ys, schema, mode)
	all.Register(y)
	all.Register(NewHubTool(m, name)) // 协调用：看名册、给同伴发消息、等作业

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
func (m *Manager) openSidecar(name string, def AgentDef, r Resolved, depth int) (*session.Session, string, error) {
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
	return sess, file, nil
}

func randSuffix() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func textOf(m message.Message) string {
	var sb strings.Builder
	for _, b := range m.Blocks {
		if b.Kind == message.BlockText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}
