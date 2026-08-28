# einoclaw-build 开发全记录

> 从零手写一个 AI 编程智能体 harness 的完整历程。从「eino 全包办」到「手写核心 + eino 只做模型调用」。
> 模块：`einoclaw-build`（参考原项目 `einoclaw`，学习对象 `oh-my-pi`）

---

## 0. 项目缘起与目标

### 缘起

原项目 `einoclaw` 是一个基于 CloudWeGo `eino` 框架的编程智能体，核心（agent 循环、session、上下文、工具、权限）几乎全由 eino 的 `adk.TurnLoop` / `NewTypedChatModelAgent` / 中间件包办——「会用，但没自己写过」。

### 目标

参照开源桌面 agent `oh-my-pi` 的核心设计，**从零手写一个 harness**：

1. **eino 退到一个点**：只保留 `components/model`（模型客户端 + 流式 + 工具调用），封装在 `internal/model`；业务代码不 import eino。
2. **手写 harness 核心**：循环、session、上下文、记忆、工具运行时、权限、subagent、trace、eval。
3. **每阶段可验证**：以「能编译 + 能观察到一个行为」收尾，不攒大块未验证代码。

### 最终形态

一个完整的 agent harness 闭环（跑 → 测 → 改）：

```
P0 地基 → P1 Agent Loop → P2 Session → P3 Context → P4 工具运行时
→ P4.5 审批 HITL → P5 记忆 → P6 Subagent+MCP(+多agent编排) → P7 Trace+Eval
+ 多会话/resume + 虚拟滚动 + steering
```

---

## 1. 三条承重不变量（贯穿所有阶段的设计 DNA）

从 oh-my-pi 提炼，每条都是一个阶段的主题，也是验收的判据：

1. **事件驱动的循环**（P1）：turn/step/run 三层；流式增量（delta）与定稿消息分离；三档中断（硬杀可中断等待 / 软信号让工具让位 / 跳过未启动工具），**绝不硬杀有副作用的工具**。

2. **追加式 JSONL 是唯一真相源**（P2）：session 转录 = trace = eval 输入，同一份文件；追加写 + 可变指针；`reset_boundary` 封存旧上下文。

3. **Tool 与 Runtime 分离 + Approval 是纯策略**（P4）：Tool（面向模型的入口）≠ Runtime（进程引擎）；审批 = `Resolve(tier, mode) → Allow|Prompt|Deny` 纯函数，不持有状态。

外加一条贯穿全程的原则：**Context = Index，不是 Everything**（分层 Context L0-L7，模型上下文放「索引+精华」，原始数据在别处）。

---

## 2. 阶段总览

| 阶段 | 主题 | 核心产出 | 状态 |
|---|---|---|---|
| P0 | 地基 | `internal/message` + `internal/model` | ✅ |
| P1 | Agent Loop | 事件驱动循环 + 累积器 | ✅ |
| P2 | Session | JSONL 追加式会话 | ✅ |
| P3 | Context | 预算 + 六字段压缩 | ✅ |
| P4 | Tool Runtime + Permission | 工具运行时 + OutputSink + 审批 | ✅ |
| P4.5 | 审批 HITL | interrupt/resume + 弹窗 | ✅ |
| P5 | Structured Memory | SQLite + FTS5 多信号召回 | ✅ |
| P6 | Subagent + MCP | Context Isolation + 外部工具 | ✅ |
| P6-L1 | 委派策略 | orchestrator/worker + 委派模式 | ✅ |
| P6-L2 | 派发运行时 | tasks[] 批量 + yield + 状态机 | ✅ |
| P7 | Trace + Eval | JSONL 审计 + 夹具评测 | ✅ |
| 扩展 | 多会话/滚动/steering | /new /resume /forget + 鼠标 + Ctrl+E | ✅ |
| P8（M1） | 地基修正 | 项目作用域数据目录 + Session v2 + 压缩正确性 + 子 agent 运行时修正 + headless `-p` | ✅ |
| P9（M2） | 委派运行时 | frontmatter agent + TaskBatch 预检 + yield 三态 + 提醒/预算 + Agent Hub + 后台作业 + hub 邮箱 | ✅ |
| P10（M3） | 记忆与上下文 | FTS 清洗 + 记忆 v2 + 双库召回 + 前缀缓存 + L1 项目层 + 剪枝阶梯 + 项目知识 + read 去重 | ✅ |

