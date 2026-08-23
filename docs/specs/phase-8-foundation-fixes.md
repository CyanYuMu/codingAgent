# Phase 8 详细设计：地基修正（M1 · Foundation Fixes）

> 状态：**已评审通过，实施中** · 日期：2026-08-24 · 所属方案：[2026-08-24-evolution-plan.md](2026-08-24-evolution-plan.md) §9 M1
> 前置：P0–P7 + P6-L1/L2 已完成
> 目标：让三条承重不变量在代码里真正成立——**会话/记忆按项目隔离、压缩不拆配对、子 agent 真正可控**——并给 harness 一个可自测的 headless 入口。

---

## 0. 已拍板决策

| 决策 | 结论 |
|---|---|
| headless 子 agent 遇到 `Prompt` 审批决策 | **默认拒绝并说明**（结果文本告知模型"headless 子 agent 无法审批，已拒绝"）；配置 `subagent.approval_escalation: true` 时升级到父审批弹窗（弹窗标注子 agent 名） |
| 默认审批模式 | `approval_mode` 默认从 `yolo` 改为 **`write`**（read/write 自动放行，exec 询问）；`--yolo` 作为显式命令行开关 |
| 数据目录 | 全部迁到 `~/.codeclaw/`（`CODECLAW_HOME` 可覆盖），按规范化 cwd 分桶；仓库根目录的 `config.yaml` 仅作向后兼容的最低优先级层 |

---

## 1. 本阶段产出与边界

### 产出

| 包 | 改动 |
|---|---|
| `internal/paths`（新） | 数据目录、项目分桶、项目身份、配置路径 |
| `cmd/agent/config.go` | 三层配置合并（用户 → 项目 → 仓库内 legacy），新默认值，`subagent` 段，`--yolo` / `-p` / `--cwd` 参数 |
| `internal/session` | Entry v2（id/parentId/ts、header 含 cwd/title/parent/model）、leaf 指针、`FirstKeptEntryID` 压缩、`session_init`/`custom` 条目、回放修复悬空 tool_call、会话清单带标题 |
| `internal/model` | `ToolSpec.Required` + 完整 JSON Schema 透传；`ModelStream` 接口（可 fake）；`IsContextOverflow` / `IsRetryable` |
| `internal/context` | `Manager` v2：`Build/Record/ShouldCompact/Compact/RecoverOverflow`；估算覆盖全部块；配对安全切点；完整序列化 |
| `internal/agent` | 循环以 `Context` 为真相源（每步重建输入、循环内记录）；终止型工具；溢出恢复与瞬时错误重试双通道 |
| `internal/runtime` | `ArtifactStore`（会话产物目录、id 扫描分配）；`Sink` 接产物存储；Bash 默认 cwd = 进程 cwd |
| `internal/tool` | Executor 接产物存储；`read_file` 支持 `artifact://N`；MCP 默认 `TierWrite` |
| `internal/subagent` | Manager 选项化（worker 工具工厂 / 父权限 / 升级审批 / sidecar 目录）；yield 终止；状态 `timeout`/`aborted`；每 Run 独立 Bash；sidecar 会话 + `session_init`；结果含用量/耗时/会话文件 |
| `internal/tui` | 接新循环（用户消息由循环记录）；会话列表显示标题；审批弹窗显示子 agent 标签 |
| `cmd/agent` | headless `-p "<prompt>"` 模式（stdout 输出事件与最终回复，用于自测与 CI） |
| `internal/eval` | 适配新 agent API |

### 不做（留给 M2–M4）

- yield 三态 / schema 派生参数 / idle 提醒 / 软预算（M2）
- EventBus + TUI Agent Hub + 后台作业 + `hub` 通信（M2）
- frontmatter agent 发现（M2）
- 记忆 schema v2 / FTS 清洗 / file_notes（M3）——M1 只做 **记忆库按项目分库** 这一件事
- L6 剪枝 / shake / split-turn / 预压缩（M3）——M1 只保证压缩 **正确**
- 审批规则引擎 / bash 分类器 / edit 工具 / hooks（M4）
- lazy 会话创建、title slot、blob 外置（M2 随 Hub 一起）

