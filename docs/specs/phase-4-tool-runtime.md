# Phase 4 详细设计：Tool Runtime + Permission

> 状态：**待评审** · 所属 spec：[harness-rearchitecture.md](harness-rearchitecture.md) · 前置：P0-P3 已完成
> 本阶段把「工具」从「调用函数」升级成「运行时」：Tool 接口 + 注册表 + bash 子进程 + OutputSink（工具结果压缩/offloading）+ 审批纯策略 + 结构化检索 + 三档中断。对应分层 Context 的 **L6 Tool Results**。

---

## 0. 目标与边界

### 本阶段产出（P4 完成时）

1. `internal/permission/policy.go` —— 审批纯策略（Tier/Mode/Resolve）。
2. `internal/runtime/sink.go` —— `OutputSink`（头尾窗口截断 + artifact 落盘，L6）。
3. `internal/runtime/bash.go` + `sandbox.go` —— bash 子进程运行时 + env 硬化。
4. `internal/tool/tool.go` —— `Tool` 接口。
5. `internal/tool/registry.go` —— 统一注册表 + 内置工具。
6. `internal/tool/executor.go` —— 工具执行调度（三档中断）。
7. `internal/tool/search.go` —— 结构化检索（grep + LSP + AST）。
8. `internal/tool/cache.go` —— 目录清单缓存。
9. `internal/agent` —— 把 P1 循环扩展成完整「工具循环」（step/turn/run + 工具事件）。

### 本阶段不做（defer）

- MCP（P6）、subagent（P6）、记忆（P5）。
- 交互式审批弹窗（human-in-the-loop interrupt/resume）——P4 先做**纯策略 Resolve**，Prompt 结果先降级为「拒绝并说明」；真正的暂停-审批-续跑留后续。
- shared/exclusive 并发并行执行——P4 先**顺序执行**工具调用，并行是优化（stretch）。

### 验收标准

- `env -u GOROOT go build ./...` + `go vet ./...` + `go test ./...` 通过。
- agent 能读/写文件、跑 bash；`rm -rf /` 之类触发拒绝；工具结果超长自动截断 + 落盘（`cat` 文件可看完整结果）；能 grep/LSP 查符号。

---

## 1. 参照 oh-my-pi 的核心原则

### 1.1 Tool 与 Runtime 分离（不是「调用函数」）

oh-my-pi 的核心架构：`Tool`（面向模型的入口：schema/校验/审批/结果塑形）≠ `Runtime/Executor`（进程引擎：子进程/超时/取消/流式）。同一批 read/write/bash 工具，运行时引擎可替换。P4 据此拆 `internal/tool`（入口）与 `internal/runtime`（进程引擎）。

### 1.2 Approval 是纯策略函数，不是有状态记忆

`Resolve(tier, policy, mode) → Allow|Prompt|Deny` 的**纯函数 + mode 枚举**，「记住决定」只靠 config。天然可测。

### 1.3 工具结果压缩 + 落盘 = 「Context = Index」（L6）

`OutputSink` 头尾窗口截断 + `artifact://<id>` 落盘 + `useless` 标记供 compaction 裁剪。上下文里放**指针 + 精华**，完整结果在磁盘。

### 1.4 结构化检索 = on-demand 工具（代码检索 ≠ 记忆检索）

grep/LSP/AST 是**普通工具**，结果进 transcript；与 P5 的记忆检索（语义向量 → `<memories>`）是两套独立子系统。跨文件符号关系**靠 LSP 不建依赖图**（oh-my-pi 的取舍；Go 里可补 `go list -deps`）。

### 1.5 三档中断保护副作用

硬杀（可中断的等待）/软信号（让工具让位）/跳过（未启动的工具）——**绝不硬杀一个已产生副作用的工具**。

---

## 2. 审批纯策略（`internal/permission/policy.go`）

```go
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
```

> P4 里 `Prompt` 降级为「拒绝 + 说明」（`tool denied: requires approval (tier=exec)`），不阻塞。真正的暂停-审批-续跑留后续。

---

## 3. OutputSink（`internal/runtime/sink.go`）

对应 oh-my-pi 的 `OutputSink`，是 L6「工具结果压缩 + offloading」的实现。

```go
package runtime

// Sink 累积工具输出，做头尾窗口截断 + 落盘。
type Sink struct {
	mu        sync.Mutex
	buf       []byte          // 当前累积（受窗口约束）
	headLimit int             // 头部保留字节数
	tailLimit int             // 尾部保留字节数
	truncated bool            // 是否截断过
	artifact  *os.File        // 溢出时落盘
	artifactID string
}

func NewSink(headLimit, tailLimit int) *Sink

// Write 累积输出；超窗口时保留头尾、中间截断，并把原始内容落盘。
func (s *Sink) Write(p []byte) (int, error)

// Result 返回给模型的结果文本：若截断则「头部 + ...(N bytes elided) + 尾部 + artifact 指针」。
func (s *Sink) Result() string

// Close 关闭 artifact 文件。
func (s *Sink) Close() error
```