---

## 3. 各阶段详细记录

### P0 · 地基（拆包 + 消息类型 + 模型封装）

**目标**：把 eino 关进一个包，立起多包骨架。

**关键设计**：
- 引入 `internal/message`（共享消息类型 `Message`/`ContentBlock` 四类块），解决 `agent ↔ model` 的循环依赖——类型词汇与会做 I/O 的模型客户端分离。
- `internal/model` 是**唯一 import eino 的包**：`Model` 接口 + eino 适配。
- **双消息模型**：`message.Message`（业务词汇）↔ `schema.AgenticMessage`（eino 模型消息），转换只发生在 `internal/model` 边界。

**踩坑**：eino 版本兼容——非 agentic 的 `openai/qwen/ark/deepseek@v0.1.x` 锁死 `eino v0.7.13`，与项目 `v0.10.0-alpha.9` 不兼容；必须用 agentic provider 包（直接基于 OpenAI SDK + acl 层），返回 `BaseModel[*schema.AgenticMessage]`。

---

### P1 · Agent Loop（事件驱动循环）

**目标**：手写循环替换 eino `TurnLoop`。

**关键设计**：
- `AgentEvent` 事件联合：`message_start`（流即将来）/ `message_update`（增量 delta）/ `message_end`（定稿完整消息）——**delta vs 定稿分离**。
- `streamAccumulator`：把增量累积成完整 `message.Message`（text/thinking 拼接、工具调用按 Index 合并）。
- `consumeStream` 抽成「最小接口 `eventStream{Recv,Usage}`」+ 回调，**可注入 fakeStream 单测**（不依赖真模型）。
- 三档中断骨架；P1 先做 ctx 取消。

**学到**：为可测试抽出纯核心（事件流消费独立于模型 I/O）。

---

### P2 · Session（JSONL 单一真相源）

**目标**：自写 JSONL 会话替换 eino `session.NewFileStore`。

**关键设计**：
- `Entry`（session 头 / message / reset_boundary / compaction）+ `Storage` 接口（`FileStorage`/`MemoryStorage`，后者供单测）。
- `Replay()`：追加日志重建消息序列；`reset_boundary` 封存（回放截止点）。
- `Compact`（P3 起）：「写摘要 + 重追加保留」替代 `firstKept` 索引追踪。
- `Fork`：快照复制（父/子隔离）。

---

### P3 · Context（预算 + 六字段压缩）

**目标**：长对话自动摘要，不溢出。

**关键设计**：
- **provider usage 是 token 真值**：阈值判断用 `Usage.PromptTokens`；本地 `estimateTokens`（`len(runes)/2` 启发式）只用于找压缩切点。
- 预算模型：`threshold = window − max(15%·window, 16384)`。
- **六字段任务导向摘要**（用户提出、后续升级）：目标/状态/决策/文件/失败/下一步——「保留未来 Agent 继续任务所需信息」，而非泛化总结。摘要迭代回喂天然发生（摘要在消息列表里）。

**学到**：「Context = Index」原则的第一次落地（上下文放精华，不是全量）。

---

### P4 · Tool Runtime + Permission（从「调用函数」到「运行时」）

**目标**：工具运行时 + 审批，替换 eino filesystem 中间件。

**关键设计**：
- `Tool` 接口 + `Registry`（统一注册表）+ `Concurrency()`（Shared/Exclusive）。
- `runtime.OutputSink`：头尾窗口截断 + `artifact://` 落盘（**L6 工具结果 = Context=Index**）。
- `runtime.Bash`：`os/exec` 子进程 + 非交互 env 硬化 + cwd 持久化。
- `permission.Resolve`：纯函数审批（Tier × Mode）。
- `Executor.ExecuteAll`：Shared 并行（goroutine + Semaphore），Exclusive 串行。
- grep（`regexp` RE2 + PCRE 兜底 + 字面量 fallback）。
- 分层 Context 架构（L0-L7）正式写入 spec。

---

### P4.5 · 权限审批 HITL（interrupt/resume）

**目标**：把「拒绝降级」升级成「人机交互」。