### 验收（可观察行为）

1. 在项目 A、B 各启动一次 `codeclaw`：`~/.codeclaw/projects/` 下出现两个桶，各自有 `current` 与会话文件；B 里问"我之前让你记住什么"召回不到 A 的记忆。
2. 用 `-p` 跑一个会触发 ≥ 15 次工具调用、且 `context_window` 被人为调小（如 8000）的任务：能观察到循环内压缩（stdout 出现 `[compaction]`），之后继续调用工具而不报 400。
3. Ctrl+C 打断一次工具调用后 `/resume`：首个请求成功（回放里悬空的 tool_call 被合成 `[interrupted]` 结果）。
4. 派一个 `Timeout: 2s` 的子 agent 做长任务：结果标 `timeout`，`Text` 含其最后一段输出，`SessionFile` 指向 sidecar。
5. `approval_mode: always-ask` 下子 agent 调 bash：父未开启升级时结果为"已拒绝"；开启 `subagent.approval_escalation: true` 后父 TUI 弹出带 `[子 agent explorer]` 前缀的审批。
6. 工具输出超过 8KB：结果尾部出现 `artifact://N`，`read_file file_path="artifact://N"` 能读回完整内容；文件位于会话产物目录。
7. `env -u GOROOT go build ./... && go vet ./... && go test ./...` 全绿。

---

## 2. 项目作用域数据目录（`internal/paths`）

```go
package paths

// Home 返回数据根目录：$CODECLAW_HOME 或 ~/.codeclaw；不存在则创建。
func Home() (string, error)

// EncodeCWD 把规范化（EvalSymlinks + Clean）后的绝对路径编码成目录名：
// 家目录下 → "-" + 相对路径（分隔符换 "-"）；其它 → "--" + 绝对路径（分隔符换 "-"） + "--"。
// 例：/Users/me/Projects/foo → -Projects-foo ；/tmp/x → --tmp-x--
func EncodeCWD(cwd string) (string, error)

// ProjectDir 返回 <Home>/projects/<EncodeCWD(cwd)>/ 并确保存在；同时写入 project.json{cwd, git_root, first_seen, last_seen}。
func ProjectDir(cwd string) (string, error)

// ProjectID 返回记忆作用域用的项目身份：git 主工作区根目录 basename + "-" + sha256(绝对路径)[:8]；非 git 用 cwd。
func ProjectID(cwd string) (string, error)

// GitRoot 返回 cwd 所在 git 主工作区根（git worktree 共享同一主根）；非 git 返回 ""。
func GitRoot(cwd string) string

// UserConfigPath = <Home>/config.yaml ；ProjectConfigPath = <cwd>/.codeclaw/config.yaml
func UserConfigPath() (string, error)
func ProjectConfigPath(cwd string) string
```

`GitRoot` 实现：向上查找 `.git`；若 `.git` 是文件（worktree），解析其中 `gitdir:` 指向的 `…/.git/worktrees/<name>`，取其上两级作为主根。

数据落点：

```
<ProjectDir>/
  project.json
  current                       # 当前会话 id
  memory.db                     # project 作用域记忆（M1：全部记忆都在这里；global 库 M3 引入）
  <ts>_<id>.jsonl               # 会话
  <ts>_<id>/                    # 会话产物目录：artifact 与子 agent sidecar
     0.bash.log
     agent-explorer-1.jsonl
```

---

## 3. 配置（`cmd/agent/config.go`）

三层合并，后者覆盖前者的**非零值**：

1. `~/.codeclaw/config.yaml`（用户）
2. `<cwd>/.codeclaw/config.yaml`（项目）
3. `./config.yaml`（仓库内 legacy；仅当前两者都没有 `models` 时才读取，并打印一次迁移提示）

新增/变更字段：

