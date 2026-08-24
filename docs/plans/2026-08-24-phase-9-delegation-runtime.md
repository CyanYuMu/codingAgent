# Phase 9 委派运行时 Implementation Plan

> **For agentic workers:** 按任务顺序实施，每个任务先写失败测试再实现（步骤用 `- [ ]` 复选框跟踪）。任务之间有依赖，不要跳序。

**Goal:** 让派发从"一次同步函数调用"变成一组**有契约、可观察、可干预、寿命受约束**的执行单元：yield 三态 + schema 校验重试保证完成度，idle 提醒与软预算保证寿命，EventBus + Agent Hub 保证可观察，后台作业 + async-result + hub 邮箱保证不阻塞与可协作，frontmatter 发现保证可定制。

**Architecture:** `subagent.Manager` 持有 **Run 名册**、**可 resize 并发闸**、**根 ctx**、**作业表**、**邮箱**。每个 Run 由 `driver` 驱动一个 **turn 阶梯**：每 turn 一个可单独取消的 `turnCtx`，turn 结束若无 terminal yield 则注入提醒（≤3 次，最后一次工具集只剩 `yield`）；软预算越界先注入收尾通知、1.5× 停当前 turn 强制 yield、宽限 5 次请求后硬杀。yield 工具三态（terminal / incremental / error）+ 工具内 schema 校验重试；终止与否由**本次调用**决定（`tool.Result.Terminal`）。事件经 `internal/bus` 三通道流向 TUI（父 agent 不消费）。后台作业挂 Manager 根 ctx，结算后按 async-result **恰好一次**投递回父会话。

**Tech Stack:** Go 1.26；`gopkg.in/yaml.v3`（frontmatter）；`embed`（内置 agent 定义）；BubbleTea v2（Hub 面板）；不新增外部依赖（JSON Schema 校验自己写最小实现）。

**Spec:** `docs/specs/phase-9-delegation-runtime.md`

## Global Constraints

- 只有 `internal/model` 可以 import eino / eino-contrib。
- 每个任务结束：`env -u GOROOT go build ./... && env -u GOROOT go vet ./... && env -u GOROOT go test ./...` 通过。
- 新代码注释写"为什么"，不写"是什么"；中文注释，风格与现有代码一致。
- 提交信息用中文前缀 `feat/fix/refactor/test:`，每个任务至少一次提交；子阶段（P9.1–P9.4）结束打一次完整测试。
- 子 agent 的原始事件**不进父上下文**：父只拿结果渲染 + 指针；bus 只有 TUI/Manager 消费。
- 后台 Run 一律挂 `Manager.root`，不挂工具调用的 ctx。
- 所有对 Run 状态的读写走 `Run.mu`；对外只暴露快照（`View()`）。
- 不得在 `internal/subagent` 里 import `internal/tui`（依赖方向：tui → subagent）。

---

## File Structure

| 文件 | 职责 |
|---|---|
| `internal/bus/bus.go`（新） | 极简发布/订阅：`Publish` 非阻塞、`Subscribe` 返回通道 + 幂等取消 |
| `internal/bus/bus_test.go`（新） | 多订阅者 / 缓冲满丢弃 / 取消后不 panic |
| `internal/paths/paths.go` | `UserAgentsDir()`、`ProjectAgentsDir(cwd)` |
| `internal/subagent/spec.go` | `AgentDef`、`TaskItem`、`TaskBatch`、`Status`（+idle/budget_stop/parked）、`Result` 扩展、`RunView` |
| `internal/subagent/discovery.go`（新） | `ParseAgentFile` / `Discover`（project → user → bundled，first-wins） |
| `internal/subagent/agents/*.md`（新） | 内置 explorer / reviewer / planner / worker 定义（`go:embed`） |
| `internal/subagent/preflight.go`（新） | `Env`、`Resolved`、`Preflight`（纯函数） |
| `internal/subagent/schema.go`（新） | `Validate` / `deriveDataSchema` / `sectionSchema` / `closedSchema` |
| `internal/subagent/yield.go` | yield 三态 + `YieldState` + 派生参数 + 校验重试 |
| `internal/subagent/driver.go`（新） | `Run` 结构、turn 阶梯、软预算监视、状态判定、产出落盘 |
| `internal/subagent/manager.go` | 名册 / 并发闸 / 根 ctx / `RunBatch` / `StartBackground` / 作业表 / `Revive` / `Shutdown` / URL 解析 |
| `internal/subagent/mailbox.go`（新） | 邮箱：投递 / 唤醒 / 清空 / 未读 |
| `internal/subagent/hub.go`（新） | `hub` 工具（list/send/inbox/wait/jobs/cancel） |
| `internal/subagent/task.go` | `{context, tasks[], background}` 参数、动态枚举、结果渲染、旧字段兼容 |
| `internal/subagent/*_test.go` | 按任务新增/更新 |
| `internal/tool/tool.go` | `Terminal.IsTerminal(args, err)` |
| `internal/tool/executor.go` | `Result.Terminal` |
| `internal/agent/loop.go` | 终止判定读 `Result.Terminal` |
| `internal/runtime/artifact.go` | `AddScheme` + `Resolve` 分派 |
| `internal/tool/tools.go` | `read_file` 支持任意 `<scheme>://` |
| `internal/tui/tui.go`、`internal/tui/hub.go`（新） | Agent Hub 面板、`/agent`、后台作业行、bus 桥 |
| `cmd/agent/main.go` | 装配 bus / 发现 / 名册 / scheme / hub 工具 |
| `cmd/agent/headless.go` | 后台作业结算 + auto-continue（≤3 轮） |
| `cmd/agent/config.go` | `subagent` 段扩展（soft_budget / max_recursion_depth / min_task_chars / background / agents_dir） |
| `example.yaml`、`docs/DEVELOPMENT_LOG.md` | 示例与记录 |