**关键设计**：
- `tool.Approver` 接口 = 阻塞回调（中断点）；agent 循环在 `Prompt` 时阻塞等待决定。
- **interrupt/resume**：暂停 → TUI 弹窗 → 人决定 → 经 channel 回传 → 恢复执行。
- 无 approver 时退化为「拒绝+说明」（P4 行为）。
- 复用基础：后续 subagent/advisor 会复用这套 interrupt 机制。

---

### P5 · Structured Memory（多信号召回）

**目标**：跨会话记住偏好/事实。

**关键设计**：
- SQLite（`modernc.org/sqlite` 纯 Go）+ `working_memory` 表 + FTS5 索引。
- **FTS5 用 trigram 分词**——默认 unicode61 不切中文，中文整句当一个 token，`偏好用` 匹配不到。
- **多信号召回是纯代码，不靠模型**：`bm25(FTS) + importance + recency(exp 半衰期) + veracity` 算术融合。
- 注入 `<memories>` 背景块，带声明「当前消息/工具结果优先」（**背景上下文，让位于活状态**）。
- **代码检索（P4 grep/LSP）vs 记忆检索（P5 语义）是两个独立子系统**。

---

### P6 · Subagent + MCP（Context Isolation）

**目标**：派子 agent 处理大任务，接入 MCP。

**关键设计**：
- `subagent.Manager`：子 agent = 新 `agent.New` + 独立 context（只传 `[prompt]`，不传父历史），headless（yolo）。
- **Context Isolation**：父只拿结构化结论，不拿子 agent 中间过程。
- `Registry.Without("task")` 防递归。
- MCP：`mark3labs/mcp-go`（stdio），工具归一 `mcp__<server>_<tool>` 进同一 registry，每 server 故障隔离。

---

### P6-L1 · 委派策略（多 agent 主动委派）

**目标**：解决「主 agent 不主动委派」（实测：有 task 工具+指令，模型仍自己探索）。

**关键设计**（参照 oh-my-pi eagerTasks）：
- `delegation_mode` 配置（conservative/preferred/always）。
- `SubagentSpec.WhenToUse`（触发场景）+ task 描述动态枚举子 agent。
- coordinator prompt 三块（角色 / 触发清单 / 反例 Don't-peek/race/duplicate）。
- **orchestrator/worker 能力边界**（always 模式硬保证）：主 agent 只挂 task/remember，worker 挂 read/write/glob/grep/bash——「不委派 = 无法完成」。

---

### P6-L2 · 派发运行时（批量并行 + yield）

**目标**：把委派从「单任务、无状态」升级成「批量并行、结构化产出、状态机」。

**关键设计**：
- `SubagentSpec` 扩展（OutputSchema/Timeout/MaxTurns）+ `Result`/`Status`。
- `task` 工具改 `tasks[]` 批量（一次多个 = 并行）。
- `RunMany` + Semaphore（并发上限）。
- **yield 显式终止**：子 agent 调 `yield {data}` 结束，Manager 拦截提取结构化产出；outputSchema 基本校验。
- 状态机 pending/running/completed/failed/killed；failure control（timeout + MaxTurns + 取消）。

---

### P7 · Trace + Eval（审计闭环）

**目标**：让 JSONL 同时是审计追踪和评测输入。

**关键设计**：
- session 加 usage 持久化（`Entry.Usage` + `AppendWithUsage`）——**JSONL 即 trace**。
- `internal/trace.Analyze`：扫 JSONL 聚合 turns/toolCalls/tokens。
- `internal/eval`：任务夹具（`prompt.md` + `input/` + `expected/`）+ `os.Chdir` 隔离 + 字节级 verify（去末尾空白）。**不用 LLM judge**。
- `cmd/eval` 入口 + `evals/write-file` 示例夹具。

---

### P8 · 地基修正（M1：让三条不变量真正成立）

**缘起**：2026-08-24 的演进方案评审（`docs/specs/2026-08-24-evolution-plan.md`）发现每条不变量都只落实了"形"：压缩会切断 tool_call/tool_result 配对、yield 不终止子 agent、超时被报成 completed、artifact 从未落盘、会话与记忆全部相对进程 cwd。P8 先修缺口，再补能力。设计见 `docs/specs/phase-8-foundation-fixes.md`。

