# einoclaw-build Harness 重构 Spec

> 集成 oh-my-pi 核心设计，把 harness 从「eino 全包办」改造成「手写核心 + eino 只做模型调用」。
>
> 状态：**待评审** · 日期：2026-08-20 · 目标模块：`einoclaw-build`

---

## 1. 目标与背景

### 现状（阶段 0–5 已完成）

einoclaw-build 已经跑通了一个最小闭环：BubbleTea v2 TUI + 流式 Markdown（含增量安全边界算法）+ 工具调用/结果展示。但 harness 核心——**agent 循环、session、事件流、上下文管理、工具运行时、权限**——几乎全部由 eino 的 `adk.TurnLoop` / `NewTypedChatModelAgent` / `session.NewFileStore` / 中间件包办。我们对这些机制的理解停留在「会用」，没有「自己写过」。

### 目标

把 oh-my-pi 的核心设计手写集成进来，让 einoclaw-build 成为一个**真正自己掌握的 harness**：

1. **eino 退到一个点**：只保留 `components/model`（聊天模型 + 流式 + 工具调用）与 `schema` 消息类型，封装在 `internal/model` 之后；业务代码不 `import` eino。
2. **手写 harness 核心**：循环、session、上下文、记忆、工具运行时、权限、subagent、trace、eval。
3. **每阶段可验证**：每个阶段以「能编译 + 能观察到一个具体行为」收尾，不攒大块未验证代码。

### 学习目标

不是「抄一份 oh-my-pi」，而是**理解并复刻它的三条承重不变量**（见 §4），在 Go 里亲手实现每个机制，理解它解决什么问题、为什么这样设计。

---

## 2. 已锁定决策

| 决策 | 结论 |
|---|---|
| **eino 边界** | 只留模型客户端（`components/model` + `schema.Message`），循环/session/context/memory/tool-runtime/permission/subagent/trace 全部手写 |
| **包结构** | 一步拆成 `cmd/` + `internal/` 多包 |
| **参考架构** | 以用户给定的 go-agent 目录结构为骨架，增补 `internal/tui` 与 `internal/model` |

---

## 3. 目标架构

```
einoclaw-build/
├── cmd/
│   └── agent/
│       └── main.go            # 装配：读配置→建 model/session/tool→起 loop→起 TUI
│
├── internal/
│   ├── message/               # 共享消息类型(零依赖): Message/ContentBlock/Role/ToolCall/ToolResult
│   │   └── message.go
│   │
│   ├── model/                 # 【唯一 eino 依赖点】模型客户端薄封装
│   │   ├── model.go           #   Model 接口: Stream(ctx, msgs, tools) → 事件流+usage
│   │   └── eino.go            #   eino components/model 的适配实现(openai/qwen/ark/deepseek)
│   │
│   ├── agent/                 # agent 定义 + 事件驱动循环
│   │   ├── agent.go           #   Agent 定义(名字/指令/模型/工具集)
│   │   ├── loop.go            #   循环: turn/step/run + 三档中断
│   │   ├── event.go           #   AgentEvent 事件联合(见 §4-①); Message/ContentBlock 在 message 包
│   │   └── state.go           #   运行态(系统提示/模型/消息/待处理工具调用/错误)
│   │
│   ├── session/               # JSONL 追加式会话(单一真相源)
│   │   ├── session.go         #   Entry 类型 + 追加/重建
│   │   ├── entry.go           #   entry 判别: message/compaction/reset_boundary/...
│   │   └── storage.go         #   FileStorage 接口 + blob 外置(fork/clear)
│   │
│   ├── context/               # 长上下文治理
│   │   ├── manager.go         #   用量记账(锚点法)+ 预算/阈值
│   │   ├── tokenizer.go       #   本地 token 估算(只算锚点后的尾部)
│   │   └── compaction.go      #   增量压缩: firstKeptEntryId 边界 + 摘要注入
│   │
│   ├── memory/                # 结构化记忆
│   │   ├── memory.go          #   working/episodic 双级 + SQLite 存储
│   │   └── retrieval.go       #   多信号 recall(lexical+FTS+importance+recency+veracity)
│   │
│   ├── tool/                  # 工具抽象 + 注册表
│   │   ├── tool.go            #   Tool 接口(name/schema/approval/execute)
│   │   ├── registry.go        #   统一注册表(builtin + MCP 归一)
│   │   └── executor.go        #   并发调度(shared/exclusive) + 结果塑形
│   │
│   ├── runtime/               # 工具运行时(进程引擎)
│   │   ├── sandbox.go         #   子进程隔离 + env 硬化
│   │   ├── bash.go            #   bash 执行器(非 PTY + PTY)
│   │   └── sink.go            #   OutputSink 截断 + artifact 落盘
│   │
│   ├── permission/            # 审批策略
│   │   └── policy.go          #   纯函数 Resolve(tier,policy,user,mode) → Allow|Prompt|Deny
│   │
│   ├── subagent/              # 子 Agent
│   │   └── manager.go         #   声明(markdown frontmatter)+ 派发 + yield 协议
│   │
│   ├── trace/                 # 审计追踪
│   │   └── tracer.go          #   JSONL 即 trace + SQLite 派生索引
│   │
│   ├── eval/                  # 评测闭环
│   │   └── evaluator.go       #   任务夹具 + 字节级 verify
│   │
│   └── tui/                   # 终端 UI(复用现有，改造事件源)
│       ├── tui.go             #   迁移现有 teaModel/Update/View
│       ├── markdown.go        #   原样迁移(含 streamingMarkdown 增量)
│       └── events.go          #   AgentEvent → TUI 消息 适配器
│
└── config.go                  # 配置(迁移现有)
```