---

# P9.1 契约与发现

### Task 1: `internal/bus` 事件总线

**Files:** Create `internal/bus/bus.go`、`internal/bus/bus_test.go`

**Interfaces:**
- Produces: `bus.Envelope{Channel string; Payload any; At time.Time}`、`bus.New() *Bus`、`(*Bus).Publish(channel string, payload any)`、`(*Bus).Subscribe(channel string, buf int) (<-chan Envelope, func())`

- [x] **Step 1: 写失败测试**（`bus_test.go`）
  - `TestPublishFanOut`：两个订阅者各收到同一条。
  - `TestPublishNonBlockingWhenFull`：`Subscribe(ch, 1)` 后连发 100 条，`Publish` 不阻塞（用 `time.After(100ms)` 断言整体耗时），通道里能取到第一条。
  - `TestUnsubscribeIdempotentAndSafe`：取消两次不 panic；取消后 `Publish` 不 panic，通道已关闭（`_, ok := <-ch; ok == false`）。
  - `TestOtherChannelNotDelivered`：订阅 A，发 B，读不到。

- [x] **Step 2: 实现**
  ```go
  type Bus struct {
      mu   sync.RWMutex
      subs map[string]map[int]chan Envelope
      seq  int
  }
  ```
  - `Publish`：`RLock` → 遍历该 channel 的订阅者 → `select { case ch <- env: default: }`（满则丢，渲染事件可丢）。
  - `Subscribe`：`Lock` → 分配 `id = seq++` → 建缓冲通道 → 返回 `cancel`：`sync.Once` 包住 `Lock` + `delete` + `close(ch)`。
  - 不变量注释：`Publish` 持 RLock 期间不可能有人 `close`（取消要写锁），所以不会向已关闭通道发送。

- [x] **Step 3: 验证** `go test ./internal/bus/ -race`

---

### Task 2: `AgentDef` / `TaskBatch` 类型与 agent 目录

**Files:** Modify `internal/subagent/spec.go`、`internal/paths/paths.go`；Test `internal/paths/paths_test.go`

**Interfaces:**
- Produces: `subagent.AgentDef`、`subagent.TaskItem`、`subagent.TaskBatch`、`subagent.Status`（新增 `StatusIdle`/`StatusBudgetStop`/`StatusParked`）、`subagent.Result`（扩展）、`paths.UserAgentsDir()`、`paths.ProjectAgentsDir(cwd)`

- [x] **Step 1: 写失败测试**
  - `paths_test.go`：`TestAgentsDirs`——`CODECLAW_HOME` 覆盖下 `UserAgentsDir()` = `<home>/agents`；`ProjectAgentsDir("/x/y")` = `/x/y/.codeclaw/agents`。
  - `spec_test.go`（新）：`TestStatusString` 覆盖全部状态（含新增三个）。

- [x] **Step 2: 实现**
  - `AgentDef` 按 spec §2.1 全字段；删除 `SubagentSpec`（全局改名，注意 `cmd/agent/main.go`、`manager.go`、测试）。
  - `TaskItem`/`TaskBatch` 按 spec §3.1；保留 `Task`→`TaskItem` 改名。
  - `Result` 增 `Agent`、`Sections`、`Warning`、`ToolCalls`、`Reminders`、`BudgetStopped`、`OutputFile`；`Data` 类型从 `map[string]any` 改为 `any`（yield 允许数组/标量产出）。
  - `RunView`：`{ID, Name, Agent, Status, CurrentTool string; Depth, Requests, ToolCalls, Tokens, ContextTokens, Unread, Revives int; Age time.Duration; SessionFile, OutputFile string}`。
  - `paths`：两个目录函数（不创建目录，缺失按空处理）。

- [x] **Step 3: 验证** 编译通过（`renderResult` 里 `Data` 的类型断言要跟着改），`go test ./internal/paths/ ./internal/subagent/`

---

### Task 3: frontmatter 发现 + 内置 agent 嵌入

**Files:** Create `internal/subagent/discovery.go`、`internal/subagent/discovery_test.go`、`internal/subagent/agents/{explorer,reviewer,planner,worker}.md`；Modify `cmd/agent/main.go`（删 `builtinDefs`）

**Interfaces:**
- Produces: `subagent.ParseAgentFile(path string, data []byte, source string) (AgentDef, error)`、`subagent.Discover(projectDir, userDir string, bundled []AgentDef) DiscoverResult`、`subagent.Bundled() []AgentDef`
- Consumes: `paths.UserAgentsDir` / `paths.ProjectAgentsDir`

- [x] **Step 1: 写失败测试**
  - `TestParseFullFrontmatter`：全字段（CSV `tools`、数组 `spawns`、`output` 嵌套 schema、`timeout: 10m`、`read_only: true`）解析正确，`SystemPrompt` = 正文（trim）。
  - `TestParseMissingNameOrBody`：缺 `name`、缺 `description`、正文空 → error。
  - `TestParseBadYAMLAndBadDuration`：坏 YAML → error；`timeout: 十分钟` → 不报错但 `Timeout == 0`（用默认值），且返回的 error 为 nil（宽容字段单独降级）。
  - `TestDiscoverPrecedence`：临时目录里 project 与 user 各放一个 `reviewer.md`，bundled 里也有 → 取 project；`Warns` 收集坏文件；同目录多文件按字典序。
  - `TestBundledParses`：`Bundled()` 四个都能解析且都有非空 `SystemPrompt`，`explorer`/`reviewer`/`planner` 有 `OutputSchema`。