```yaml
approval_mode: write          # 默认 write（原 yolo）
delegation_mode: preferred
subagent:
  max_concurrency: 4
  approval_escalation: false  # true = headless 子 agent 的 Prompt 决策升级到父弹窗
  default_timeout: 10m
  default_max_turns: 50
```

命令行：`--yolo`（强制 `approval_mode: yolo`）、`-p "<prompt>"`（headless）、`--cwd <dir>`（默认 `os.Getwd()`）。

---

## 4. Session v2（`internal/session`）

### 4.1 Entry

```go
type Entry struct {
    Type      EntryType `json:"type"`
    ID        string    `json:"id,omitempty"`        // 8 hex；header 无
    ParentID  string    `json:"parentId,omitempty"`  // 追加时 = 当前 leaf；reset 后首条为空
    Timestamp string    `json:"ts,omitempty"`        // RFC3339Nano

    // EntrySession（header）
    Version       int    `json:"version,omitempty"`  // 2
    SessionID     string `json:"sessionId,omitempty"`
    CWD           string `json:"cwd,omitempty"`
    Title         string `json:"title,omitempty"`
    ParentSession string `json:"parentSession,omitempty"`
    Model         string `json:"model,omitempty"`

    // EntryMessage
    Message *message.Message `json:"message,omitempty"`
    Usage   model.Usage      `json:"usage,omitzero"`

    // EntryCompaction
    Compaction *Compaction `json:"compaction,omitempty"` // {Summary, FirstKeptEntryID, TokensBefore}

    // EntryInit（子 agent 首条）
    Init *SessionInit `json:"init,omitempty"` // {Agent, SystemPrompt, Task, Tools, OutputSchema, Depth, ParentToolCallID}

    // EntryCustom（非 LLM 状态）
    CustomType string          `json:"customType,omitempty"` // tool_execution_start | session_exit
    Data       json.RawMessage `json:"data,omitempty"`
}
```

### 4.2 语义

- **leaf**：`Session` 持有 `leafID`；`Append*` 生成 id、设 `ParentID = leafID`、更新 leaf；`Reset()` 追加 `reset_boundary` 后 leaf 仍指向它（回放从它之后开始）。
- **Replay**：从 leaf 沿 `ParentID` 回溯到根（遇重复 id 终止），反转后：
  1. 最新 `reset_boundary` 之后才进入模型上下文；
  2. 最新 `compaction` 展开为 `[摘要 user 消息] + 从 FirstKeptEntryID 起的 message 条目`；
  3. `EntryInit`/`EntryCustom`/header 不产生消息；
  4. **修复悬空 tool_call**：assistant 的每个 tool_call 若在后续消息里没有配对的 tool 结果，则在其后合成 `NewToolMessage(id, name, "[interrupted: tool did not run]", true)`（仅回放，不落盘）。
- **兼容 v1**：读到无 `id` 的条目时按文件顺序赋临时 id 并串成线性链（内存中），leaf = 最后一条。v1 的 compaction（无 FirstKeptEntryID，后面跟重追加的保留消息）仍按"替换之前全部"处理。
- **Compact(summary, firstKeptID string, tokensBefore int)**：只追加一条 compaction 条目，不再重追加保留消息。
- **Fork**：复制条目（保留 id 链）到新存储，新 header 带 `ParentSession`。
- **标题**：`SetTitle(title)` 追加 `title_change`；`Info()` 返回 header + 最新标题。

### 4.3 Manager

```go
type Info struct { ID, Title, FirstUser, Path string; ModTime time.Time }
func NewManager(projectDir string) (*Manager, error)
func (m *Manager) Current() (*Session, error)           // current 文件；无则新建
func (m *Manager) New(cwd string) (*Session, error)      // id = <20060102-150405>_<6hex>
func (m *Manager) Switch(id string) (*Session, error)    // 支持 id 前缀匹配（唯一时）
func (m *Manager) List() ([]Info, error)                 // 只读每个文件前 8 行取 header/首条 user/title
func (m *Manager) ArtifactDir(s *Session) string          // <projectDir>/<id>/ 并确保存在
```