**已拍板决策**：headless 子 agent 遇到审批默认"拒绝并说明"，`subagent.approval_escalation: true` 才升级到父弹窗；默认 `approval_mode` 从 `yolo` 改为 `write`。

**关键设计**：
- `internal/paths`：数据根目录 `$CODECLAW_HOME` / `~/.codeclaw`，项目桶 `projects/<EncodeCWD(cwd)>/`（符号链接别名共桶），`GitRoot` 解析 worktree 主根，`ProjectID` 供记忆作用域。
- 配置三层合并（用户 → 项目 → 仓库内 legacy），新增 `subagent` 段。
- **Session v2**：条目带 `id/parentId/ts`，header 带 `cwd/title/parent`；leaf 指针 + 路径回放；`Compact(summary, firstKeptID)` 不再重追加保留段；`session_init`/`custom` 条目；回放时为悬空 tool_call 合成 `[interrupted]` 结果；v1 文件按线性链兼容；会话清单带标题/首句，`/resume` 支持前缀。
- **循环 v2**：`agent.Context` 成为真相源——每步 `Build()` 重建输入、循环内 `Record()`；mid-turn 压缩；溢出 → `RecoverOverflow`（保留量减半）与瞬时错误退避重试互斥；`tool.Terminal` 终止型工具。
- **压缩正确性**：估算覆盖全部块；切点只能落在 user 或无 tool_call 的 assistant；摘要输入含工具调用与截断后的结果。
- **产物**：`runtime.ArtifactStore`（会话产物目录、id 扫描分配）接进 Executor/Sink；`read_file` 支持 `artifact://N` 并改为按行读取。
- **子 agent**：`Options` 装配；yield 实现 `Terminal` 真正终止；状态 `timeout`/`aborted`；每 Run 独立 Bash/cwd；sidecar `agent-<name>-<hex>.jsonl`；权限继承父 mode（默认 `denyApprover`，可升级 `labeledApprover`）；结果含用量/耗时/转录路径。
- 模型层：`ModelStream` 接口（可 fake）；工具定义用完整 JSON Schema 透传（`task` 的 `tasks[].{subagent,prompt}` 对模型可见）；`IsContextOverflow`/`IsRetryable`。
- headless `codeclaw -p "<prompt>"`（`--yolo`、`--cwd`）用于自测与 CI。

**修复的缺陷**（对应演进方案诊断编号）：A1 yield 不终止、A2 超时报 completed、A4 子 agent yolo 绕过审批、A7 共享 Bash/cwd、B1 数据相对 cwd、B2 条目无 id、B3 悬空 tool_call、D1 切点拆配对、D2 估算漏工具块、D3 摘要看不到工具、D4 循环内无法压缩/无溢出恢复、D5 artifact 未落盘、E4 MCP 一律 read。

**学到**：把"真相源"放进循环而不是 UI，压缩/恢复才能在 turn 内生效；"终止"必须是循环级语义而不是工具返回值；子 agent 的隔离要落到 bash 实例与会话文件这种具体资源上，而不是 prompt 上。

### 扩展功能

| 功能 | 设计 |
|---|---|
| **多会话 + /resume** | `session.Manager`（`<id>.jsonl` + `sessions/current` 指针）+ `/new` `/sessions` `/resume` |
| **聊天列表虚拟滚动** | `scrollOffset` + 只渲染可见窗口 `[start,end)` + PgUp/PgDn + 鼠标滚轮（`MouseModeCellMotion`） |
| **steering 补充输入** | `RunSteering`（steer 通道非阻塞注入）+ Ctrl+E；注入后下一步模型调用生效 |
| **/forget** | `memory.Clear()`（清长期记忆，与 `/new` 清对话分离） |

---

## 3.9 P9（M2）委派运行时

> spec `docs/specs/phase-9-delegation-runtime.md`，plan `docs/plans/2026-08-24-phase-9-delegation-runtime.md`。
> 目标：把派发从「一次同步函数调用」变成**有契约、可观察、可干预、寿命受约束**的执行单元。

### P9.1 契约与发现