- [x] **Step 2: 实现**
  - `splitFrontmatter(data []byte) (fm []byte, body string, ok bool)`：要求首行是 `---`，找下一行 `---`。
  - `frontmatter` 结构体用 `yaml` tag：`name/description/when_to_use/tools/spawns/model/output/schema_mode/max_turns/soft_budget/timeout/read_only/blocking`；`tools`/`spawns` 用 `stringList` 自定义 `UnmarshalYAML`（接受标量 CSV 与序列）。
  - `Discover`：按目录顺序（project、user）`os.ReadDir` → 只取 `.md` → 字典序 → 解析 → `seen[name]` first-wins；最后追加 bundled 里未出现的名字。目录不存在按空。
  - 内置 markdown 四份，写清 outputSchema 与"完成协议"提示（原 `main.go` 里的 prompt 迁进来，补充 Target/Change/Acceptance 语言）。
  - `//go:embed agents/*.md` + 解析缓存（`sync.Once`）；解析失败直接 `panic`（内置定义写错必须在启动时暴露）。
  - `main.go`：`defs := subagent.Discover(paths.ProjectAgentsDir(cwd), userAgentsDir, subagent.Bundled())`，`Warns` 用 `log.Printf` 打印。

- [x] **Step 3: 验证** `go test ./internal/subagent/ -run 'Parse|Discover|Bundled'`

---

### Task 4: 预检（`Preflight`）

**Files:** Create `internal/subagent/preflight.go`、`internal/subagent/preflight_test.go`

**Interfaces:**
- Produces: `subagent.Env`、`subagent.Resolved`、`subagent.Preflight(b TaskBatch, env Env) ([]Resolved, error)`

- [x] **Step 1: 写失败测试**（表驱动，每条断言 error 文本含关键提示词）
  - 空 `tasks` / 空 `context` / `task` < `MinTaskChars` / 未知 agent（错误里列出可用名） / `Depth >= MaxDepth` / `Spawns` 不含目标 / `agent == SelfAgent`。
  - `TestPreflightNamingAndDedup`：两项同 agent 且都没给 name → `worker-1`/`worker-2`；显式重名 → 第二个变 `X-2`；name 里的空格/斜杠被 sanitize。
  - `TestPreflightSchemaPrecedence`：item 的 `OutputSchema` 覆盖 def；`SchemaMode` 缺省 `permissive`，item 覆盖 def。

- [x] **Step 2: 实现**
  - 按 spec §3.2 的顺序检查；错误文本面向模型（说"怎么改"，不只说"错了"）。
  - `SeqNext` 缺省用 Manager 的 `atomic.Int64`；测试里注入确定性实现。
  - `sanitizeName` 复用 `manager.go` 里已有的实现（移到 preflight.go 并加导出注释）。

- [x] **Step 3: 验证** `go test ./internal/subagent/ -run Preflight`

---

### Task 5: Manager 接入预检 + 工具集/深度/只读

**Files:** Modify `internal/subagent/manager.go`、`internal/subagent/task.go`；Test `internal/subagent/manager_test.go`

**Interfaces:**
- Produces: `(*Manager).RunBatch(ctx, TaskBatch, Env) ([]Result, error)`、`(*Manager).Env(depth int, self string, spawns []string) Env`、`subagent.NewTaskTool(mgr *Manager, depth int, self string, spawns []string) tool.Tool`
- Consumes: `Preflight`、`Discover` 的结果（`Options.Defs`）

- [x] **Step 1: 写失败测试**
  - `TestToolSetReadOnly`：`read_only: true` 的 def → 子 agent 拿到的工具名集合 == `{read_file, glob, grep, yield}`（hub 在 Task 14 加）。断言方式：给 `scriptModel` 记录它收到的 `[]model.ToolSpec`，取最后一次。
  - `TestToolSetSpawnsAndDepth`：`spawns: [worker]` 且 `depth+1 < maxDepth` → 工具集含 `task`；`depth+1 == maxDepth` → 不含。
  - `TestRunBatchRejectsThinPrompt`：一行 prompt 的批次 → `RunBatch` 返回 error，且**没有**任何 sidecar 文件生成（预检先于子进程）。
  - `TestTaskToolLegacyArgs`：`{"tasks":[{"subagent":"explorer","prompt":"<足够长的描述>"}]}` 仍能跑（兼容映射），结果文本里提示新字段名。

- [x] **Step 2: 实现**
  - `Options` 增 `MaxDepth`、`MinTaskChars`、`SoftBudget`、`Bus *bus.Bus`、`AllowBackground bool`。
  - `Manager.RunBatch`：`Preflight` → 为每项建 Run → 并发闸 → `drive`（Task 9 之前先保留现有 `Run` 逻辑，本任务只接线，测试用现有行为）。
  - 工具集构造抽成 `func (m *Manager) buildTools(def AgentDef, depth int, store *runtime.ArtifactStore, ys *YieldState) (*tool.Registry, *tool.Registry)`（第二个返回值 = 只含 yield 的注册表，Task 9 用）。
  - `task.go`：新参数 schema（`context` 必填、`tasks[].{name,agent,task,output_schema,schema_mode,effort}`、`background`）；旧字段 `subagent`/`prompt` 兼容映射；描述里动态枚举 agent（带 `[READ-ONLY]`/`[BLOCKING]`/`[schema]` 标记）+ 三句硬约束。
  - `background` 参数在 Task 12 之前：`AllowBackground == false` 或未实现时忽略并在结果里说明"已同步执行"。

