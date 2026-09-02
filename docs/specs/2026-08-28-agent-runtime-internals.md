# Agent Runtime 内部机制详解（实现即文档）

> 日期：2026-08-28 · 依据代码：`internal/` 与 `cmd/agent/` 当前实现（P0–P11.2）
> 阅读方式：每个机制先讲**它怎么跑**（含 `文件:行号`），再讲**为什么这么设计**。
> 本文与 [2026-08-24-architecture-overview.md](2026-08-24-architecture-overview.md) 互补：那是骨架图，本文是逐机制的实现细节。

---

## 0. 总览：一条用户消息的旅程

```
用户输入 (TUI/headless)
  → cmgr.Record(user)                       ← 真相源：追加进 session JSONL
  → ag.Run(ctx, steer)                      ← 循环每步从真相源重建输入
      → cc.Build() → [system前缀] + [会话回放]
      → model.Stream(msgs, tools.Specs())   ← 唯一的 eino 依赖点
      → consumeStream 累积 → Record(assistant)
      → toolCallsOf → executor.ExecuteAll   ← 审批 → 执行 → Sink 截断落盘
      → Record(tool结果) → 回到循环
  → 事件流 AgentEvent → TUI 渲染 / headless 打印
```

三条承重不变量（设计 DNA，见 DEVELOPMENT_LOG §1）：

1. **事件驱动的循环**：`Agent.Run` 只吐事件，不做渲染；delta 与定稿消息分离（`internal/agent/event.go:13-27`）。
2. **追加式 JSONL 是唯一真相源**：会话转录 = trace = eval 输入（`internal/session/`）；UI 丢了没关系，循环里的 `Context` 才是真身。
3. **Tool 与 Runtime 分离 + 审批是纯策略**：工具是「面向模型的入口」，bash/文件 I/O 在 `internal/runtime`；审批是纯函数（`internal/permission`），不持有状态。

---

## 1. Agent Runtime：事件驱动的循环

### 1.1 装配（`cmd/agent/main.go:180-388`）

一次 `codeclaw` 进程启动做七件事：

| 步骤 | 位置 | 说明 |
|---|---|---|
| 项目作用域 | `main.go:181-208` | cwd 规范化 → `paths.ProjectDir(cwd)` 得到**项目桶**；会话/产物/记忆全部落桶内，跨项目互不可见 |
| 模型 | `main.go:193-201` | `model.New(...)`，唯一触碰 eino 的地方 |
| 记忆双库 | `main.go:214-236` | 项目库 `<桶>/memory.db` + 全局库 `<Home>/memory/global.db`，`memory.Union` 合成一个召回器 |
| 工具工厂 | `main.go:259-274` | `workerTools(cwd, store)`：**每次调用返回一套新工具**，每个 agent / 子 agent 拿到独立的 `runtime.Bash` 实例（cwd 隔离） |
| 审批 | `main.go:276-291` | mode + 规则 + approver；TUI 用三态弹窗，headless 用 `headlessApprover{}`（一律拒绝并说明） |
| 子 agent Manager | `main.go:295-304` | 委派运行时（见 §1.7） |
| 主 agent | `main.go:309-365` | 工具集按委派模式裁剪 → Executor → `agent.New("codeclaw", ...)` |

> **为什么装配在这里**：`cmd/agent` 是唯一知道「生产环境长什么样」的地方（配置三层、路径、记忆库、审批器）；`internal/agent` 保持对宿主一无所知，全部经接口注入。这使循环可以用 `MemoryContext`（`internal/agent/agent.go:59-118`）在测试里跑起来。

### 1.2 Context：循环的真相源抽象（`internal/agent/agent.go:13-23`）

```go
type Context interface {
	Build(ctx) ([]message.Message, error)      // 每步重建模型输入
	Record(m, u) error                         // 落盘一条消息
	ShouldCompact(u) bool                      // 上一步 usage 是否超阈值
	Compact(ctx) (method string, err error)    // "prune" | "summary" | ""
	RecoverOverflow(ctx) (method string, err error)
}
```

生产实现是 `context.Manager`（`internal/context/manager.go:56-71`，包着 `session.Session`）；测试用 `MemoryContext`。循环**不持有私有消息切片**——每步从 session 回放重建输入（`manager.go:130-139`）。

> **为什么把真相源放进循环而不是 UI**（P8 的核心修正）：早期版本循环持局部 `msgs` 切片，压缩只改 session 文件，循环里的旧切片照样把上下文撑爆。真相源单一化之后，「压缩 → 下一步 Build 生效」是自然结果，turn 内压缩与溢出恢复才成为可能（`internal/agent/loop.go:48-55, 130-139`）。

### 1.3 Run 主循环（`internal/agent/loop.go:16-116`）

```go
func (a *Agent) Run(ctx, steer) <-chan AgentEvent
```