| 机制 | 做法 |
|---|---|
| EventBus | `internal/bus`：Publish 非阻塞（订阅者缓冲满即丢）、Subscribe 返回通道 + 幂等注销。总线只服务观测，真相源仍是 JSONL |
| agent 定义发现 | markdown frontmatter：项目 `.codeclaw/agents/*.md` → 用户 `~/.codeclaw/agents/*.md` → 内置（`go:embed`），同名 first-wins；坏文件只告警。单个字段格式错（如 `timeout: 十分钟`）只降级该字段，不废掉整个定义 |
| 派发契约 | `TaskBatch{context, tasks[], background}`；纯函数 `Preflight` 在起子进程**之前**拒绝：空 context / 一句话派发（<40 字符）/ 未知 agent / 深度超限 / spawn policy / 同名递归；运行名去重（parked 的名字也不复用） |
| 工具集裁剪 | `read_only` 裁到 read/glob/grep；`spawns` 非空且深度未满才给 `task`；另备一个「只含 yield」的注册表给强制收尾用 |

### P9.2 完成度协议

| 机制 | 做法 |
|---|---|
| 按调用判定终止 | `tool.Terminal` 改成 `IsTerminal(args, err)`，结果带 `Result.Terminal`——增量 yield 与「工具内退回重试」都不会误终止 run |
| yield 三态 | `data`=终止提交 / `data+section`=增量分段（不终止，收尾时按 schema 装配）/ `error`=主动放弃。线格式是扁平三参数，**不用顶层 anyOf**（strict provider 会拒掉整个工具定义） |
| schema | `data` 的线格式由 outputSchema **递归去 required** 派生（增量分段才提交得进来），真正校验在工具内：不符则带路径与剩余次数退回重试 ≤3 次，permissive 放行并告警、strict 判 `schema_violation` |
| turn 阶梯 | 每 turn 一个可单独取消的 ctx；turn 结束没 terminal yield → 注入提醒（≤3 次，最后一次把工具集换成只剩 yield）。阶梯耗尽：有 schema 判 failed，无 schema 判 completed 并标注 |
| 软预算 | 越界注入收尾通知 → 1.5× 停当前 turn 强制收尾 → 宽限 5 次请求仍不 yield 则 `killed`。定义里的预算只能比全局上限更小（`-1` 显式关闭） |

### P9.3 可观测

| 机制 | 做法 |
|---|---|
| 会话内 URL | `ArtifactStore` 成为路由表：`AddScheme` 注册 `agent://` / `history://`，`read_file` 见到任意 `<scheme>://` 都交给它——读回大内容对模型只有一个入口 |
| Run 名册 | 运行名 = hub 地址 = `agent://` 地址 = 作业 id；结束的 Run 留作 `parked`（可事后读产出与转录、可被唤醒）；名册外按产物目录回落解析（resume 后仍能读） |
| Agent Hub | TUI `ctrl+a`：表头聚合运行中/已结束/累计 tokens，行显示当前工具与用量；`j/k` 选择、`x` 终止、`esc` 关闭；`/agents` 打印名册、`/agent <名> <文本>` 发消息 |

### P9.4 异步与通信

| 机制 | 做法 |
|---|---|
| 后台作业 | `task{background:true}` 立刻返回作业 id；后台 Run 挂 **Manager 根 ctx**（父 turn 结束、用户 Esc 都不该带走它），`Shutdown` 统一收 |
| 恰好一次投递 | 结算进「待投递」队列；`TakeSettled` / `hub jobs` / `hub wait` 共用这一个队列——谁先看到谁消费，不会重复送 |
| async-result | 有活动 run → 走 steering 注入；空闲 → 自动起一轮继续（headless 最多 3 轮，防 CI 死循环） |
| hub 邮箱 | `list/send/inbox/wait/jobs/cancel`。send 对运行中的注入 steering、对已结束的**唤醒续跑**、对 Main 进主信箱；wait 只在完全没事可做时用 |
| parked → revive | 重开同一个 sidecar 续跑（转录里两轮都在），预算与提醒按这一次重新计，结果按后台作业回投 |
| 可 resize 并发闸 | 令牌通道 + 「债」计数：缩容不打断在跑的 Run，归还的令牌直接扣掉 |

### 实测（deepseek-v4-flash）