- [x] **Step 3: 验证** `go test ./internal/subagent/`（旧测试按新 API 更新：`Task{Subagent,Prompt}` → `TaskBatch{Context, Tasks:[]TaskItem{{Agent,Task}}}`）

- [x] **Step 4: 提交** `feat: P9.1 契约与发现（bus / frontmatter agent / TaskBatch 预检 / 工具集与深度）`

---

# P9.2 完成度：yield 三态 · 阶梯 · 软预算

### Task 6: 终止判定改为按调用

**Files:** Modify `internal/tool/tool.go`、`internal/tool/executor.go`、`internal/agent/loop.go`、`internal/subagent/yield.go`；Test `internal/tool/executor_test.go`、`internal/agent/loop_test.go`

**Interfaces:**
- Produces: `tool.Terminal interface{ IsTerminal(args map[string]any, err error) bool }`、`tool.Result{Content string; IsError bool; Terminal bool}`

- [x] **Step 1: 写失败测试**
  - `executor_test.go`：`TestResultTerminalFromTool`——假工具 `IsTerminal(args, err)` 在 `args["stop"]==true && err==nil` 时为真 → `Result.Terminal` 相应为 true/false；执行出错时 `Terminal == false`。
  - `loop_test.go`：`TestLoopStopsOnTerminalResult`——终止型工具返回 `Terminal:true` → 循环结束且发 `EventTerminated`；同一 assistant 消息里终止工具在前、普通工具在后 → 两个工具都执行（结果都记录），然后终止。

- [x] **Step 2: 实现**
  - `Executor.Execute`：解析 args 后执行，然后 `if t, ok := t.(Terminal); ok { res.Terminal = t.IsTerminal(args, err) }`。
  - `loop.go`：删掉注册表查找，改 `if results[i].Terminal { terminated = tc.Name }`；保留"先记录所有 tool 结果再终止"的顺序。
  - `yield.go`：`IsTerminal(args, err) = err == nil && strings.TrimSpace(str(args["section"])) == ""`（增量提交不终止；工具内退回重试时 err != nil 也不终止）。

- [x] **Step 3: 验证** `go test ./internal/tool/ ./internal/agent/ ./internal/subagent/`

---

### Task 7: 最小 JSON Schema 校验器与派生

**Files:** Create `internal/subagent/schema.go`、`internal/subagent/schema_test.go`

**Interfaces:**
- Produces: `Validate(schema map[string]any, value any) []string`、`deriveDataSchema(map[string]any) map[string]any`、`sectionSchema(schema map[string]any, label string) (map[string]any, bool)`、`closedSchema(map[string]any) bool`

- [x] **Step 1: 写失败测试**
  - `TestValidateObjectRequired`：缺字段 → issue 文本含字段名与路径（`findings[0].file`）。
  - `TestValidateTypes`：string/number/integer/boolean/array/object/null 的正反例；`integer` 接受 `float64(3)` 拒绝 `3.5`（JSON 解出来都是 float64）。
  - `TestValidateEnum`、`TestValidateNestedArrayItems`。
  - `TestValidateNilSchema`：schema 为 nil → 无 issue。
  - `TestDeriveDataSchemaStripsRequired`：递归删掉所有 `required`，保留 `type/properties/items/enum/description`，对象层加 `additionalProperties:true`；原 schema **不被修改**（深拷贝）。
  - `TestSectionSchemaArrayItems`：`findings` 是数组 → 返回其 `items`；`verdict` 是标量 → 返回该属性 schema；未知名 → `false`。
  - `TestClosedSchema`：有 `properties` 且无 `additionalProperties:true` → true。

- [x] **Step 2: 实现**
  - `Validate` 递归，路径用 `$`/`.field`/`[i]` 拼接；issue 上限 20 条（多了截断并附 `…`）。
  - 不支持的关键字（`$ref`/`oneOf`/`anyOf`/`allOf`/`pattern`/`format`/min/max）**忽略**，文件头注释声明限制。
  - `deriveDataSchema` 深拷贝（`map[string]any`/`[]any` 递归）。

- [x] **Step 3: 验证** `go test ./internal/subagent/ -run 'Validate|Derive|Section|Closed'`

---

### Task 8: yield 三态

**Files:** Modify `internal/subagent/yield.go`；Test `internal/subagent/yield_test.go`（新）

**Interfaces:**
- Produces: `YieldState`（`Data any`、`Sections map[string][]any`、`Error string`、`SchemaOverridden bool`、`SchemaViolation bool`、`Issues []string`）、`NewYieldTool(st *YieldState, schema map[string]any, mode string) tool.Tool`

- [x] **Step 1: 写失败测试**（直接调 `Execute`，用 `runtime.NewSink` 取文本）
  - `TestYieldTerminalSuccess`：`{"data":{...}}` → 无 error、`st.Data` 设好、`IsTerminal` 为 true、sink 文本"结果已提交"。
  - `TestYieldIncrementalAccumulates`：两次 `{"section":"findings","data":{...}}` → `st.Sections["findings"]` 长度 2，`IsTerminal` false，文本提示"继续工作…最后不带 section 再调一次"。
  - `TestYieldErrorState`：`{"error":"卡在 X"}` → `st.Error` 设好、terminal。
  - `TestYieldDataAndErrorRejected`、`TestYieldSectionWithoutData`：返回 error（不 terminal）。
  - `TestYieldEmptyRetriesThenAbort`：连续 4 次 `{}` → 前 3 次 error（文本含"剩余"次数），第 4 次不报错但 `st.Error` 非空且 terminal。
  - `TestYieldSchemaRetryThenPass`：schema 要求 `{findings,verdict}`；第 1 次给错 → error 含 issue 与"剩余 2 次"；第 2 次给对 → 通过，`SchemaOverridden == false`。
  - `TestYieldSchemaOverridePermissive` / `TestYieldSchemaViolationStrict`：错 4 次 → permissive 放行 `SchemaOverridden=true`；strict `SchemaViolation=true`（两者都 terminal）。
  - `TestYieldAssembleFromSections`：先两段 `findings` + 一段 `verdict`，再 `{}` → `st.Data` 是装配出的对象（`findings` 为数组、`verdict` 为最后值）并通过校验。
  - `TestYieldUnknownSectionOnClosedSchema`：封闭 schema 下未知 section → error 列出可用名。
  - `TestYieldParametersDerived`：`Parameters()["data"]` 里没有 `required`，`Description()` 含 schema 关键字段名。