### 依赖方向（单向、无环）

```
cmd/agent ──▶ agent ──▶ {model, session, context, memory, tool, permission, subagent, tui}
                 │
tool ──▶ {runtime, permission}
agent ──▶ session ──▶ (无内部依赖，纯 stdlib + json)
context ──▶ {session, model}          # compaction 需要模型做摘要
memory ──▶ (SQLite, 独立)
tui ──▶ agent(event 类型)             # 只依赖事件类型，不依赖循环实现
trace/eval ──▶ {session}
```

规则：**只有 `internal/model` 可以 import eino**；其余包 import 要么是 stdlib，要么是 UI/存储/校验等非 eino 库。

---

## 4. 三条承重不变量（贯穿所有阶段）

这三条是从 oh-my-pi 提炼出来的「设计 DNA」，每个阶段都在实现它们的某一块，也是验收时的判据。

### ① 事件驱动的循环，不是同步 for 循环

- 词汇：**step** = 一次 LLM 调用；**turn** = 一次模型回复 + 它的全部工具调用/结果；**run** = 一次循环执行。
- 循环内部所有中间态都以**事件**吐出（`agent_start` / `turn_start` / `message_start` / `message_update` / `message_end` / `tool_execution_start` / `tool_execution_update` / `tool_execution_end` / `turn_end` / `agent_end`）。
- **三档中断**：硬杀（只杀可中断的等待）、软信号（让后台化工具让位）、跳过（还没开始的工具）。**绝不硬杀一个已产生副作用的工具**。

### ② 追加式 JSONL 日志是唯一真相源

- session 转录 = trace = eval 输入，**同一份文件**。
- 每行一个 entry（`message` / `tool_result` / `compaction` / `reset_boundary` / …），追加写 + 一个可变 `leaf` 指针。
- 上下文重建 = 重放日志到 leaf；compaction 是「替换而非追加」；大块内容 `blob:sha256:<hash>` 外置。

### ③ Tool 与 Tool Runtime 分离 + Approval 是纯策略

- `Tool`（面向模型的入口：schema/校验/审批/结果塑形）≠ `Runtime/Executor`（进程引擎：子进程/超时/取消/流式）。
- 审批 = `Resolve(tier, toolPolicy, userPolicy, mode) → Allow|Prompt|Deny` 的**纯函数 + mode 枚举**，不做有状态会话记忆；「记住决定」只靠持久化 config。

---

## 4.5 分层 Context 架构（贯穿所有阶段的顶层设计）

核心原则：**Context = Index，不是 Everything**——模型上下文里放「索引 + 精华」，原始数据存在别处（文件/磁盘/记忆库），需要时按指针取回。七层，各自生命周期不同：