---

## 5. 模型层（`internal/model`）

```go
type ToolSpec struct {
    Name, Description string
    Parameters        map[string]any // JSON Schema properties（含嵌套 items/properties/enum/description）
    Required          []string
}

// ModelStream 是一次流式调用；*Stream 实现它；测试可注入 fake。
type ModelStream interface {
    Recv() (ModelEvent, error)
    Usage() Usage
    Close()
}
type Model interface {
    Stream(ctx context.Context, msgs []message.Message, tools []ToolSpec) (ModelStream, error)
}

func IsContextOverflow(err error) bool // 匹配 context_length_exceeded / maximum context length / prompt is too long / too many tokens / input length exceeds
func IsRetryable(err error) bool       // 429 / 5xx / rate limit / overloaded / timeout / connection reset|refused / EOF（非 io.EOF 语义）
```

`toSchemaTools` 改为：把 `{type:object, properties, required}` JSON 编码后反序列化为 `*jsonschema.Schema`，用 `schema.NewParamsOneOfByJSONSchema`。这样 `task` 工具的 `tasks[].{subagent,prompt}` 结构对模型可见。

---

## 6. 上下文管理 v2（`internal/context`）

```go
type Manager struct {
    session    *session.Session
    summarizer summarizer
    window, keepRecent int
    system     func(ctx context.Context) []message.Message // 系统提示 + 记忆块，由装配方注入
}

func New(s *session.Session, sum summarizer, window, keepRecent int, system func(context.Context) []message.Message) *Manager
func (m *Manager) Build(ctx context.Context) ([]message.Message, error)       // system() + session.Replay()
func (m *Manager) Record(msg message.Message, u model.Usage) error            // session.AppendWithUsage
func (m *Manager) ShouldCompact(u model.Usage) bool                           // u.PromptTokens > threshold
func (m *Manager) Compact(ctx context.Context) (bool, error)                  // 正常压缩；返回是否发生
func (m *Manager) RecoverOverflow(ctx context.Context) (bool, error)          // keep 减半后压缩；无可压内容返回 false
```

- `estimateTokens`：text/thinking 按 `runes/2`，tool_call 按 `(len(name)+len(args))/2`，tool_result 按 `runes/2`，每消息 +4。
- `findCutPoint(msgs, keep)`：从新到旧累计到 `keep` 得到候选 `i`，然后 `for i > 0 && !safeCut(msgs, i) { i-- }`。`safeCut(i)` ⇔ `msgs[i].Role == user` 或 `msgs[i].Role == assistant 且无 tool_call 块`。tool 消息永远不安全；带 tool_call 的 assistant 不安全（其结果在后面）。
- `serializeConversation`：assistant 的 tool_call 输出 `assistant→tool_call name(args)`；tool 结果输出 `tool(name): <内容，超过 1500 字符取头 1000 + "…" + 尾 500>`；thinking 不输出。
- `Compact` 返回后，`session` 里只多一条 compaction；调用方重新 `Build()`。
- `TokensBefore` 记录触发时的 PromptTokens（审计用）。

---

## 7. Agent 循环 v2（`internal/agent`）

```go
// Context 是循环的真相源抽象（生产用 context.Manager，测试用 MemoryContext）。
type Context interface {
    Build(ctx context.Context) ([]message.Message, error)
    Record(m message.Message, u model.Usage) error
    ShouldCompact(u model.Usage) bool
    Compact(ctx context.Context) (bool, error)
    RecoverOverflow(ctx context.Context) (bool, error)
}

func New(name string, m model.Model, tools *tool.Registry, exec *tool.Executor, cc Context) *Agent
func (a *Agent) Run(ctx context.Context, steer <-chan message.Message) <-chan AgentEvent
```

循环（每步）：