- `Run` 开一个 goroutine，事件经**带 16 缓冲的 channel** 吐出；消费方 1 秒没读就丢弃——持久化已在循环内完成，丢事件只影响渲染（`loop.go:20-26`）。
- 循环体每步：

1. **steering**（`loop.go:41-47`）：非阻塞取注入消息（Ctrl+E、hub、后台作业结算），记为 user 消息，下一步模型调用生效。
2. **mid-turn 压缩**（`loop.go:49-55`）：上一步 `usage.PromptTokens` 超阈值 → 在下一次模型调用前压缩（先剪枝后摘要），成功后 `InvalidateReadHistory`（read_file 去重的前提「内容仍在上文」失效）。
3. **Build → Stream**（`loop.go:56-68`）：重建输入、请求模型、拿到 `ModelStream`。
4. **consumeStream**（`loop.go:157-180`）：把增量累积成完整 assistant 消息，emit `message_start/update/end`；流中途出错返回 error（参数可能被截断，绝不执行半截工具调用）。
5. **记录 + 提取工具调用**（`loop.go:83-90`）：assistant 落盘；无工具调用即 turn 结束。
6. **三档中断之「跳过」**（`loop.go:92-94`）：ctx 已取消则不启动工具——回放时悬空 tool_call 会被合成 `[interrupted]` 结果（见 §5.6）。
7. **ExecuteAll → 逐条落盘结果**（`loop.go:98-110`）：先记录完所有结果再检查终止，保持 tool_call/tool_result 配对完整。
8. **终止型工具**（`loop.go:111-114`）：`Result.Terminal`（yield 的终止提交）结束 run。

- 步数上限 `maxIterations=50`（`agent.go:41`），子 agent 可覆盖（`SetMaxIterations`，`agent.go:52-56`）。

> **为什么「终止」是循环级语义而不是工具返回值**（P8 修正 A1）：早期 yield 只把 "done" 写进结果文本，循环照跑。现在 `tool.Terminal` 按**调用**判定（`tool.go:22-27`）：yield 的增量提交不终止、出错重试不终止、只有「无 section 的最终提交」终止（`subagent/yield.go:139-144`）。

### 1.4 事件模型与流式累积

事件类型（`internal/agent/event.go:13-27`）：

```
EventAgentStart → EventTurnStart → (EventMessageStart → EventMessageUpdate* → EventMessageEnd
                                    → EventToolStart* → EventToolEnd*) 循环
→ EventTurnEnd → EventAgentEnd
+ EventError / EventCompaction / EventRetry / EventTerminated
```

**delta vs 定稿**：`MessageUpdate` 只带增量（`event.go:52-55`），`MessageEnd` 带完整消息与 usage。累积器 `streamAccumulator`（`internal/agent/state.go:12-61`）：

- text/thinking 直接拼接；
- 工具调用**按 `StreamingMeta.Index` 分组**（`state.go:15, 30-44`）：同 Index 的 Args 片段拼接，CallID 只在首个分块出现（DEVELOPMENT_LOG bug #2：早期按 CallID 分组把一次调用拆成两个）；
- 块顺序固定 thinking → text → toolCalls（`state.go:49-61`），保证落盘格式稳定。

### 1.5 模型层：唯一的 eino 依赖（`internal/model/`）

- 类型词汇（`internal/model/model.go`）：`ToolSpec`（模型视角，无执行逻辑，`model.go:12-17`）、`Usage`（provider 真值，`model.go:20-26`）、`ToolCallDelta`（`model.go:40-45`）、`ModelStream` 接口（`model.go:56-60`，测试可注入 fake）。
- `einoModel.Stream`（`internal/model/eino.go:26-39`）把 `message.Message` 转成 eino 的 `schema.AgenticMessage`（`eino.go:76-134`），工具定义转 `schema.ToolInfo`（`eino.go:134-171`），usage 从 `schema.TokenUsage` 换算（`eino.go:184-192`）。
- 错误分类是**纯特征串匹配**（`internal/model/errors.go:6-46`）：溢出 markers（"context_length_exceeded" 等）→ 压缩恢复；瞬时 markers（429/5xx/网络）→ 退避重试；两者互斥（`IsRetryable` 先排除溢出，`errors.go:35-37`）。

> **为什么用子串匹配而不是错误码**：不同 provider 的错误文本五花八门，harness 要能在**任何 provider 后**工作；分类失误的代价是单向的——溢出被当瞬时错误重试只会 400 循环，所以溢出判定更宽、且优先级最高。

### 1.6 宿主消费