**语义**（对应 oh-my-pi）：
- 头尾窗口：保留 `headLimit` 头部 + `tailLimit` 尾部，中间截断。
- **落盘**：截断时把完整原始输出写到 `sessions/artifacts/<id>.log`，`Result()` 里返回 `[完整输出已保存: artifact://<id>]` 指针——**Context = Index**。
- 空输出 → `(no output)`；`useless: true` 标记（供 P3 compaction 裁剪）。

---

## 4. bash 运行时（`internal/runtime/bash.go` + `sandbox.go`）

```go
package runtime

// Bash 执行一条 shell 命令，输出进 Sink。
// ctx 取消 → 杀子进程；cwd 持久化（会话内 shell 状态复用）。
type Bash struct {
	mu  sync.Mutex
	cwd string // 会话内持久 cwd
}

func (b *Bash) Execute(ctx context.Context, command string, sink *Sink) error
```

- **子进程**：`exec.Command("bash", "-c", command)`，`cmd.Dir = b.cwd`，stdout/stderr 都进 sink。
- **env 硬化**（`sandbox.go` 的 `nonInteractiveEnv()`）：`PAGER=cat` / `EDITOR=true` / `GIT_TERMINAL_PROMPT=0` / `TERM=dumb` / `NO_COLOR=1` / `CI=true`。
- **取消**：`exec.CommandContext(ctx, ...)`，ctx 取消即杀进程（三档中断的「硬杀」）。
- **cwd 持久化**：命令里的 `cd X && ...` 更新 `b.cwd`（简单解析，P4 先支持前缀 `cd <path> &&`）。

> PTY（交互式终端）P4 不做——非 PTY 已够跑 `go test`/`ls`/`grep` 等；PTY 是 stretch。

---

## 5. Tool 接口 + 注册表（`internal/tool/tool.go` + `registry.go`）

```go
package tool

// Tool 是面向模型的工具入口。
type Tool interface {
	Name() string
	Description() string
	Parameters() map[string]any // JSON Schema properties（给模型的工具定义）
	Tier() permission.Tier
	Execute(ctx context.Context, args map[string]any, sink *runtime.Sink) (string, error)
}

// Registry 统一注册表。
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

func NewRegistry() *Registry
func (r *Registry) Register(t Tool)
func (r *Registry) Get(name string) (Tool, bool)
func (r *Registry) Specs() []model.ToolSpec // 转成给模型的工具定义
```

**内置工具**（P4 首批）：

| 工具 | Tier | 说明 |
|---|---|---|
| `read_file` | read | 读文件（支持 offset/limit） |
| `glob` | read | 文件名匹配 |
| `grep` | read | 文本搜索（§8） |
| `lsp` | read | LSP 符号检索（§8） |
| `write_file` | write | 写文件 |
| `edit_file` | write | 编辑文件 |
| `bash` | exec | 执行 shell |

---

## 6. 工具执行 + 三档中断（`internal/tool/executor.go` + agent 循环扩展）

### 6.1 Agent 循环扩展成完整「工具循环」

P1 的 `Run` 只跑一次模型调用。P4 扩展成 step/turn/run 三层：

```go
func (a *Agent) Run(ctx context.Context, input []message.Message) <-chan AgentEvent {
	// 与 P1 相同的外层：agent_start / turn_start
	msgs := append([]message.Message{system}, input...)

	for step := 0; step < a.maxIterations; step++ {
		stream := a.model.Stream(ctx, msgs, a.tools.Specs())
		assistant := consumeStream(ctx, stream, emit) // 累积成 assistant 消息（text + toolCalls）
		msgs = append(msgs, assistant)

		if len(toolCallsOf(assistant)) == 0 {
			break // 无工具调用，turn 结束
		}
		for _, tc := range toolCallsOf(assistant) {
			// 三档中断的「跳过」：启动前检查 ctx
			if ctx.Err() != nil {
				break
			}
			emit(toolExecutionStart{tc})
			result := a.executeTool(ctx, tc) // 审批 + 执行
			emit(toolExecutionEnd{tc, result})
			msgs = append(msgs, message.NewToolMessage(tc.ID, tc.Name, result, false))
		}
	}
	// turn_end / agent_end
}
```

### 6.2 executeTool：审批 → 执行

```go
func (a *Agent) executeTool(ctx, tc) string {
	t, ok := a.tools.Get(tc.Name)
	if !ok {
		return "tool not found: " + tc.Name
	}
	decision := permission.Resolve(t.Tier(), a.mode)
	if decision == DecisionPrompt {
		return "tool denied: requires approval (tier=" + string(t.Tier()) + ")"
	}
	sink := runtime.NewSink(head, tail)
	defer sink.Close()
	result, err := t.Execute(ctx, args, sink)
	if err != nil {
		return "tool error: " + err.Error()
	}
	return sink.Result() // 截断后的文本（含 artifact 指针）
}
```

### 6.3 三档中断