1. 非阻塞取 steering → `Record(user)`。
2. `msgs = cc.Build()`；若 `cc.ShouldCompact(lastUsage)` → `cc.Compact()` 成功则 emit `EventCompaction` 并重新 `Build()`。
3. `model.Stream`：错误若 `IsContextOverflow` → `cc.RecoverOverflow()`，成功则 emit `EventCompaction` 并 `continue`（不计步）；若 `IsRetryable` 且重试 < 3 → 退避 `500ms·2^n` 后 `continue`；否则 emit `EventError` 并结束。
4. `consumeStream` → `cc.Record(assistant, usage)`；流中途错误同样走第 3 步分流（溢出/重试），但已记录的 assistant 不重复。
5. 无 tool_call → 结束。
6. `ctx.Err() != nil` → 跳过未启动工具，结束。
7. `ExecuteAll` → 每个结果 `Record(toolMsg)` + emit `EventToolEnd`；若某工具实现 `tool.Terminal` 且本次执行无错 → emit `EventTerminated` 并结束。

新增事件：`EventCompaction{Reason: "threshold"|"mid-turn"|"overflow"}`、`EventTerminated{ToolName}`、`EventRetry{Attempt, Delay, Err}`。

`emit` 保留 1s 超时策略；`EventMessageEnd`/`EventToolEnd` 的持久化已在循环内完成，丢事件只影响渲染。

用户输入：调用方（TUI / headless / subagent）在 `Run` 前 `cc.Record(NewUserMessage(text), Usage{})`。

---

## 8. 产物（`internal/runtime`、`internal/tool`）

```go
// ArtifactStore 管理一个会话的产物目录；id 单调递增，首次分配前扫描已有 *.log 的最大 id。
type ArtifactStore struct{ dir string; next int64; once sync.Once; mu sync.Mutex }
func NewArtifactStore(dir string) *ArtifactStore
func (s *ArtifactStore) Create(tool string) (id string, f *os.File, err error)  // <dir>/<id>.<tool>.log
func (s *ArtifactStore) Resolve(ref string) (path string, err error)              // "artifact://3" 或 "3" → 路径
```

- `Sink` 新增 `SetArtifactStore(store *ArtifactStore, tool string)`；截断时用 store 分配；`Result()` 尾部为 `\n[完整输出已保存: artifact://3 ；用 read_file 的 file_path="artifact://3" 读取]`。
- `Executor` 持有 `*ArtifactStore`（可 nil），`Execute` 时注入。
- `read_file`：`file_path` 以 `artifact://` 开头时经 store 解析；`Builtins(bash, store)`。
- `Bash`：`NewBash(cwd)` 的 cwd 为空时用 `os.Getwd()`；`parseCd` 后的相对路径相对当前 cwd 解析为绝对路径。

---

## 9. 子 agent 运行时修正（`internal/subagent`）

```go
type Options struct {
    Model        model.Model
    WorkerTools  func(cwd string, store *runtime.ArtifactStore) *tool.Registry // 每个 Run 独立 Bash
    Memory       memory.Recaller
    Mode         permission.Mode   // 继承父
    Approver     tool.Approver     // 父的审批器
    Escalate     bool              // true 时 Prompt 决策升级到父审批（弹窗带子 agent 标签）
    SessionDir   string            // 父会话产物目录；"" = 不落盘（MemoryStorage）
    CWD          string
    MaxConcurrency int
    Defs         []SubagentSpec
    Summarizer   interface{ Summarize(context.Context, []message.Message) (string, error) }
    ContextWindow int
}
func NewManager(o Options) *Manager
```