- **TUI**：`internal/tui/tui.go:492-501` `runAgent`——先 `cmgr.Record(user)` 再 `for ev := range ag.Run(...)` 转发给 BubbleTea；`runMu` 保证同一时刻只有一个 run（DEVELOPMENT_LOG bug #6：双 run 竞态并发写 session）。`/new` `/resume` `/clear` 切会话时调用 `Registry().ResetConv()`（`tui.go:363-407`）。
- **headless**：`cmd/agent/headless.go:73-111` `runOnce`——把事件打印到 stdout；后台作业结算后自动续跑，最多 `maxAutoContinue=3` 轮（`headless.go:28-48`，防 CI 死循环）。

### 1.7 子 agent 驱动：turn 阶梯（`internal/subagent/driver.go:198-258`）

每个子 agent Run 由 `Manager.drive` 驱动，**每 turn 一个可单独取消的 ctx**：

```
turn: agent.Run(turnCtx, r.steer) 消费到关闭
  ├─ 有 terminal yield            → 结算
  ├─ runCtx 取消 / killed         → 结算（aborted/killed/timeout）
  ├─ 软预算停机 budgetStop        → 强制收尾 turn：工具集换成只剩 yield（forcedTurnIterations=3）
  └─ 无 yield                     → 注入提醒（≤3 次），最后一次同样只给 yield
```

常量在 `driver.go:24-29`：`maxYieldReminders=3`、`budgetStopMultiplier=1.5`、`budgetGraceRequests=5`。提醒/通知文案由 `idleReminder`/`budgetNotice`/`forcedBudgetNotice` 生成（`driver.go:455-472`），作为 user 消息落进 sidecar 会话（`driver.go:244, 254`），模型看到的就是「还剩几次机会」。

> **为什么事件通道必须读到关闭**（`driver.go:227` 注释）：`agent.Run` 的 goroutine 在通道关闭后才结束；不读干净，下一 turn 会与它并发写同一 session 文件。

---

## 2. 工具：定义、注册、调用

### 2.1 Tool 接口与三个可选接口（`internal/tool/tool.go:11-44`）

```go
type Tool interface {
	Name() string
	Description() string
	Parameters() map[string]any // JSON Schema properties（给模型的工具定义）
	Tier() permission.Tier      // 基线危险等级：read / write / exec
	Concurrency() Concurrency   // Shared 可并行 / Exclusive 必须串行
	Execute(ctx, args map[string]any, sink *runtime.Sink) error
}
type Terminal interface { IsTerminal(args, err) bool }          // 按调用判定是否终止 run
type Decisioner interface { Decision(args) permission.ToolDecision } // 按调用自检审批（替代固定 Tier 的扩展）
type RequiredParams interface { Required() []string }
```

- `Execute` 把结果写进 `Sink`，返回 error 表示执行失败——**工具自己不做截断/落盘**，那是 Sink 的职责（Tool 与 Runtime 分离）。
- `Concurrency` 声明并发性：`bash`/`write_file`/`edit` 是 Exclusive（串行），`read_file`/`grep` 等是 Shared（可并行）。
- `Decisioner`（P11.1）让「按参数审批」成为可能：`echo hi` 只读、`echo x > /etc/passwd` 危险，固定 Tier 区分不了（见 §2.7）。

### 2.2 Registry（`internal/tool/registry.go`）

- 统一名字空间：内置工具与 MCP 工具都注册进同一个 map（`registry.go:11-25`）；MCP 归一为 `mcp__<server>_<tool>`（`internal/tool/mcp.go:36`）。
- `Specs()`（`registry.go:92-113`）生成给模型的工具定义，**按名字排序**——工具定义顺序稳定是 prompt 前缀稳定的前提之一。
- `Without(name)`（`registry.go:36-46`）供子 agent 裁剪（防递归派发）。
- 会话级状态的两个钩子：`ResetConv`（`registry.go:69-75`，换会话清全部）与 `InvalidateReadHistory`（`registry.go:83-89`，压缩后只清已读区间、保留 edit 守卫指纹）。

### 2.3 内置工具（`internal/tool/tools.go:16-26`）

`Builtins(bash, store)` 返回 6 个工具：`read_file` / `write_file` / `edit` / `glob` / `grep` / `bash`。read/write/edit **共享一个 `fileGuard`**（`tools.go:17-25`）：

- `read_file`（`tools.go:72-122`）：按行读取（offset/limit，UTF-8 安全）；支持会话内 URL（`artifact://`/`agent://`/`history://`，经 `ArtifactStore.Resolve` 路由）；会话内去重——同一文件 mtime+size 未变且请求区间已被已读区间并集覆盖时，返回「文件未变更（上次读过第 a-b 行）」（`tools.go:110-113`）。
- `write_file`（`tools.go:148-165`）：覆盖写；成功后 `markWritten` 更新 guard 指纹。
- `edit`（`tools.go:199-244`，P11.2）：先读后改——`guard.freshRead`（没读过或读后被外部改过都拒绝）；`old_string` 逐字节唯一匹配（否则报告出现次数）；字节级替换天然保留换行与 BOM。
- `bash`（`tools.go:302-308`）：委托给 `runtime.Bash`（每 agent 独立实例，cwd 持久化）。