| 层 | 内容 | 生命周期 | 落点阶段 |
|---|---|---|---|
| **L0 System** | 系统提示词 + 工具定义 | 静态 | P0/P1/P4 |
| **L1 Project** | 约定文件(AGENTS.md/CLAUDE.md) + workspace 骨架 | 会话启动加载 | P3 增补 |
| **L2 Task** | 当前任务 + 用户消息 | 静态 | P1/P2 |
| **L3 Recent Conversation** | 近期对话 | 增长→压缩 | P2 session |
| **L4 Working Memory** | 六字段压缩摘要 | 每次压缩替换 | P3 compaction |
| **L5 Retrieved Memory** | 召回的长期记忆 | 按需召回 | P5 |
| **L6 Tool Results** | 截断/落盘的工具结果 | 索引式 | P4 OutputSink |
| **L7 Long-term Memory** | 持久记忆 | 持久+召回 | P5 |

三条关键架构决策（调研 oh-my-pi 得出）：

1. **代码检索 ≠ 记忆检索，拆两个独立子系统**：grep/LSP/AST（on-demand 工具 → transcript）与语义向量检索（memory 后端 → `<memories>` 块）分开。别把符号级代码事实误当「可压缩的记忆」——这是 P4（代码检索）与 P5（记忆检索）的边界。
2. **结构化检索靠 LSP，不建依赖图**：跨文件符号关系全靠 LSP `definition`/`references`/`workspace-symbol`。oh-my-pi 没有依赖图/git blame——这是 Go 生态可补强处（`go list -deps`/`go mod graph` 包依赖图 + `go-git blame`）。
3. **工具结果压缩 + 落盘 = Context=Index 的实现**：`OutputSink` 头尾窗口 + artifact 指针 + 结果整形（分组/分页/`useless` 标记）。

## 5. 现状去留

| 现有文件 | 处置 |
|---|---|
| `markdown.go` | **保留**，迁入 `internal/tui/markdown.go`（含 streamingMarkdown 增量渲染，全量复用） |
| `tui.go` | **保留改造**，迁入 `internal/tui/`；`teaModel`/Update/View/渲染函数不动，只把消息来源从 eino 的 `OnAgentEvents` 换成自有 `AgentEvent` 适配 |
| `config.go` / `example.yaml` | **保留**，配置扩展出 handlers/approval/memory 等段 |
| `model.go` | **重写**，收编进 `internal/model`，变成 `Model` 接口 + eino 适配 |
| `main.go` | **重写**，拆成 `cmd/agent/main.go`（装配）+ 循环/session 逻辑下放到 internal |
| `handlers.go` | **删除**，filesystem 中间件由 `internal/tool` + `internal/runtime` 自建工具取代 |
| `messages.go` | **迁移**，TUI 消息类型迁入 `internal/tui`，工具调用/结果类型迁入 `internal/agent/event.go` |
| `sessions/` | **替换**，由 `internal/session` 的 JSONL 格式取代 eino 的 `.evlog` |

---

## 6. 阶段计划

**每阶段工作流**（贯穿全程）：

1. **写该阶段的详细设计文档** —— `docs/specs/phase-N-<topic>.md`，参照 oh-my-pi 对应机制逐条讲清「问题 → 数据模型 → 接口 → 算法 → 边界/错误处理」，并给出 Go 落点。
2. **用户评审设计文档** —— 确认后再动手。
3. **开发** —— 按设计文档一步步写，每步可编译。
4. **验证** —— 对照该阶段的「验收」行为确认。

每个阶段末尾都要求：`env -u GOROOT go build ./...` 与 `go vet ./...` 通过，且有一个**可观察的验收行为**。阶段顺序即依赖顺序。各阶段内部再拆成原子子步骤，细节见 writing-plans 计划与对应阶段设计文档。

### P0 · 地基：拆包 + 核心类型 + 模型封装

**目标**：把现有代码拆进多包骨架，定义核心接口类型，把 eino 关进 `internal/model`。

**产出**：
- 建立 `cmd/agent` + 全部 `internal/` 空包骨架。
- `internal/agent/event.go`：定义 `AgentEvent` 事件联合 + `Message`/`ContentBlock`（text/thinking/toolCall/toolResult 四种块）。
- `internal/model/model.go`：`Model` 接口 —— `Stream(ctx, msgs []Message, tools []ToolSpec, opts) (*StreamResult, error)`，`StreamResult` 含事件流 + `Usage`。
- `internal/model/eino.go`：用现有 agenticopenai/qwen/ark/deepseek 实现 `Model`（迁移 `model.go` 的 provider switch）。