- [x] **Step 2: 实现**（按 spec §4.2 的流程；注意 `YieldState` 全部方法加锁）
  - `Parameters()`：`{"data": deriveDataSchema(schema), "error": {type:string}, "section": {type:string, enum: <已知分段名，若封闭>}}`；`Required()` 返回 `nil`（三者都可选，语义在 description 与工具内校验）。
  - `Concurrency()` 返回 `ConcurrencyExclusive`（同一消息里的多次 yield 串行，计数器语义才确定）。
  - `Description()`：动态生成，含 `data`/`error`/`section` 三态说明 + schema（>2KB 只留顶层字段名）。

- [x] **Step 3: 验证** `go test ./internal/subagent/ -run Yield -race`

---

### Task 9: 驱动器：turn 阶梯 + 软预算 + 状态机

**Files:** Create `internal/subagent/driver.go`；Modify `internal/subagent/manager.go`；Test `internal/subagent/driver_test.go`（新）

**Interfaces:**
- Produces: `Run`（内部）、`(*Run).View() RunView`、`(*Manager).drive(runCtx, *Run, *runtimeSet) Result`
- Consumes: `agent.New`/`Run`、`YieldState`、`bus`

- [x] **Step 1: 写失败测试**（`scriptModel` 扩展：记录每次调用收到的工具名集合；支持"第 N 次调用返回 X"）
  - `TestLadderRemindsThreeTimes`：模型永远只回文本 → 恰好 3 条提醒被记进 sidecar（grep `[提醒`），`Result.Reminders == 3`。
  - `TestForcedTurnOnlyHasYield`：第 3 次提醒那一 turn，模型收到的工具集只有 `yield`。
  - `TestLadderExhaustedNoSchema` / `WithSchema`：无 schema → `completed` + `Yielded=false` + `Warning` 含"未 yield"；有 schema → `failed`。
  - `TestSoftBudgetNoticeThenForcedYield`：`SoftBudget=4`；模型一直调 `glob`；第 4 次请求后 sidecar 出现 `[预算提醒]`；到第 6 次（1.5×）当前 turn 被停，下一 turn 工具集只有 yield 且 sidecar 有 `[强制收尾]`；此时模型调 yield → `completed`、`BudgetStopped == true`。
  - `TestBudgetGraceExhaustedKills`：强制后仍不 yield → 状态 `killed`，`Text` 保留最后文本。
  - `TestYieldErrorMakesFailed`：`yield(error=…)` → `failed` 且 `Err` 文本 = 该 error。
  - `TestTimeoutAndAbortStillPark`：超时/父取消 → 状态正确、`SessionFile` 非空、名册里变 `parked`。
  - `TestPanicRecovered`：注入 panic 的假工具 → `failed` 且不炸测试进程。
  - `TestProgressPublished`：订阅 `ChProgress` 能看到 `CurrentTool` 变化与 `Requests` 增长；订阅 `ChLifecycle` 能看到 running→completed→parked。

- [x] **Step 2: 实现**
  ```go
  type Run struct {
      mu sync.Mutex
      ID, Name, Agent string
      Depth int
      def  AgentDef
      spawn spawnSpec            // revive 需要：task 文本 / context / parentCallID / schema / mode
      status Status
      currentTool string
      requests, toolCalls, reminders, revives int
      usage model.Usage
      contextTokens int
      budgetStop, killed bool
      lastText string
      steer chan message.Message // 缓冲 8：预算通知 / hub 消息 / 父 steering
      cancel, turnCancel context.CancelFunc
      sessionFile, outputFile string
      startedAt, settledAt time.Time
      ys *YieldState
      inbox *mailbox
  }
  ```
  - `drive` 主循环按 spec §5.1；`consume(events)` 内做：requests/toolCalls/usage 记账、`currentTool` 更新、bus 发布（progress 100ms 合并，状态变化必发）、预算判定（notice / stop / kill）。
  - **必须把事件通道读到关闭**再进入下一 turn；`turnCancel()` 后继续 range 直到 close。
  - 提醒文本按 spec §5.2 的三种模板；提醒作为 user 消息 `cc.Record`（进 sidecar，可审计）。
  - 强制 turn：`agent.New(name, model, yieldOnlyReg, yieldOnlyExec, cc)`；`SetMaxIterations(2)`（只需要一次工具调用）。
  - 结算：状态判定（spec §5.3 优先级）→ 写 `<sessionDir>/<Name>.md` → `session_exit` custom 条目（含 status/requests/reminders/budgetStop/schema 状态）→ `ChLifecycle(parked)`。
  - `RunMany` 删除，全部走 `RunBatch`（同步）与 `StartBackground`（Task 12）。

- [x] **Step 3: 验证** `go test ./internal/subagent/ -race`

- [x] **Step 4: 提交** `feat: P9.2 完成度（yield 三态 + schema 校验重试 + idle 提醒阶梯 + 软预算状态机）`

---