| 档 | 触发 | P4 落地 |
|---|---|---|
| **硬杀** | ctx 取消（Ctrl+C / 新消息 preempt） | `CommandContext` 杀 bash 子进程 |
| **软信号** | 让「可后台化」工具让位 | P4 工具都是同步快速/可硬杀，暂不单独实现；结构上预留 |
| **跳过** | 未启动的工具 | 循环启动每个工具前检查 `ctx.Err()` |

> 「绝不硬杀已产生副作用的工具」在 P4 天然满足：write/edit 是快速原子操作（硬杀窗口极小），bash 可被 CommandContext 干净终止（子进程死，不污染状态）。

---

## 7. 结构化检索（`internal/tool/search.go`）

对应 §1.4。三块能力，P4 先做 **grep**（最有价值、最易），LSP/AST 按需接：

### 7.1 grep（容错双引擎 + 截断）

- **双引擎降级**：`regexp`（RE2 语义，线性时间）→ 失败试 `regexp2`（PCRE，支持 lookaround）→ 失败 fallback `regexp.QuoteMeta` 字面量——**永不因 pattern 非法而报错**。
- **截断**：`maxMatches`（总匹配）+ `maxMatchesPerFile` + 行宽截断；结果进 Sink。
- **忽略规则**：`.gitignore` / `node_modules` / `.git` 跳过。

### 7.2 LSP（gopls，符号级跨文件检索主力）

- **接 gopls**：`exec.Command("gopls", "serve")`，stdio JSON-RPC（`Content-Length` 帧）。
- **能力**：`definition` / `references` / `workspace/symbol` / `hover` / `symbols`（outline）。
- **生命周期**：惰性 spawn（首次调用才起）+ `initFailures` 负缓存退避；`didOpen`/`didChange` 同步。
- **结果整形**：`references` 前 N 个带目标行上下文，其余只给 `文件:行`。

### 7.3 AST（`go/parser`/`go/ast`/`go/types`）

- Go 项目用标准库（零依赖，符号级信息比 tree-sitter 强）。
- `summarizeCode` 式折叠：大文件进上下文前，BFS 折叠函数体/import 段到目标行数。
- 内容寻址 parse 缓存（key = 内容 hash，命中逐字节校验）。

> P4 实现优先级：grep（必做）> LSP（建议做，gopls 已广泛可用）> AST 折叠（stretch）。全部是 on-demand 工具，结果进 transcript。

---

## 8. 目录清单缓存（`internal/tool/cache.go`）

对应 oh-my-pi 的 `fs-scan-cache`：**只缓存目录枚举（path+type+mtime），不缓存内容/检索结果**。

```go
type ScanCache struct {
	mu    sync.Mutex
	items map[string]scanEntry // key = 规范化 root + 遍历选项 hash
}
type scanEntry struct {
	createdAt time.Time
	entries   []string
}
```

- TTL 极短（1s），`Get` 命中即返回 `entries + cacheAge`。
- **写后失效**：`write_file`/`edit_file`/`bash` 写盘成功后调 `Invalidate(path)`。
- 用「低延迟」换「近实时一致性」，且绝不缓存内容级结果。

---

## 9. 边界情况与错误处理

| 情况 | 处理 |
|---|---|
| 工具不存在（模型幻觉出工具名） | 返回 `tool not found`，不崩 |
| 审批 Prompt（P4 降级） | 返回 `tool denied` |
| 工具执行错误 | 返回 `tool error: ...`，不崩 agent |
| 工具输出超长 | Sink 截断 + 落盘 + 返回 artifact 指针 |
| ctx 取消（bash 运行中） | CommandContext 杀子进程，返回部分结果 |
| pattern 非法 | grep 双引擎降级到字面量，不报错 |
| LSP 未安装 | 惰性 spawn 失败 → 返回「gopls 不可用」，不影响其它工具 |

---

## 10. 对外契约（后续阶段依赖）

| 类型/函数 | 包 | 后续谁依赖 |
|---|---|---|
| `tool.Tool` / `Registry` | `internal/tool` | `internal/agent`（P4）、`internal/subagent`（P6）、MCP（P6） |
| `runtime.Sink` | `internal/runtime` | `internal/tool`（P4）、`internal/context`（P3 compaction 裁剪 `useless`） |
| `permission.Resolve` / `Tier` / `Mode` | `internal/permission` | `internal/agent`（P4）、`internal/subagent`（P6 yolo） |
| `AgentEvent` 工具事件（toolExecutionStart/End） | `internal/agent` | `internal/tui`（工具展示） |

---

## 11. 待评审点

1. **审批 Prompt 降级为「拒绝 + 说明」**（不做交互式暂停-审批-续跑，留后续）——接受吗？
2. **工具调用顺序执行**（shared/exclusive 并行是 stretch）——接受吗？
3. **PTY 交互式终端不做**（非 PTY 已够，PTY stretch）——接受吗？
4. **结构化检索优先级 grep > LSP > AST 折叠**，AST 折叠是 stretch——接受吗？
5. **`bash` 的 cwd 持久化用「前缀 `cd <path> &&` 解析」**（不做完整 shell 状态模拟）——接受吗？