**关键机制（学到）**：接口把「我们依赖什么」和「eino 怎么实现」隔开——业务包只看见 `Model`，eino 只存在于 `internal/model`。

**验收**：多包编译通过；`go run ./cmd/agent` 能启动 TUI 骨架（此时还没接新循环，可先用一段临时代码证明 `Model.Stream` 能吐字）。

---

### P1 · Agent Loop：手写事件驱动循环

**目标**：用自写循环替换 eino `TurnLoop`，实现三档中断，把流式事件接进 TUI。

**产出**：
- `internal/agent/state.go`：运行态（系统提示/模型/消息/待处理工具调用/错误）。
- `internal/agent/loop.go`：`Run(ctx, input) → <-chan AgentEvent`。step=一次模型调用，turn=回复+工具调用/结果，循环直到无工具调用。
- `internal/tui/events.go`：`AgentEvent → TUI 消息`（aiTextChunk/aiThinking/toolCall/toolResult）适配器。

**关键机制（学到）**：
- 流式模型输出 → 累积 chunk → 合并出完整工具调用（复用已有 `ConcatAgenticMessages` 思路）。
- 三档中断：`context.Context` 硬取消 + 软信号 channel + 待启动工具跳过。

**验收**：单轮问答流式渲染到 TUI（复用 markdown 增量渲染）；Ctrl+C 能立刻停。

---

### P2 · Session + Event：JSONL 单一真相源

**目标**：用自写 JSONL 会话替换 eino `session.NewFileStore`。

**产出**：
- `internal/session/entry.go`：entry 判别（message/tool_result/compaction/reset_boundary/…）。
- `internal/session/session.go`：追加写（`bufio.Writer` + `json.Encoder`）+ `ReplayToLeaf()` 重建消息序列。
- `internal/session/storage.go`：`Storage` 接口（File/Memory）+ `blob:` 内容寻址外置 + `/clear`（写 `reset_boundary`）+ `/fork`（共享日志 + 换 leaf）。

**关键机制（学到）**：为什么「追加 + leaf 指针」能同时支持恢复/分支/压缩——日志不可变，可变态只有一个指针。

**验收**：退出重启后历史恢复；`/clear` 封存旧上下文不再注入。

---

### P3 · Context Engineering：预算 + 增量压缩 + 双通道恢复

**目标**：长对话不再溢出，自动摘要压缩。

**产出**：
- `internal/context/tokenizer.go`：本地 token 估算（复用 provider 的 `usage` 作真值 + 只估算锚点后的尾部）。
- `internal/context/manager.go`：预算模型 `threshold = window − max(0.15·window, reserve)`；`shouldCompact = contextTokens > threshold`。
- `internal/context/compaction.go`：增量压缩——从新到旧找切点，**绝不切在 toolResult**，旧段做**六字段任务导向摘要**（目标/状态/决策/文件/失败/下一步，保留「继续任务所需信息」而非泛化总结）+ 保留段原样；摘要注入为一条 `compaction` entry。

**关键机制（学到）**：`firstKeptEntryId` 边界是「摘要替换而非删除」的关键；retry 与 overflow 是两条互斥恢复通道（可重试错误→指数退避重试，溢出→压缩），绝不重试溢出的请求。

**验收**：人为拉长对话触发自动压缩，可见「摘要块 + 最近消息」结构，不再报上下文溢出。

---

### P4 · Tool Runtime + Permission：从「调用函数」到「运行时」

**目标**：自建工具注册表 + bash 子进程运行时 + 审批策略，替换 eino filesystem 中间件。