# P9.3 可观测：名册 · 产出 URL · Agent Hub

### Task 10: 名册、产出落盘与 `agent://` / `history://`

**Files:** Modify `internal/runtime/artifact.go`、`internal/tool/tools.go`、`internal/subagent/manager.go`；Test `internal/runtime/artifact_test.go`（若无则新建）、`internal/subagent/manager_test.go`

**Interfaces:**
- Produces: `(*ArtifactStore).AddScheme(scheme string, resolve func(rest string) (string, error))`、`(*Manager).Roster() []RunView`、`(*Manager).ResolveAgentURL(name string) (string, error)`、`(*Manager).ResolveHistoryURL(name string) (string, error)`

- [x] **Step 1: 写失败测试**
  - `TestAddSchemeDispatch`：注册 `agent` → `Resolve("agent://X")` 走自定义解析；`Resolve("artifact://1")` 仍走原逻辑；未知 scheme → 错误里列出已注册 scheme。
  - `TestReadFileViaAgentScheme`：Run 结束后 `read_file file_path="agent://Reviewer"` 能读到 data JSON。
  - `TestRosterContainsParked`：Run 结束后 `Roster()` 里仍有该行且 `Status == "parked"`，`OutputFile`/`SessionFile` 非空。
  - `TestResolveHistoryFallbackToGlob`：名册里没有（模拟 resume）时按 `<sessionDir>/agent-<Name>-*.jsonl` glob 兜底。

- [x] **Step 2: 实现**
  - `ArtifactStore`：`schemes map[string]func(string)(string,error)` + `Resolve` 先按 `<scheme>://` 前缀分派，无 scheme 前缀时按 artifact id（兼容 M1）。
  - `read_file`：`strings.Contains(path, "://")` → 交 store 解析（store 为 nil 时报清晰错误）。
  - `Manager`：`runs map[string]*Run`（key = Name）+ `mu`；`Roster()` 返回按启动时间排序的快照。
  - 产出文件写法：`<sessionDir>/<sanitizeName(Name)>.md`，内容 = 元信息头（agent/status/requests/tokens/duration）+ `## data`（JSON）+ `## sections` + `## last text`。
  - `cmd/agent/main.go`：`store.AddScheme("agent", mgr.ResolveAgentURL)`、`store.AddScheme("history", mgr.ResolveHistoryURL)`。

- [x] **Step 3: 验证** `go test ./internal/runtime/ ./internal/tool/ ./internal/subagent/`

---

### Task 11: TUI Agent Hub 面板与 `/agent`

**Files:** Create `internal/tui/hub.go`；Modify `internal/tui/tui.go`、`cmd/agent/main.go`

**Interfaces:**
- Produces: `tui.NewModel(ag, mgr, cmgr, mem, cwd string, sub *subagent.Manager, b *bus.Bus)`（签名扩展）、`hubTickMsg`、`renderHub(rows []subagent.RunView, sel, width int) string`
- Consumes: `bus.Subscribe`、`subagent.Manager.Roster/Cancel/Send`

- [x] **Step 1: 写失败测试**（TUI 主体不好测，只测纯渲染与选择逻辑）
  - `hub_test.go`：`TestRenderHubRows`——给定 3 行（running/parked/failed）渲染出 3 行文本，含 Name、status、`req=`、当前工具；选中行有标记。
  - `TestHubSelectionClamp`：`j`/`k` 越界不 panic，选择被夹在 `[0, len-1]`。
  - `TestParseAgentCommand`：`/agent Reviewer 再核对 X` → `("Reviewer", "再核对 X", true)`；`/agent` 无参数 → `ok == false`。

- [x] **Step 2: 实现**
  - `teaModel` 增 `hubOpen bool`、`hubSel int`、`sub *subagent.Manager`、`bus *bus.Bus`。
  - `ctrl+a` 切换面板；面板打开时 `j/k/x/esc` 优先（审批弹窗仍最高优先级）。
  - `View()`：`hubOpen` 时把 chat 区下半部分换成 Hub 面板（表头 = 聚合：running N / parked M / 总 tokens）。
  - bus 桥：`NewModel` 里起 goroutine 订阅 `ChLifecycle`/`ChProgress`/`ChJob`，收到就 `program.Send(hubTickMsg{})`（100ms 节流，避免刷屏）；`hubTickMsg` 在 `Update` 里只触发重绘。
  - `/agent <Name> <文本>`：调 `sub.Send("Main", name, text)`（Task 14 实现；本任务先接 `Roster` 判断存在性，未实现时提示"待 P9.4"）。
  - `x`：对选中行调 `sub.Cancel([]string{id})`（Task 12 之前先调 Run 的 cancel）。

- [x] **Step 3: 验证** `go test ./internal/tui/`；真实模型 headless 冒烟已验证名册/`agent://`/`history://`（`- [ ]` 交互式 TUI 面板开合待人工确认）

- [x] **Step 4: 提交** `feat: P9.3 可观测（Run 名册 + agent://history:// + TUI Agent Hub）`

---

# P9.4 异步与通信

### Task 12: 后台作业

**Files:** Modify `internal/subagent/manager.go`；Create `internal/subagent/jobs.go`、`internal/subagent/jobs_test.go`

**Interfaces:**
- Produces: `JobInfo`、`JobResult`、`(*Manager).StartBackground(b TaskBatch, env Env) ([]JobInfo, error)`、`(*Manager).Jobs() []JobInfo`、`(*Manager).Cancel(ids []string) int`、`(*Manager).TakeSettled() []JobResult`、`(*Manager).Pending() int`、`(*Manager).Shutdown(grace time.Duration)`、`(*Manager).SetConcurrency(n int)`