- 派两个 background 子 agent → `task` 立刻返回作业 id，主 agent 回「已派发」后 turn 就结束；两个作业结算后自动续跑并综合结果。
- `hub send` 追问已完成的 A → 唤醒续跑，A 先用 `hub send` 回了一句结论、再 yield 最终产出；同一个 sidecar 里有两条 `session_exit`。
- 子 agent 的工具集实测是 `glob/grep/hub/read_file/yield`（只读 agent 拿不到 bash/write_file）。
- 续跑那轮触发了 1 次 idle 提醒后才 yield —— 阶梯在真实模型上确实在兜底。

---

## 3.10 P10（M3）记忆与上下文

> spec `docs/specs/phase-10-memory-context.md`，plan `docs/plans/2026-08-24-phase-10-memory-context.md`。
> 目标：修掉「记忆召回静默失败」这个 crit、让提示词前缀在会话内稳定（prompt cache 不再每轮失效）、给上下文收缩加一级零成本手段（剪枝）、把项目知识沉淀成跨会话可复用的项目地图。

### P10.1 记忆正确性

| 机制 | 做法 |
|---|---|
| FTS 清洗 | 问句永不直传 `MATCH`：分词 → 丢弃 <3 字符词 → CJK 3 字滑窗 → 双引号包裹（内部 `"` 转义 `""`）→ OR 连接 ≤24 项；无可洗词走 importance×recency 兜底召回（不再静默失败） |
| Schema v2 + 迁移 | `memories`（scope/project_id/kind/key/why/veracity/importance/访问计数/superseded_by）+ FTS5 trigram；旧 `working_memory` 一次性幂等搬运、旧表保留 |
| 写入 | 有 key → upsert（保留 created_at/access_count）；无 key → trigram Jaccard ≥0.85 判近重复更新而非新增；密钥模式整条拒写；>2000 字符截断；每 scope 上限淘汰最低分 |
| 召回打分 | `0.45·fts + 0.2·importance + 0.15·recency + 0.1·log1p(access) + 0.1·scopeBoost` × veracity；召回后批量回写访问计数 |
| 双库 | 项目库 + 全局库经 `Union` 合并、按 content 去重、项目库 scopeBoost 靠前；`remember` 支持 scope、新增 `forget`（失效不删行） |

### P10.2 注入位置与项目指令层

| 机制 | 做法 |
|---|---|
| 前缀缓存 | `context.Manager` 缓存 system 前缀：只在会话首轮、压缩后、切会话时重算——连续 10 轮前缀字节一致，prompt cache 存活 |
| L1 项目层 | `internal/instructions`：用户级 AGENTS.md → git 根到 cwd 逐级（同级 AGENTS.md 优先于 CLAUDE.md，祖先先近者后）；`@import` 展开（相对导入者目录、`~` 展开、≤5 跳、防环、代码块与 git@/邮箱豁免、缺失原样保留）；`RULES.md` 同链收集、粘性渲染在块尾 |
| 注入顺序 | `[基础指令] [项目指令] [<memories>] [<project-map>] [<sticky-rules>]`；召回查询用最近 3 个用户 turn（`BuildRecallQuery`） |

### P10.3 上下文治理

| 机制 | 做法 |
|---|---|
| 剪枝纯函数 | `PlanPrune`/`ApplyPrune`：保护最近 40k token、单条 <50 token 不剪、至少省 20k 才动手；占位 `[输出已省略：约 N tokens]` 保留 `artifact://` 指针、永不拆 tool 配对、幂等（占位不再是候选） |
| 落盘与回放 | 剪枝边界落 `prune` 自定义条目（`{beforeEntryID, savings}`）；`Replay()` 回放期应用占位——JSONL 审计完整、前缀单调不 churn |
| 压缩阶梯 | `Compact` 先剪后摘：剪得够就返回 `prune`（零模型调用），剪无可剪才调摘要器；溢出恢复同阶梯 |
| `<files>` 树 | 摘要附文件活动树（Read/Write/RW、目录分组、上限 20）；压缩后追加 `<recent-files>` + auto-continue 用户消息 |

### P10.4 项目知识