**产出**：
- `internal/tool/tool.go`：`Tool` 接口（name/schema/approval/execute）。
- `internal/tool/registry.go`：统一注册表；内置工具（read/write/edit/glob/grep/bash/ls）。
- `internal/tool/search.go`：结构化代码检索——grep（`regexp` RE2 + `regexp2` PCRE 兜底 + 字面量 fallback）+ LSP（gopls `definition`/`references`/`workspace-symbol`）+ AST（`go/parser`/`go/ast`/`go/types`）。
- `internal/tool/cache.go`：目录清单缓存（短 TTL + 写后失效，只缓存枚举不缓存内容）。
- `internal/tool/executor.go`：并发调度（shared/exclusive 两级）+ 结果塑形。
- `internal/runtime/bash.go` + `sandbox.go`：`os/exec` 子进程 + 非交互 env 硬化（`PAGER=cat`/`TERM=dumb`/`NO_COLOR=1`）+ 超时/取消。
- `internal/runtime/sink.go`：`OutputSink` 头尾窗口截断 + `artifact://` 落盘。
- `internal/permission/policy.go`：纯函数审批 + mode 枚举（always-ask/write/yolo）。

**关键机制（学到）**：Tool 与 Runtime 分离——同一批工具运行时引擎可替换；审批是纯策略；**工具结果压缩 + 落盘（OutputSink + artifact）就是「Context=Index」的 L6 实现**；代码检索（grep/LSP/AST）是 on-demand 工具返回进 transcript，与 P5 记忆检索分离。

**验收**：agent 能读/写文件、跑 bash；`rm -rf /` 之类被拒（返回「需要审批」）；工具结果超长自动截断并落 artifact。

---

### P4.5 · 权限审批 Human-in-the-loop：interrupt/resume + 审批弹窗

**目标**：把审批从「纯策略」升级成「人机交互」——`Prompt` 时暂停 turn、弹窗、人决定、恢复。

**产出**：
- `internal/agent`：interrupt/resume 机制（暂停 turn → 等待决定 → 恢复继续工具循环）。
- `internal/permission/policy.go`：`DecisionPrompt` 触发 interrupt（而非降级拒绝）。
- `internal/tui`：审批弹窗（允许/拒绝 + 显示命令/参数）。

**关键机制（学到）**：interrupt/resume 是「暂停/恢复运行中 turn」的通用机制，后续 subagent/advisor 也会复用；HITL 审批只是它的第一个应用。

**验收**：危险命令弹窗 → 点允许继续执行 → 点拒绝工具被拒；点允许后 agent 继续后续工具循环。

---

### P5 · Structured Memory：working→episodic + 多信号 recall

**目标**：跨会话记住偏好与事实。

**产出**：
- `internal/memory/memory.go`：SQLite（`modernc.org/sqlite`，纯 Go 无 CGO）`working_memory`/`episodic_memory` 双级 + FTS5 索引。
- `internal/memory/retrieval.go`：多信号打分 recall（lexical + FTS + importance + recency + veracity），注入为 `<memories>` 背景块。

**关键机制（学到）**：记忆是**背景上下文、让位于活状态**；model 写的事实与 harness 写的转录用 `source`/`veracity` 区分；召回是打分排序而非单一匹配；**记忆检索（L5/L7 语义向量）与 P4 代码检索（grep/LSP/AST）是两个独立子系统**——别把符号级代码事实当「可压缩的记忆」。

**验收**：跨会话问「我之前偏好 X 吗」能召回；记忆注入后 agent 记得偏好但不被旧记忆误导。

---

### P6 · Subagent + MCP：分解任务 + 接入外部工具

**目标**：派生子 agent 处理大任务，接入 MCP server 工具。

**产出**：
- `internal/subagent/manager.go`：markdown frontmatter 声明 + 派发（递归深度门控、借用父 tool registry）+ yield 协议（产出 = 保留的 `yield` 工具调用）。
- `internal/tool` 增补：MCP 客户端（`github.com/mark3labs/mcp-go`，已在 module cache）把 `mcp__<server>_<tool>` 归一进 registry。

**关键机制（学到）**：子 agent 是「同一循环跑在独立 session + 借用父资源」；**Context Isolation——子 agent 有自己独立的 context/tools/task，只把结构化产出（几 KB）交还父 agent，而非复制父的整个 context**；MCP 是「多一个工具源」，250ms 启动门让慢 server 不阻塞。

**验收**：`task` 工具能派子 agent 并收到结构化产出；接一个 MCP server 后其工具可被调用。