- [ ] **Step 1: 写失败测试**
  - `TestStartBackgroundReturnsImmediately`：慢模型（delay 300ms）+ `StartBackground` → 调用在 50ms 内返回，`Pending() == 1`。
  - `TestSettledDeliveredExactlyOnce`：等作业结束 → `TakeSettled()` 返回 1 条，再调返回 0 条。
  - `TestJobsSnapshotConsumesDelivery`：结束后先调 `Jobs()`（含结果摘要）→ `TakeSettled()` 返回 0 条。
  - `TestCancelStopsRun`：`Cancel([id])` → 状态 `aborted`，`Pending()` 归零。
  - `TestBlockingAgentInlineInBackgroundBatch`：批次里一个 `blocking: true` 的 agent + 一个普通 → 返回时 blocking 那项已有结果，另一项是 running job。
  - `TestShutdownWaitsForBackground`：`Shutdown(1s)` 后所有 Run 退出（`Pending() == 0`），且 sidecar 里有 `session_exit`。
  - `TestSetConcurrency`：并发闸从 1 调到 3 后三个 Run 能同时 running（用 barrier 假工具断言）。

- [ ] **Step 2: 实现**
  - `Manager` 增 `root context.Context` + `rootCancel`（`NewManager` 里建）、`wg sync.WaitGroup`、`jobs map[string]*jobEntry`、`settled []JobResult`（未投递集合）。
  - `StartBackground`：预检 → 为每项建 Run → `go m.driveJob(...)`（挂 `m.root`）→ 立刻返回 `JobInfo`。
  - 结算：写 `settled` → `bus.Publish(ChJob, JobSettled{...})`。
  - `Jobs()`/`TakeSettled()` 共用 `settled` 集合（`Jobs()` 把已结束行的结果一并取走）。
  - `SetConcurrency`：可 resize 闸——用 `chan struct{}` + 目标容量计数的实现（`acquire` 时对比当前容量；缩容只影响后续 acquire，不打断在跑的）。
  - `Shutdown`：`rootCancel()` → `wg.Wait()` 带 `grace` 超时（超时则打印告警返回）。

- [ ] **Step 3: 验证** `go test ./internal/subagent/ -race -run 'Background|Job|Cancel|Shutdown|Concurrency'`

---

### Task 13: async-result 投递（TUI + headless）

**Files:** Modify `internal/tui/tui.go`、`cmd/agent/headless.go`、`cmd/agent/main.go`；Test `cmd/agent/headless_test.go`（新）、`internal/tui/hub_test.go`

**Interfaces:**
- Produces: `subagent.RenderAsyncResult(rs []JobResult) string`、`tui` 的 `jobSettledMsg`、`runHeadless` 的 auto-continue 循环

- [ ] **Step 1: 写失败测试**
  - `TestRenderAsyncResult`：单条/多条渲染都含 `<system-notice>`、job id、Name、status 与结果段。
  - `TestHeadlessAutoContinueBounded`：注入一个假的 "pending → settled" 序列（把 `Pending`/`TakeSettled` 抽成小接口便于替换）→ 最多 3 轮 auto-continue 后退出；每轮打印 `[job settled …]`。
  - `TestDeliverToActiveRunUsesSteer`：活动 run 存在时投递走 steer（断言 steer 通道收到消息），不新起 run。

- [ ] **Step 2: 实现**
  - `subagent.RenderAsyncResult` 按 spec §7.2 模板。
  - TUI：bus 订阅里遇 `ChJob` → `program.Send(jobSettledMsg{})`；`Update` 处理：`TakeSettled()` → 若为空忽略；否则聊天区落 `── 后台作业完成：… ──`，然后
    - 有活动 run（`currentSteer != nil`）→ 推 `message.NewUserMessage(text)`；
    - 无活动 run → 起一轮（复用 `Enter` 的 run 启动路径，输入文本 = async-result 文本，但**不**在聊天区显示为用户输入）。
  - headless：一轮结束后 `for i := 0; i < 3 && mgr.Pending() > 0; i++ { 等待（`--wait-jobs` 上限，轮询 200ms 或订阅 ChJob）→ TakeSettled → 打印 → 再跑一轮 }`。
  - `main.go`：`defer mgr.Shutdown(5*time.Second)`。

- [ ] **Step 3: 验证** `go test ./cmd/agent/ ./internal/tui/`

---

### Task 14: hub 工具（邮箱 / 唤醒 / parked revive）

**Files:** Create `internal/subagent/mailbox.go`、`internal/subagent/hub.go`、`internal/subagent/hub_test.go`；Modify `internal/subagent/manager.go`（工具集加 `hub`、`Send`、`Revive`）

**Interfaces:**
- Produces: `subagent.NewHubTool(mgr *Manager, self string) tool.Tool`、`(*Manager).Send(from, to, text, replyTo string) error`、`(*Manager).Inbox(name string) []Mail`、`(*Manager).Wait(ctx, name string, ids []string, d time.Duration) (string, error)`、`(*Manager).Revive(name, text string) (JobInfo, error)`、`(*Manager).TakeMainInbox() []Mail`

- [ ] **Step 1: 写失败测试**
  - `TestSendToRunningPushesSteer`：running Run 的 sidecar 里出现 `[hub from Main]` 消息且模型下一步能看到（断言 sidecar 内容）。
  - `TestSendToParkedRevives`：parked Run + `Send` → 产生一个新的后台作业（`Pending() == 1`），sidecar 追加新 user 条目，`Revives == 1`。
  - `TestSendToUnknownFails`：返回 error，文本含"no such peer"与可用名单。
  - `TestInboxDrains`：两条消息 → `inbox` 返回 2 条且再调为空；未读数归零。
  - `TestWaitWokenByMessage` / `TestWaitTimeout` / `TestWaitWokenByJob`：三种唤醒原因各自返回对应文本。
  - `TestHubListExcludesSelf`：`list` 不含自己，含 `Main`。
  - `TestHubToolInSubagentToolset`：子 agent 的工具集包含 `hub`（read tier）。