| 机制 | 做法 |
|---|---|
| file_notes | `file_notes` 表（path/summary/symbols/mtime/size/hit_count）；explorer 子 agent 结构化产出（`files:[{path, role}]`）在结算时确定性 upsert，无需额外模型调用 |
| 项目地图 | 会话首轮注入 `<project-map>`（按目录分组、hit_count 排序、预算 1.5k token）；mtime/size 变了标「(可能已过时)」 |
| read 会话内去重 | `read_file` 记录 `path → {mtime+size 指纹, 已读行区间并集}`：未变更且区间已覆盖 → 返回「文件未变更（上次读过第 a–b 行），内容仍在上文中」；会话内 URL 不去重；`Registry.ResetConv()` 在 `/new` `/resume` `/clear` 时清空（`reset_boundary` 封存后旧上下文不再成立） |

### 实测与验收

- FTS：含 `?`/引号/括号的问句可召回；「构建命令」类问句在另一个项目里只召回 global 条（分库隔离）。
- 前缀：单测钉住「10 次 Build 前缀一致、system() 只算一次」。
- 剪枝：`TestMidTurnCompactionLadderSmoke`（真 session + 真 Manager + 脚本化模型）——阶梯 `[mid-turn:prune, mid-turn:summary]` 各一次、落盘 prune/compaction 条目、回放收缩且 tool 配对完整。
- read 去重：`TestReadFileDedupSmoke`（真循环 + 真 Builtins + 脚本化模型）——同一文件第二次 `read_file` 结果含「未变更」而非全文，且循环正常收尾；工具包 `-race` 全绿。

**学到**：记忆召回这类「静默失败」只有把错误显式上报 + 用纯函数把查询清洗与打分拆开，才能用单测钉住；「先剪后摘」把压缩拆成零模型调用的阶梯，能在不牺牲正确性的前提下大幅降低长会话成本；会话级状态（read 记录）的生命周期必须显式挂在会话边界（/new /resume /clear）上，否则「内容仍在上文中」会变成谎话。

---

## 4. 过程中发现并修复的 bug（17 个）

### 工具循环相关的 3 个（P4 埋坑，实测踩出）

1. **工具没传给模型**：P0 的 `Stream` 用 `_ []ToolSpec` 忽略工具 → 模型口头答「你可以用 ls」。修复：`toSchemaTools` + `WithTools`。
2. **流式工具参数按 CallID 误拆**：eino 流式按 `StreamingMeta.Index` 分组（CallID 只在首块出现），我按 CallID 分组把一次调用拆两个。修复：`ToolCallDelta.Index`。
3. **工具结果没持久化**：session 只记 user/assistant，缺 tool 结果 → 多轮 replay 报 `insufficient tool messages`。修复：`EventToolEnd` 时 `Append` 工具结果。

### reviewer 子 agent 发现的（多 agent 编排验证时白送）

4. **consumeStream 错误被吞**：`assistant, _ := consumeStream(...)` 不返回错误，流中途出错仍执行截断的工具调用。
5. **Sink 大输出 nil panic**：`NewSink` 从不 `SetArtifactDir`，大输出 `s.artifact.Write()` nil 解引用。
6. **TUI 双 run 竞态**：Enter 只 `currentCancel()` 不 join，新旧两轮并发写 session。修复：`runMu`。
7. **emit 取消丢事件**：`select ctx.Done()` 丢弃收尾事件，取消时 session 留下无回复的 user 消息。修复：`time.After` 超时。
8. **ExecuteAll 无并发上限**：无界扇出 goroutine。修复：Semaphore(8)。
9. **memory 两表非事务**：`working_memory` + `memory_fts` 两条 Exec。修复：包事务。
10. **审批弹窗残留**：ctx 取消后 `pendingApproval` 仍 modal。修复：Enter 清 `pendingApproval`。

### eval / 其它自测发现的

11. **eval 空指令 400**：`agent.New(..., "", ...)` 产生空 content system 消息被模型拒绝。
12. **eval 隔离失效**：`write_file`/`read_file` 用相对路径=进程 cwd，不是 workdir。修复：`os.Chdir(workdir)`。

（其余 reviewer 提的 lower 项——TUI 退出时序、Session.Close 加锁、error 包装 `%w`、goroutine 入口 recover——列为后续 hardening，未修。）

---

## 5. 最终架构