- **Run(ctx, Task) Result**：
  1. 解析定义；建 `runtime.NewBash(cwd)` + `ArtifactStore(sessionDir)` + `WorkerTools(...)`；注册 `yield`。
  2. 审批器：`Escalate` → `labeledApprover{inner: parent, label: "[子 agent <name>]"}`（把标签塞进 `ToolCall.Name` 的展示副本）；否则 `denyApprover{}`（返回 false，Executor 文本为 `tool denied: headless subagent cannot prompt for approval`）。
  3. sidecar：`SessionDir != ""` → `session.New(id, FileStorage(<dir>/agent-<name>-<n>.jsonl))` + header `ParentSession` + `EntryInit`；否则 `MemoryStorage`。
  4. `context.New(childSession, summarizer, window, keep, system)`；`Record(user task)`；`agent.New(...)`；`SetMaxIterations`。
  5. 消费事件：`EventToolEnd` 且 `Name == "yield"` 且无错 → 解析 args.data；`EventTerminated` → `yielded = true`；累计 `Requests`（MessageEnd 数）、`Usage`；`EventError` → 记录。
  6. 状态判定（优先级）：父 ctx 取消 → `StatusAborted`；runCtx 超时 → `StatusTimeout`；`runErr != nil` → `StatusFailed`；yielded 且（无 schema 或 data != nil）→ `StatusCompleted`；未 yield 且无 schema → `StatusCompleted`（`Yielded=false`，task 输出标注 `[未显式 yield，以下为最后输出]`）；需要 schema 但无 data → `StatusFailed`。
  7. `Result{ID, Name, Status, Yielded, Data, Text, Err, Usage, Requests, DurationMs, SessionFile}`。
- **yield**：实现 `tool.Terminal`（`IsTerminal() bool { return true }`），循环在其执行后终止。
- **RunMany**：并发上限来自 `Options.MaxConcurrency`；结果按输入序；`Task` 增加 `Name`（缺省 `<subagent>-<序号>`）。
- **task 工具输出**：每个结果一段：`## <name> (<subagent>) [<status>] requests=<n> tokens=<n> <dur>`，随后 data JSON 或 text；失败附 `error:`；`SessionFile` 路径一行（便于人工查看）。

---

## 10. TUI 与 headless

- `runAgent`：`cmgr.Record(userMsg)` → `agent.Run(ctx, steer)`；不再自己 `Append`。`EventCompaction` 渲染一行 `── 上下文已压缩（<reason>）──`；`EventRetry` 渲染 `重试 n/3 …`。
- `/sessions` 显示 `id · 标题/首句 · 时间`；`/resume` 支持 id 前缀。
- 审批弹窗：`approvalRequestMsg` 增加 `label`（子 agent 升级审批时非空），渲染 `⚠ 审批 [子 agent explorer]`。
- headless：`codeclaw -p "prompt"`：不起 TUI；用同一装配；把事件打印到 stdout（`▶ tool name args` / `◀ 结果前 3 行` / `[compaction]` / 最终回复正文）；退出码 0/1；`--yolo` 常用于 CI。

---

## 11. 迁移与兼容

- 旧 `./sessions/*.jsonl`：不自动迁移；启动时若检测到旧目录存在且新项目桶为空，打印一行提示（`旧会话位于 ./sessions，可手动复制到 <ProjectDir>`）。
- 旧 `./memory.db`：同上提示。
- 旧 `./config.yaml`：作为第 3 层继续可用。

---

## 12. 测试策略

- **纯函数**：`paths.EncodeCWD`、`GitRoot`（临时 git 目录）、配置合并、`estimateTokens`、`findCutPoint`（含"切点落在 tool 消息前的 assistant"场景）、`serializeConversation`、`IsContextOverflow`、`ArtifactStore.Create/Resolve`。
- **Session**：v2 追加链/leaf/Replay；compaction + FirstKeptEntryID；悬空 tool_call 修复；v1 文件兼容；Manager.List 标题。
- **Agent 循环**：`fakeModel`（脚本化：每步返回预设事件/错误）+ `MemoryContext`：① 工具循环记录顺序正确；② 终止型工具终止；③ 溢出错误触发 `RecoverOverflow` 后继续；④ 可重试错误重试后成功；⑤ 取消时跳过工具。
- **Subagent**：fakeModel 驱动：yield 终止并提取 data；超时 → `timeout`；deny approver 下 bash 被拒；escalate 下父 approver 收到带标签的调用；sidecar 文件存在且首条为 `session_init`。
- **Executor/Sink**：截断落盘 + `artifact://` 读回。
