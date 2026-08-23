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
│   ├── session/      # JSONL 会话 + Manager（多会话）
│   ├── context/      # 预算 + 六字段压缩
│   ├── memory/       # SQLite + FTS5 多信号召回
│   ├── tool/         # Tool 接口 + Registry + 执行器 + MCP
│   ├── runtime/      # OutputSink + bash 子进程 + env 硬化
│   ├── permission/   # 审批纯策略
│   ├── subagent/     # 子 agent 派发 + yield + 状态机
│   ├── trace/        # JSONL 聚合统计
│   ├── eval/         # 夹具 + 字节 verify
│   └── tui/          # BubbleTea TUI + 渲染 + 审批弹窗
├── docs/
│   ├── specs/        # 各阶段设计文档 + 总 spec + multi-agent spec
│   └── plans/        # 各阶段实现计划
└── evals/            # 评测夹具
```

依赖方向（单向、无环）：`cmd` → `agent` → `{model, session, context, memory, tool, permission, subagent}` → `message`；只有 `model` import eino。

---

## 6. 待办 / Stretch（未做，按需后置）

- **P6-L3 通信+隔离**：mailbox bus、worktree 隔离、session 持久化（多轮子 agent）、审计 hook。
- **P3 stretch**：响应式溢出恢复 + retry 双通道、多级摘要方法、snapcompact 成像。
- **P5 stretch**：episodic 巩固（sleep）、向量嵌入检索、Weibull 衰减、多声部 recall、auto-retain。
- **P6 stretch**：markdown frontmatter 声明、完整 spawn policy、run monitor（软预算）、MCP http/sse。
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