### 2.4 bash 运行时（`internal/runtime/bash.go`）

- `NewBashWithTimeout(cwd, timeout)`（`bash.go:24-37`）：cwd 绝对化，默认 120s 超时（配置可到 600s，`cmd/agent/config.go` applyDefaults 钳制）。
- `Execute`（`bash.go:45-82`）：
  - `parseCd` 解析 `cd <path> && ` 前缀（`bash.go:87-97`），cwd 实例内持久化；
  - `WithTimeout` + `Setpgid` 独立进程组；取消时 `cmd.Cancel` 先 SIGTERM 进程组、5s 后 SIGKILL 补刀（`bash.go:59-67`）——管道与后台孙进程一起回收；
  - `SanitizeEnv(nonInteractiveEnv())`：非交互硬化 + 剔除密钥类环境变量（`internal/runtime/sandbox.go:38-52`）；
  - 超时返回明确错误「命令超时（120s），已终止进程组」（`bash.go:77-79`），模型看到的是失败而非截断输出。
- **分类器** `runtime.Classify`（`internal/runtime/classify.go:13-20`）：按 `|`/`||`/`&&`/`;`/换行切段，逐段保守判定——任一段危险即危险、全段只读才算只读、未知命令回落 exec（询问）。只读白名单含 git/go 的**子命令特判**（`classify.go` `gitReadOnlySub` 等）；危险模式含 `rm -rf`/`sudo`/`mkfs`/`dd of=`/`curl|sh`/`git reset --hard`/fork bomb/重定向写系统路径（`classify.go:41-78` 的 `dang*` 正则族）。

> **为什么分类器是纯函数**：40+ 条样例集（`classify_test.go`）单测钉住。安全默认的保守方向很明确——漏判危险是事故、误判只读只是多一次审批，所以不认识的命令一律回落询问。

### 2.5 Executor 执行链（`internal/tool/executor.go`）

一次工具调用的完整路径（`Execute`，`executor.go:75-137`）：

```
查表 Get(name)
  → json.Unmarshal(call.Args)          // 非法 JSON 按空参处理（不因模型手抖中断 run）
  → Decisioner 自检（可选）→ ToolDecision{Tier, Policy, Override, Reason}
  → permission.ResolveRules(td, rules, mode, name, args) → allow / deny / prompt
      deny   → "tool denied: <原因>"（规则原文/工具原因可见）
      prompt → 无 approver 拒绝；有 approver 阻塞等用户（HITL 中断点）
               「本会话允许」（sessionAllow，只豁免非 Override 的 prompt）
  → 执行：sink := NewSink(4000, 4000) + SetArtifactStore
  → t.Execute(ctx, args, sink)
  → Terminal 判定 + 错误塑形 → Result{Content, IsError, Terminal}
```

**并发模型**（`ExecuteAll`，`executor.go:139-163`）：Shared 工具 goroutine 并行（Semaphore 上限 8，`executor.go:47`），Exclusive 串行——遇到 Exclusive 先 `wg.Wait()` 等并行批次完成再执行（`executor.go:146-149`），结果按调用序回填。`acquire` 带 ctx 取消（`executor.go:165-173`）。

> **为什么审批在工具执行前、且决策理由进结果文本**：模型需要知道「为什么被拒」才能改对路径（例如换一条不危险的命令）；而「denied by rule: read(./.env*)」直接指出是用户规则，避免模型重试撞墙。

### 2.6 Sink：L6 截断落盘（`internal/runtime/sink.go`）

- `Write`（`sink.go:39-65`）：累积输出；超过 `headLimit+tailLimit`（执行器用 4000+4000，`executor.go:14-17`）时保留头尾、中间 elide，完整内容写进 artifact 文件（首次截断时把当前全部内容落盘，`sink.go:51-58`）。
- `Result`（`sink.go:68-83`）：未截断=原文；截断=`头 + ...(N bytes elided)... + 尾 + [完整输出已保存: artifact://N …]`。
- `ArtifactStore`（`internal/runtime/artifact.go:23-33`）：会话产物目录、id 扫描分配（resume 不覆盖旧产物）；同时是**会话内 URL 路由表**——`AddScheme` 注册 `agent://`/`history://`（`main.go:306`），`read_file` 见到任意 `scheme://` 都交给它（`tools.go:77-87`）。**读回大内容对模型永远只有 read_file 一个入口**。

### 2.7 审批决策：tier × mode × 规则 × 自检（`internal/permission/`）

- 基线：`Resolve(tier, mode)`（`policy.go:31-43`）——read/write/exec × yolo/write/always-ask，纯函数。
- 扩展：`ToolDecision{Tier, Policy(allow|deny|prompt), Override, Reason}` + 用户规则 `Rules{Allow,Ask,Deny}`（`rules.go:20-36`），`ResolveRules` 五步（`rules.go:100-154`）：

