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
	Defs           []SubagentSpec
	Summarizer     agentctx.Summarizer
	ContextWindow  int
}

// Manager 派发子 agent。
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
	return &Manager{o: o, sem: make(chan struct{}, o.MaxConcurrency)}
}

// List 返回子 agent 定义（确定性顺序，供 task 工具枚举）。
func (m *Manager) List() []SubagentSpec { return m.o.Defs }

func (m *Manager) find(name string) (SubagentSpec, bool) {
	for _, d := range m.o.Defs {
		if d.Name == name {
			return d, true
		}
	}
	return SubagentSpec{}, false
}

// Run 派发一个子 agent 并等待其结束，返回 Result。
// Context Isolation：子 agent 只看到 system + task，不看父历史；父只拿结构化产出/最后文本 + 转录指针。
func (m *Manager) Run(ctx context.Context, t Task) Result {
	name := t.Name
	if name == "" {
		name = m.defaultName(t.Subagent)
	}
	start := time.Now()
	def, ok := m.find(t.Subagent)
	if !ok {
		return Result{ID: t.Subagent, Name: name, Status: StatusFailed, Err: fmt.Errorf("unknown subagent %q", t.Subagent)}
	}

	// 运行时：独立产物存储 / 工具集（独立 bash） / 审批
	var store *runtime.ArtifactStore
	if m.o.SessionDir != "" {
		store = runtime.NewArtifactStore(m.o.SessionDir)
	}
	var tools *tool.Registry
	if m.o.WorkerTools != nil {
		tools = m.o.WorkerTools(m.o.CWD, store)
	} else {
		tools = tool.NewRegistry()
	}
	tools = tools.Without("task") // 防递归
	tools.Register(NewYieldTool())
	var approver tool.Approver = denyApprover{}
	if m.o.Escalate && m.o.Approver != nil {
		approver = labeledApprover{inner: m.o.Approver, label: "[子 agent " + name + "]"}
	}
	exec := tool.NewExecutor(tools, m.o.Mode, approver)
	if store != nil {
		exec.SetArtifactStore(store)
	}

	// sidecar 会话
	sess, file, err := m.openSidecar(name, def, t)
	if err != nil {
		return Result{ID: t.Subagent, Name: name, Status: StatusFailed, Err: err}
	}
	defer sess.Close()

	system := func(context.Context) []message.Message {
		return []message.Message{message.NewSystemMessage(def.SystemPrompt + subagentCompletionNote)}
	}
	cc := agentctx.New(sess, m.o.Summarizer, m.o.ContextWindow, 16384, system)
	_ = cc.Record(message.NewUserMessage(t.Prompt), model.Usage{})

	sub := agent.New(def.Name, m.o.Model, tools, exec, cc)
	sub.SetMaxIterations(def.MaxTurns)

	runCtx := ctx
	cancel := context.CancelFunc(func() {})
	if def.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, def.Timeout)
	}
	defer cancel()

	res := Result{ID: t.Subagent, Name: name, SessionFile: file}
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
			if ev.ToolStart.Name == "yield" {
				var args map[string]any
				if json.Unmarshal([]byte(ev.ToolStart.Args), &args) == nil {
					if d, ok := args["data"].(map[string]any); ok {
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
	case def.OutputSchema != nil && res.Data == nil:
		res.Status, res.Err = StatusFailed, errors.New("子 agent 未通过 yield 产出符合 schema 的结构化数据")
	default:
		res.Status = StatusCompleted
	}
	_ = sess.AppendCustom("session_exit", map[string]any{
		"status": StatusString(res.Status), "requests": res.Requests, "yielded": res.Yielded, "durationMs": res.DurationMs,
	})
	return res
}

// subagentCompletionNote 附在子 agent 系统提示后：说明 yield 协议与禁止事项。
const subagentCompletionNote = `

## 完成协议
你是被主 agent 派来完成一项子任务的执行者。工作期间只用工具推进，不做进度汇报。
任务完成时必须调用 yield 工具提交结构化产出 data（这是返回结果的唯一方式，调用后运行立即结束）。
如果确实无法完成，也要调用 yield，在 data.error 里说明尝试过什么、卡在哪里。`

// openSidecar 为子 agent 建独立会话：有 SessionDir 则落盘为 agent-<name>.jsonl，否则内存。
func (m *Manager) openSidecar(name string, def SubagentSpec, t Task) (*session.Session, string, error) {
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
	_ = sess.AppendInit(session.SessionInit{Agent: def.Name, SystemPrompt: def.SystemPrompt, Task: t.Prompt, OutputSchema: def.OutputSchema, Depth: 1})
	return sess, file, nil
}

func randSuffix() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func sanitizeName(n string) string {
	var b strings.Builder
	for _, r := range n {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "agent"
	}
	return b.String()
}

// defaultName 生成稳定的默认运行名：<subagent>-<序号>。
func (m *Manager) defaultName(subagent string) string {
	return fmt.Sprintf("%s-%d", subagent, m.seq.Add(1))
}

// RunMany 并行派发多个子 agent（Semaphore 限并发），结果按输入序返回。
func (m *Manager) RunMany(ctx context.Context, tasks []Task) []Result {
	results := make([]Result, len(tasks))
	var wg sync.WaitGroup
	for i, t := range tasks {
		if t.Name == "" { // 在派发前按输入序命名，避免并发下序号乱序
			t.Name = m.defaultName(t.Subagent)
		}
		if err := m.acquire(ctx); err != nil {
			results[i] = Result{ID: t.Subagent, Name: t.Name, Status: StatusAborted, Err: err}
			continue
		}
		wg.Add(1)
		go func(i int, t Task) {
			defer wg.Done()
			defer m.release()
			defer func() {
				if r := recover(); r != nil {
					results[i] = Result{ID: t.Subagent, Name: t.Name, Status: StatusFailed, Err: fmt.Errorf("子 agent panic: %v", r)}
				}
			}()
			results[i] = m.Run(ctx, t)
		}(i, t)
	}
	wg.Wait()
	return results
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