```
einoclaw-build/
├── cmd/
│   ├── agent/        # TUI 入口（装配 + 双协程）
│   └── eval/         # 评测入口
├── internal/
│   ├── message/      # 共享消息类型（零依赖）
│   ├── model/        # 【唯一 import eino 的包】模型客户端
│   ├── agent/        # 事件驱动循环 + AgentEvent + 累积器
│   ├── session/      # JSONL 会话 v2（树/分支/回放）+ Manager（多会话）
│   ├── context/      # 预算 + 六字段压缩 + 剪枝阶梯 + system 前缀缓存
│   ├── memory/       # SQLite v2 + FTS5 清洗召回 + file_notes
│   ├── instructions/ # L1 项目指令层（层级 + @import + RULES 粘性）
│   ├── tool/         # Tool 接口 + Registry + 执行器 + MCP
│   ├── runtime/      # OutputSink + ArtifactStore + bash 子进程 + env 硬化
│   ├── permission/   # 审批纯策略
│   ├── subagent/     # 子 agent 派发 + yield + 状态机 + hub 邮箱 + 名册
│   ├── bus/          # EventBus（观测通道，真相源仍是 JSONL）
│   ├── paths/        # 数据根目录 + 项目桶 + 配置分层
│   ├── trace/        # JSONL 聚合统计
│   ├── eval/         # 夹具 + 字节 verify
│   └── tui/          # BubbleTea TUI + 渲染 + 审批弹窗 + Agent Hub
├── docs/
│   ├── specs/        # 各阶段设计文档 + 总 spec + multi-agent spec
│   └── plans/        # 各阶段实现计划
└── evals/            # 评测夹具
```

依赖方向（单向、无环）：`cmd` → `agent` → `{model, session, context, memory, tool, permission, subagent}` → `message`；只有 `model` import eino。

---

## 6. 待办 / Stretch（未做，按需后置）

- **M4 治理与闭环（下一阶段，演进方案 §E/§F）**：审批规则引擎（allow/ask/deny）+ `Approval(args)` + bash 危险命令分类器 + 进程组/超时 + env 脱敏；`edit` 工具（read-before-edit + mtime 检查）；shell hooks；trace 派生索引 + `codeclaw stats/trace`；eval v2（并行 + pass@k + 不 os.Chdir）；fake model 回归套件扩展。可选 worktree 隔离。
- **P6-L3 通信+隔离**：worktree 隔离、审计 hook（mailbox bus 已在 P9 落地）。
- **P3 stretch**：多级摘要方法、snapcompact 成像（溢出恢复 + retry 双通道已在 P8 落地）。
- **P5 stretch**：episodic 巩固（sleep）、向量嵌入检索、Weibull 衰减、auto-retain（FTS 清洗/去重/失效已在 P10 落地）。
- **P6 stretch**：MCP http/sse（frontmatter 声明、spawn policy、软预算已在 P9 落地）。
- **hardening**：TUI 退出时序、Session.Close 加锁、error 包装 `%w`/哨兵错误、goroutine 入口 recover。
- **收尾**：状态栏、banner、斜杠命令框架、token 用量显示。

---

## 7. 关键收获（一句话总结每个阶段的核心）

| 阶段 | 一句话收获 |
|---|---|
| P0 | 类型词汇与会做 I/O 的客户端分离（`message` vs `model`），eino 关进一个包 |
| P1 | delta vs 定稿；为可测试抽出流消费核心 |
| P2 | 追加日志 + 可变指针 = 恢复/分支/压缩的统一机制 |
| P3 | provider usage 是真值；压缩是「面向未来任务的有损压缩」 |
| P4 | Tool 与 Runtime 分离；审批是纯函数；工具结果落盘 = Context=Index |
| P4.5 | interrupt/resume = 阻塞回调 + channel |
| P5 | 多信号召回是纯代码；代码检索 ≠ 记忆检索 |
| P6 | 子 agent 独立 context 只交还结论；防递归 |
| P6-L1 | 能力边界（orchestrator/worker）比 prompt 更硬地保证委派 |
| P6-L2 | yield 显式终止 + outputSchema + 状态机保证完成度 |
| P7 | JSONL 即 trace；评测用字节 diff 不用 LLM judge |