```
1. 工具 deny            → deny（永远）
2. 用户 deny 规则命中    → deny（任何模式含 yolo）
3. yolo                → 工具显式 allow/prompt、用户 allow/ask 命中 → allow（裸 Override 忽略）
4. 非 yolo 且 Override  → prompt（除非工具显式 allow）
5. 工具显式 policy      → 用户规则 → tier×mode
```

- **回归不变量**：空规则 + 无 Decisioner 时与旧 `Resolve(tier, mode)` 逐字节等价（`rules_test.go: TestResolveRulesEmptyRulesEquivalent`）。
- 规则语法 `tool(args*)`：`*` 通配，`read(./.env*)` 这类路径规则；配置三层**列表追加**合并（用户 deny 不被项目 allow 顶掉，`cmd/agent/config.go` `mergeConfig`）。
- bash 自检：只读 → TierRead（write 模式免审批）；危险 → Override 强制询问（**yolo 下也拦**，`tools.go:290-300`）。
- HITL：TUI 三态弹窗 `[y]允许 [a]本会话允许 [n]拒绝`（`internal/tui/approval.go:9-44` + `tui.go:258-268`）；headless `headlessApprover` 一律拒绝并说明（`cmd/agent/headless.go:18-23`）。子 agent 默认 `denyApprover`（拒绝并说明），开 `approval_escalation` 才升级到父弹窗（带 `[子 agent X]` 标签，`internal/subagent/manager.go:412-415`）。

---

## 3. 参数怎么校验

参数校验分**三层**，各有分工：

### 3.1 声明层：JSON Schema 交给 provider（喂给模型）

- 工具用 `Parameters()` 声明 properties（`tools.go:59-65` 等），`RequiredParams.Required()` 声明必填（`tools.go:66`）。
- `Registry.Specs()` 汇总（`registry.go:92-113`）→ `toSchemaTools` 转成 eino 的 `schema.ToolInfo`（`model/eino.go:134-171`）→ 随请求发给 provider。
- 这一层是**给模型的契约**：描述写得好不好直接决定模型调用质量。例如 `edit` 的描述明确「old_string 须逐字节唯一匹配」「先 read 后 edit」（`tools.go:182-186`）。

> **为什么不做调用前的 schema 强制校验**：模型偶尔输出不合规参数（缺字段、类型错），harness 的立场是**失败要便宜、反馈要能改对**——所以把校验放进工具 Execute 的第一行（返回带指导的错误），而不是在 Executor 里做一次严格拦截（那样模型只会看到「bad request」，不知道改哪）。

### 3.2 运行时解析与错误反馈（`executor.go:80-83` + 各工具首行）

- `json.Unmarshal(call.Args)` 到 `map[string]any`；**非法 JSON 按空参处理**——参数截断/损坏时，工具会得到空 args 并在首行返回「xx 必填」，run 不中断，模型看到错误后重试（`tools.go:74-76`、`tools.go:151-153`、`tools.go:204-206`、`tools.go:304-306`）。
- 数值断言用 `float64`（JSON 解码形态，`tools.go:98-104`）；工具内再做语义校验（`edit` 的 old 唯一性、yield 的 data/error 互斥）。

### 3.3 yield 的 schema 子集校验器（`internal/subagent/schema.go`）

子 agent 的结构化产出是**唯一有完整 schema 校验的参数路径**：

- 支持子集：type（含类型数组）/required/properties/items/enum，递归校验，返回人类可读问题列表（`schema.go:21-31`、`validateNode:33-77`）；有意忽略 $ref/oneOf/pattern 等——「误报代价高于漏报」（`schema.go:10-16`）。
- **线格式放松**：`deriveDataSchema` 递归去 required、对象放开 additionalProperties（`schema.go:198-228`）——增量分段提交的是部分数据，线格式必须容得下；完整性由工具内 `Validate`（原 schema）负责。
- **重试协议**（`yield.go:189-224`）：校验失败带路径与剩余次数退回（`schemaRetry:227-234`，≤3 次）；超限后 permissive 放行 + 警告、strict 判 `schema_violation` 失败。空提交同样 ≤3 次后判失败（`yield.go:190-201`）。三态互斥在 `Execute` 首行（`yield.go:154-156`）。
- yield 的 `Required()` 是**空**（`yield.go:131`）：data/error/section 三态互斥，顶层 anyOf 对部分 provider 不安全，语义放工具内校验。

### 3.4 task 派发预检（`internal/subagent/preflight.go:36-98`）

`task` 工具的契约校验发生在**起子进程之前**：空 context / 一句话派发（<40 字符）/ 未知 agent / 深度超限 / spawn policy / 同名递归，全部在 `Preflight` 里拒绝并返回原因——**预检失败不起子进程**，把错误文本直接还给模型。`task` 参数 schema（`internal/subagent/task.go:60-88`）把 `context/tasks[]` 结构暴露给模型。