- [ ] **Step 2: 实现**
  - `mailbox`：`{mu, msgs []Mail, unread int, wake chan struct{}}`；`push` 非阻塞唤醒（`select default`）。
  - `Manager.Send`：查名册 → running/idle → 推 `Run.steer`（记为 user 消息 `[hub from X] …`）；parked → `Revive`；`Main` → 进主邮箱 + `bus.Publish(ChMailbox)`（TUI 提示）。
  - `Revive`：重开 sidecar（`session.Open`）→ 重建 tools/cc/YieldState → 新 `Run.revives++` → 走 `drive` → 结算按后台作业投递。
  - `hub` 工具：`Tier() = TierRead`，`Concurrency() = Shared`（`wait` 阻塞但不写状态）；`wait` 的 `Timeout` 夹到 `[1, 120]` 秒；description 按 spec §8（明确"只协调、不传长内容、能用工具查的别问 peer"）。
  - 工具集：`buildTools` 里统一加 `hub`（主 agent 的注册表也加）。

- [ ] **Step 3: 验证** `go test ./internal/subagent/ -race`

---

### Task 15: 配置、示例、文档与冒烟

**Files:** Modify `cmd/agent/config.go`、`cmd/agent/config_test.go`、`example.yaml`、`docs/DEVELOPMENT_LOG.md`；Create `.codeclaw/agents/README.md`（示例定义说明）

- [ ] **Step 1: 写失败测试**：`config_test.go` 覆盖新字段的默认值与"只能下调 soft_budget"的合并规则。
- [ ] **Step 2: 实现**：`subagentConfig` 增 `SoftBudget`、`MaxRecursionDepth`、`MinTaskChars`、`Background`、`AgentsDir`；`applyDefaults` 补默认（200 / 2 / 40 / true / ""）；`example.yaml` 加注释示例；`DEVELOPMENT_LOG.md` 追加 P9 段（含"本阶段踩的坑"）。
- [ ] **Step 3: 冒烟**（真实模型，`--yolo`）
  - `codeclaw --yolo -p "并行派两个 explorer 分别梳理 internal/agent 与 internal/subagent，然后综合"` → 观察 job id 立即返回、Hub 有两行、async-result 回投、最终综合。
  - TUI 里 `ctrl+a` 看面板；`/agent <Name> 追问…` 唤醒 parked。
- [ ] **Step 4: 提交** `feat: P9.4 异步与通信（后台作业 + async-result 投递 + hub 邮箱 + parked revive）`

---

## 验收对照表（对应 spec §1 验收）

| # | 验收 | 覆盖任务 | 验证方式 |
|---|---|---|---|
| 1 | 后台派发不阻塞 + 自动投递 | 12、13 | `TestStartBackgroundReturnsImmediately`、`TestSettledDeliveredExactlyOnce` + 冒烟 |
| 2 | Hub 看到当前工具与 token | 10、11 | `TestProgressPublished`、`TestRenderHubRows` + 冒烟 |
| 3 | 子 agent 间 hub 协作 | 14 | `TestSendToRunningPushesSteer` + 冒烟 |
| 4 | schema 错误退回重试后通过 | 8 | `TestYieldSchemaRetryThenPass`、strict/permissive 两例 |
| 5 | 追问已完成子 agent | 14、11 | `TestSendToParkedRevives` + `/agent` 冒烟 |
| 6 | idle 提醒恰好 3 次 | 9 | `TestLadderRemindsThreeTimes`、`TestForcedTurnOnlyHasYield` |
| 7 | 软预算三段式 | 9 | `TestSoftBudgetNoticeThenForcedYield`、`TestBudgetGraceExhaustedKills` |
| 8 | frontmatter 覆盖与容错 | 3 | `TestDiscoverPrecedence`、坏文件只告警 |
| 9 | 预检拒绝一行 prompt / 只读 / 深度 | 4、5 | `Preflight` 表驱动 + `TestToolSet*` |
| 10 | build/vet/test 全绿 | 全部 | 每任务 Step 3 |

## 风险与对策

| 风险 | 对策 |
|---|---|
| 阶梯 + 提醒让弱模型陷入"提醒→文本→提醒"循环烧 token | 提醒短、最多 3 次、最后一次只给 yield；软预算是硬上限，`killed` 兜底 |
| 取消当前 turn 留下悬空 tool_call | M1 回放修复已覆盖；driver 测试里显式断言"预算停机后下一 turn 请求成功" |
| 两轮并发写同一 sidecar | 事件通道必须读到关闭再进入下一 turn；`-race` 跑全部 subagent 测试 |
| 后台 Run 被父 turn 取消 | 后台一律挂 `Manager.root`；`TestStartBackground*` 用已取消的 ctx 断言仍能跑完 |
| async-result 重复投递 | `settled` 未投递集合是唯一消费点（`Jobs`/`Wait`/`TakeSettled` 共用），`TestJobsSnapshotConsumesDelivery` 守住 |
| `task` 参数破坏性变更导致旧会话续聊报错 | 旧字段 `subagent`/`prompt` 兼容映射 + 结果里提示新字段名 |
| 自己写的 schema 校验器覆盖不全 | 文件头声明支持范围；`permissive` 是默认（校验失败不硬失败）；`strict` 只给明确需要的 agent |