**多 agent 主动委派 + 并行编排升级**（详见 [multi-agent-orchestration.md](multi-agent-orchestration.md)，分三层按杠杆落地）：
- **P6-L1 委派策略**：`delegation_mode` 配置（conservative/preferred/always）+ coordinator 角色 + 触发清单/反例 + orchestrator/worker 能力边界 + whenToUse。
- **P6-L2 派发运行时**：统一 `SubagentSpec` + `tasks[]` 批量派发 + Semaphore 并发控制 + `yield`/outputSchema 完成度保证 + 状态机 + failure control。
- **P6-L3 通信+隔离**（延后）：mailbox bus + worktree 隔离 + session 持久化 + 审计 hook。

---

### P7 · Trace + Eval：审计闭环

**目标**：让 JSONL 同时是审计追踪和评测输入，形成「跑 → 测 → 改」闭环。

**产出**：
- `internal/trace/tracer.go`：确认 session JSONL 即 trace；增量解析灌 SQLite 派生索引（token/cost/duration 查询）。
- `internal/eval/evaluator.go`：任务夹具（prompt + input + expected）+ 字节级 verify（格式化后 diff，无 LLM judge）；输出 `pass/fail/error` trace。
- `evals/` 目录放 fixture。

**关键机制（学到）**：对开放式 agent 任务，评分用「字节精确 diff」或「任务自带 reward 函数」，不用 LLM judge；离线反事实回放（重放 read 调用推最优配置）是闭环的落点。

**验收**：跑一个 fixture 出 pass/fail + 用量报告；`omp stats` 式用量审计可查。

---

## 7. 关键技术决策（Go 库选型）

| 关注点 | 选择 | 理由 |
|---|---|---|
| 模型客户端 | eino `components/model` + agentic 包装（现有） | 唯一 eino 依赖点，流式+工具调用已成熟 |
| 消息类型 | **自建** `internal/agent.Message/ContentBlock`，`internal/model` 处转换到 eino `schema` | 业务包不 import eino，边界最干净 |
| 存储(SQLite) | `modernc.org/sqlite`（纯 Go） | 无 CGO，跨平台；FTS5 可用 |
| JSON schema 校验 | `github.com/santhosh-tekuri/jsonschema/v5`（新增） | 工具入参/子 agent 产出校验 |
| 子进程/PTY | `os/exec` + `github.com/creack/pty`（新增） | 标准做法 |
| MCP 客户端 | `github.com/mark3labs/mcp-go`（已在 module cache） | 免自写协议层 |
| TUI | 继续 BubbleTea v2 + glamour + lipgloss | 已跑通，复用 |
| 配置 | `gopkg.in/yaml.v3`（现有） | 不变 |

---

## 8. 范围与分期（core vs stretch）

**Core（本 spec 覆盖，逐阶段做）**：P0–P7 全部。

**Stretch（后置/可选，不进主线阶段，需要时再起新 spec）**：

- snapcompact（对话渲染成 PNG 帧压缩）——视觉模型依赖，学习价值低。
- polyphonic 4-voice recall / Weibull 衰减 / 贝叶斯 veracity 真值维护——先做多信号 recall 的单 voice，其余按需。
- advisor/watchdog（独立评审模型）——P6 之后可加。
- worktree 隔离（subagent 用 go-git worktree）——P6 后可选。
- 进程内 extension 插件系统（Go 需 WASM 或子进程协议）——先用「注册表 hooks」轻量替代，完整版后置。
- 反事实回放优化器（read_optimizer）——P7 的可选深化。

---

## 9. 验收总则

- 每个阶段：`env -u GOROOT go build ./...` + `go vet ./...` 零错误。
- 每个阶段：至少一个**可观察**的行为验收（见各阶段「验收」）。
- 不攒大块未验证代码：一个机制做完、能跑、能看到，再进下一个。

---

## 10. 待定（进入实现前需在相应阶段敲定）

1. **消息类型边界**（已定）：`internal/agent.Message` 为**自建 struct**，含 `[]ContentBlock`（四种块 text/thinking/toolCall/toolResult）；`internal/model` 处转换为 eino `schema`。
2. **模型 client 具体形态**（已定）：**下沉到 `components/model` 自组流式**——`internal/model` 直接调用 eino 的 `model.ChatModel.Stream()`，自己组装 text/thinking/tool-call 增量与 usage，**不依赖** agentic 包装层。
3. **审批与 TUI 的交互**：审批弹窗走 BubbleTea 的哪个消息通道（阻塞等待 vs 事件往返）？——P4 敲定。