---

## 4. 记忆怎么设计

记忆分**三层**，各自独立（spec `docs/specs/phase-10-memory-context.md`）：

| 层 | 载体 | 说明 |
|---|---|---|
| L1 指令记忆 | 文件：`~/.codeclaw/AGENTS.md` → git 根到 cwd 逐级 `AGENTS.md`/`CLAUDE.md`，`@import` 展开、`RULES.md` 粘性 | `internal/instructions/`，注入为 system 前缀第二部分 |
| L7 事实记忆 | SQLite 双库（项目 + 全局） | 「构建命令是 X」「用户偏好中文」这类一句式事实 |
| L7′ 项目知识 | `file_notes` 表 + 项目地图注入 | explorer 子 agent 的结构化产出确定性沉淀 |

### 4.1 事实记忆：Schema v2 与迁移（`internal/memory/memory.go`）

- 表结构（`memory.go:97-118`）：`memories`（scope/project_id/kind/key/content/why/source/veracity/importance/时间戳/access_count/superseded_by/tags）+ FTS5 trigram 虚表 + `(scope, project_id, key)` 唯一索引。
- 幂等迁移（`migrateLegacy:132-176`）：P5 的 `working_memory` 在 `memories` 为空时搬运一次，**旧表保留不删**——记忆是用户数据，出问题要能回查。
- **作用域是库的属性而非写入参数**（`memory.go:52-58` 注释）：曾经允许按参数指定 scope，结果是「降级到项目库却仍标 global」的孤儿行——谁都查不到。现在调用方选哪个库就写哪个作用域。

### 4.2 写入路径（`Remember`，`memory.go:202-234`）

```
trim → 密钥检测（命中整条拒绝）→ 截断（>2000 字，标 truncated）
→ 有 key：upsert（保留 created_at/access_count，importance 取较大值）
→ 无 key：FTS 取 top5 候选 + trigram Jaccard ≥0.85 → 更新而非新增
→ insert（主表与 FTS 同事务）
→ 超限淘汰：按 importance×recency×veracity 最低分真删
```

- **密钥拒绝而非打码**（`memory.go:178-198`）：半条密钥仍是泄漏面；错误文本告诉模型「记引用（配置项名、文件路径）而不是值」。
- **失效而非删除**：`Forget` 置 `veracity=0` 并在 why 记原因（`memory.go:372-384`）、`Invalidate` 记 `superseded_by`（`387-391`）——过时记忆要能回答「当初为什么这么记、什么时候作废的」。
- 近重复检测用 `Similarity`（`query.go:89-108`）：trigram Jaccard，同义改写「用户偏好中文回复」vs「用户希望用中文回复」命中。

### 4.3 召回路径（`Recall`，`internal/memory/retrieval.go:126-145`）

```
query → FTSQuery 清洗（分词、<3 字符丢弃、CJK 3 字滑窗、双引号转义、OR 连接、≤24 项）
      → 有实词：MATCH（trigram）+ bm25 排序，多取 4 倍交给打分
      → 无实词 / FTS 零命中：兜底召回（importance×recency）
→ scoreMemory 五信号打分 → topK → touch（访问回写）
```

- **FTSQuery 清洗是 crit 修复**（`query.go:23-53`）：用户原文直传 `MATCH` 遇到 `?()"-` 报语法错，错误被上层吞掉后表现就是「记忆功能看起来在，实际永远召回不到」。现在清洗后永不报错，无实词走兜底（「你还记得什么」也有结果）。
- 打分纯代码（`retrieval.go:23-28`）：`(0.45·fts + 0.20·importance + 0.15·recency(72h 半衰) + 0.10·log10(access) + 0.10·scopeBoost) × veracity`——调参能被单测钉住，不靠 LLM judge。
- 多库合并 `Union`（`retrieval.go:184-216`）：各自召回后按分数合并、按 content 去重；**单库出错不拖垮其它库**；全空且有错才报错。
- 访问回写 `touch`（`retrieval.go:148-164`）：批量一条 UPDATE，失败只记不抛——召回主流程不受影响。

### 4.4 注入位置与频率（`cmd/agent/main.go:329-363`）

- 注入形态：`<memories>` 块固定排在 **system 前缀末尾**（`[基础指令+env] [项目指令层] [记忆块] [项目地图]`），带声明「当前用户消息与工具结果优先」。
- **只在三个时刻刷新**：会话首轮 / 压缩后 / 换会话——由 `context.Manager` 的前缀缓存保证（`manager.go:66-71, 106-118`：`sysDirty` 标记 + `InvalidateSystem()`）。其余轮次复用缓存 → **前缀字节稳定 = provider 的 prompt cache 每轮命中**，这是长会话里最大的隐性成本（实测第二次模型调用 `cached=2304`）。
- 召回查询用 `BuildRecallQuery`（`query.go:125-145`）：最近 3 个用户 turn + 当前任务，截 4000 字。
- `remember` 工具的结果文本说明生效时机（`memory_tool.go:73-75`）：「它会在下次压缩或新会话时进入背景上下文」——记忆是背景信息，不为它破坏前缀缓存。

### 4.5 项目知识 file_notes（`internal/memory/notes.go`）

- 表：`file_notes(project_id, path, summary, symbols, mtime, size, hit_count)`（`notes.go:13-27`）。
- 来源：explorer 类子 agent 的结构化产出 `files:[{path, role}]` 在**结算时确定性 upsert**（`internal/subagent/driver.go:388-408`，`upsertNotes`）——不需要额外模型调用。
- 注入：会话首轮 `<project-map>`（按目录分组、hit_count 排序、预算 1.5k token，`notes.go:60-85`）；mtime/size 变了标「(可能已过时)」（`noteStale:87-94`）。
- 决策（spec §0 #10）：**read_file 默认不用笔记顶替内容**——拿摘要冒充文件内容会让模型基于旧信息改代码，是正确性问题；只做安全的会话内去重（§2.3）。

---

## 5. 失败 case 怎么处理

### 5.1 模型错误三分类（`internal/agent/loop.go:126-153` + `internal/model/errors.go`）

```
模型出错
├─ ctx 已取消            → 安静退出（不报错、不重试）
├─ IsContextOverflow     → 溢出恢复通道：RecoverOverflow（保留量减半压缩），与重试互斥
├─ IsRetryable           → 指数退避重试：500ms·2^(n-1)，上限 3 次，EventRetry 可见
└─ 其它                  → EventError，run 结束（TUI 显示、headless 退出码 1）
```

- 重试与溢出**互斥**（`errors.go:35-37`）：溢出重试只会 400 循环。
- 重试不计步（`loop.go:63-66, 72-75`：`step--`），不消耗 maxIterations 预算。

### 5.2 上下文溢出恢复（`internal/context/manager.go:169-176`）

`RecoverOverflow`：保留量减半再走压缩阶梯；仍无可压内容则 `compact(ctx, 1)` 只留最后一段——保证**一定能腾出空间**，循环恢复后 continue（`loop.go:130-139`）。压缩方式进事件 reason（`overflow:prune` / `overflow:summary`），TUI/headless 可观测。

### 5.3 mid-turn 压缩阶梯（`loop.go:49-55` + `manager.go:206-273`）

先便宜后昂贵：

```
Compact：tryPrune（L6 剪枝，零模型调用）→ 省够 20k 就落一条 prune 边界，返回 "prune"
       → 不够：findCutPoint（切点只在 user 或无 tool_call 的 assistant 上，compaction.go:13-45）
       → 模型六字段摘要 + <files> 树（manager.go:222-229）
       → sess.Compact(summary, firstKeptID) + 压缩后 <recent-files> 恢复消息（manager.go:238-248）
```

- **剪枝落盘**：`PlanPrune`/`ApplyPrune`（`internal/context/prune.go:37-79`）——保护最近 40k、单条 <50 不剪、至少省 20k；边界落 `prune` 自定义条目，**回放期应用**占位（JSONL 审计完整、前缀单调不 churn）。
- **切点安全**（`compaction.go:13-45`）：`findCutPoint` 从新到旧累计 keepTokens，再继续向旧移动到 `safeCut`——切点绝不落在 tool 消息、绝不拆 tool_call/tool_result 配对（P8 修的 400 类问题）。
- 压缩后 `InvalidateSystem()`（`manager.go:249`）：记忆块与粘性规则重贴——这是前缀允许变化的少数时刻之一。

### 5.4 工具失败

- 工具 Execute 返回 error → `Result{Content: 原文+[tool error: …], IsError: true}`（`executor.go:133-136`）→ 作为 tool 消息**落盘**（`loop.go:103`）→ 模型下一步调用能看到错误并自行修正。
- 审批拒绝：deny 结果带原因（规则原文/分类器理由），模型可见（§2.5）。
- bash 超时：进程组回收 + 明确错误（§2.4）。
- 大输出：Sink 截断 + artifact 指针，内容不丢（§2.6）。

### 5.5 取消与中断

- **三档中断**：硬杀（ctx 取消）在等待模型/审批时生效；「让位」在工具执行中由工具自决（bash 的进程组回收）；「跳过」= 已取消则不启动未执行的工具（`loop.go:92-94`）——**绝不硬杀有副作用的工具**。
- 取消且流里没有内容 → 不记录空 assistant（`loop.go:78-80`）。
- 事件投递 1s 保护（`loop.go:20-26`）：消费者消失不阻塞循环，持久化不受影响。

### 5.6 会话悬空修复（`internal/session/replay.go:89-109`）

Ctrl+C 中断可能留下「assistant 有 tool_call 但没有结果」的孤儿。`repairDangling` 在**回放期**为每个无配对的 tool_call 合成 `[interrupted: tool did not run]` 结果（仅回放、不落盘）——`/resume` 后首个请求不会被 API 以 insufficient tool messages 拒绝（P8 修的 B3）。

### 5.7 子 agent 失败状态机（`internal/subagent/driver.go:318-384`）

`settle` 按优先级判定终态：

```
parent 取消            → aborted（保留 transcript 与 partial）
killed（预算宽限耗尽/人工）→ killed
软预算停机仍未 yield      → killed
runCtx 超时             → timeout（"子 agent 超时（10m）"）
runErr                 → failed
yield(error=…)         → failed（阻塞描述带回父 agent）
strict schema 违规      → failed（schema_violation）
terminal yield         → completed（permissive 放行带 warning）
无 schema 且提醒耗尽     → completed + 警告「最后一段可能不是完整结论」
```

- **completed ≠ 验收**：父提示词明确「completed 只表示成功 yield，不表示产物可接受」。
- 结算后 Run 变 **parked**（`driver.go:376-381`）：转录与产出保留在产物目录（`writeOutput:411-451`，父只拿摘要 + `agent://<Name>` 指针），可被 `hub send` 唤醒续跑（`resetForRevive:183-194`，预算与提醒按新一次重计）。
- 软预算三段式（`checkBudget:298-315`）：越界 → steering 注入收尾通知（一次）；1.5× → 停当前 turn 强制收尾（工具集只剩 yield）；宽限 5 次请求 → 硬杀。
- 失败都不白费：`Result` 带 `SessionFile`/`OutputFile`/`lastText`/`Sections`，父可读 `history://<name>` 追问细节。

### 5.8 后台作业与 panic（`internal/subagent/`）

- 后台 Run 挂 **Manager 根 ctx**（父 turn 结束、用户 Esc 都不带走它），`Shutdown` 统一收（`main.go:367-371`）。
- 结算进「待投递」队列，`TakeSettled`/`hub jobs`/`hub wait` 共用——谁先看到谁消费，**恰好一次投递**。
- goroutine 入口 `recover`（`internal/subagent/manager.go:329`、`jobs.go:134, 269`）：panic 不带走宿主进程，转成 failed + 堆栈可查。

---

## 6. 设计原则回顾（为什么这么设计）

| 原则 | 落点 |
|---|---|
| **真相源唯一** | 循环从 `Context.Build()` 重建输入（`agent.go:15-23`）；JSONL 即 trace/eval 输入 |
| **便宜优先** | 压缩阶梯剪枝（零模型调用）→ 摘要；记忆召回无实词走兜底而非空返回 |
| **失败可反馈** | 所有拒绝/错误都带「原因 + 怎么改」（deny 规则原文、yield 的 schema 路径、edit 的 read-before-edit 提示） |
| **保守方向单向** | 未知 bash 命令回落询问、密钥整条拒绝、坏规则只告警跳过、edit 守卫 fail-safe 拒绝 |
| **纯函数承载正确性** | 审批决策、bash 分类、FTS 清洗、记忆打分、剪枝计划全是纯函数——bug 只能靠单测发现，所以全部拆出来钉住 |
| **前缀稳定** | system 前缀缓存（`manager.go:66-71`）、工具定义排序（`registry.go:95`）、记忆块只在三时刻刷新——prompt cache 是长会话最大的隐性成本 |
| **隔离落到具体资源** | 每个 agent 独立 `runtime.Bash` 实例（cwd 隔离）、独立 session sidecar、独立审批器装配——而不是 prompt 上写「别互相影响」 |

---

## 附：关键文件索引

| 主题 | 文件 |
|---|---|
| 循环/事件/累积 | `internal/agent/{agent,loop,event,state}.go` |
| 模型抽象 | `internal/model/{model,eino,errors}.go` |
| 上下文真相源 | `internal/context/{manager,compaction,prune,files,tokenizer}.go` |
| 会话 JSONL | `internal/session/{session,entry,replay,prune,manager}.go` |
| 工具 | `internal/tool/{tool,registry,executor,tools,fsguard,search,mcp,memory_tool}.go` |
| 运行时 | `internal/runtime/{bash,sink,artifact,sandbox,classify}.go` |
| 审批 | `internal/permission/{policy,rules}.go` |
| 记忆 | `internal/memory/{memory,retrieval,query,notes}.go` |
| 指令层 | `internal/instructions/` |
| 子 agent | `internal/subagent/{manager,driver,yield,schema,task,preflight,hub,jobs}.go` |
| 装配 | `cmd/agent/{main,config,headless}.go` |
