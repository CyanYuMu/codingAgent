# codeclaw

一款在终端中运行的 AI 编程智能体（coding agent）——一个**从零手写**的 Agent Harness，用 Go 实现。

> 设计目标：把「用框架」变成「掌握框架」。核心机制（agent 循环、会话、上下文、记忆、工具运行时、权限、子 agent 编排、评测审计）全部手写，底层模型框架只退化为一个被隔离的「模型客户端」。

---

## 亮点设计（为什么值得看这份代码）

这个项目不是又一个「调 API 的壳」，而是对 AI 编程智能体**全部核心机制**的一次系统实现。以下设计经过了从 P0 到 P11 共 12 个阶段的演进打磨，每一条都对应真实踩过的坑：

### 1. 三条承重不变量（设计 DNA）

| 不变量 | 含义 | 解决的问题 |
|---|---|---|
| **事件驱动的三层循环** | run / turn / step 三层，流式增量（delta）与定稿消息分离 | 可观测、可中断、可测试，UI 与执行解耦 |
| **追加式 JSONL 是唯一真相源** | 会话转录 = trace = eval 输入，同一份文件 | 审计、恢复、回放、评测共用一套数据 |
| **Tool/Runtime 分离 + 纯函数审批** | Tool（面向模型的声明）≠ Runtime（进程引擎）；审批 = `Resolve(tier, mode)` 纯函数 | 无副作用、可单测、权限边界不被绕过 |

### 2. 三层循环：每步从真相源「重建」输入

`agent.Context` 接口把循环与上下文治理解耦：**每一步都从 session 重新 `Build()` 模型输入，不持有私有消息切片**。因为有三个写入方（steering 注入、mid-turn 压缩、工具结果记录）随时在改真相源——内存副本必然过期。这是 mid-turn 压缩能安全生效的前提。

### 3. 长上下文治理：先便宜后昂贵的三级阶梯

- **L6 剪枝（零模型调用）**：保护窗内保留最近 40k token 工具结果，旧结果替换为 `[Output truncated · artifact://N]` 占位，永不拆散 tool_call/tool_result 配对；
- **外置（shake）**：更早的大块结果写入 artifact 留指针；
- **摘要（模型调用）**：六字段任务导向摘要（目标/状态/决策/文件/失败/下一步），切点必须落在 user 消息或无工具调用的 assistant 消息——**绝不把「工具调用」与「它的结果」劈开**（否则 API 报 `insufficient tool messages`）。

触发点覆盖 turn 内：**mid-turn 压缩**（下一次模型调用前）修掉了「同一 turn 里 20 次工具调用撑爆窗口」；溢出与重试互斥——溢出不该走重试通道。

### 4. 多 Agent 编排：从「同步函数调用」到「有契约的执行单元」（最厚的一层）

主 agent 派发子 agent 不是「并行调几个函数」，而是**一组有契约、可观察、可干预、寿命受约束的执行单元**：

- **TaskBatch 契约 + 预检**：整批共享 Goal/Constraints/Contract；任务描述 < 40 字符**整批拒绝**（外包者看不到父历史，一句话派发只能靠猜）；深度上限、同名冲突、spawn 白名单逐项校验；
- **每人一份独立装备**：独立 sidecar 会话（可审计、可 revive）、独立 Bash/cwd（`cd` 不影响他人）、裁剪过的工具集（reviewer 只有只读工具）、**继承父审批而非 yolo**（派发 ≠ 放行）；
- **yield 三态完成协议**：`yield(data)` 终止 / `yield(data, section)` 增量提交 / `yield(error)` 主动放弃；产出按 outputSchema 校验，不合格退回重试 ≤3 次；
- **完成度五层保证**：契约 → 协议 → 驱动（idle 提醒 ≤3 次，第 3 次工具集只剩 yield；软预算 → 1.5× 强制收尾 → 宽限 5 次硬杀）→ 验证（completed ≠ 验收，须派 reviewer）→ 可恢复（失败也保留转录与部分产物，可 `hub send` 唤醒续跑）；
- **状态机如实记账**：`pending/running/idle/completed/failed/timeout/killed/aborted/parked`——超时就是 timeout，被杀就是 killed，**绝不把半成品报成 completed**；
- **后台作业 + hub 邮箱**：`background: true` 立即返回 job id，结果**恰好一次**投递回主会话；子 agent 间通过 `hub {list,send,inbox,wait,jobs,cancel}` 协调接口契约，消息以 `[hub from X]` 记入对方会话可审计；后台作业挂在 Manager 根 ctx（Esc 不会连带取消）；
- **可 resize 并发闸**：令牌通道 + 「债」计数，缩容不打断正在跑的任务。

### 5. 记忆：多信号召回，纯代码不靠模型

- SQLite + **FTS5 trigram 分词**（unicode61 不切中文，trigram 才能搜中文）；
- 召回是纯代码算术融合：`0.45·bm25 + 0.2·importance + 0.15·recency + 0.1·access + 0.1·scopeBoost`，recency 72h 半衰期；
- **FTS 查询清洗**：问句永不直传 `MATCH`（标点会语法报错且被静默吞掉）——分词、CJK 滑窗、双引号包裹、OR 连接；
- upsert 去重（key / 近重复 trigram Jaccard ≥0.85）、失效（superseded_by）、双库（global + project）；
- **注入位置固定 + 前缀缓存**：记忆块只在首轮与压缩后刷新——保持提示词前缀稳定，prompt cache 不失效；
- 项目知识层 `file_notes` + 项目地图注入，跨会话减少「从零 explore」的重复读取。

### 6. 多会话与项目隔离

数据落在 `~/.codeclaw/projects/<encoded-cwd>/`，按规范化 cwd 分桶（先 EvalSymlinks），**同一目录不同写法落同一桶，不同项目互不可见**——修掉「所有项目会话/记忆混在一个仓库目录」的根因。配置三层合并：用户 → 项目 → 仓库内 legacy。

### 7. 会话：追加日志 + 可变 leaf 指针

- 条目一旦写入不可变，可变状态只有 leaf；`reset_boundary` 封存旧上下文；fork 保留 id 链复制；
- **回放修复悬空 tool_call**：Ctrl+C 打断后留下「有调用没结果」，恢复时自动合成 `[interrupted: tool did not run]`——这是 `/resume` 首个请求不被 API 拒绝的原因（仅在内存修复，不落盘）。

### 8. 安全治理：审批规则引擎 + bash 分类器（P11）

- `permissions.{allow,ask,deny}` 规则语法 `tool(args*)` 通配，五步决策（工具 deny → 用户 deny → yolo 忽略裸 Override → 非 yolo Override 强制询问 → 工具显式 policy → 用户规则 → tier×mode），**deny 在 yolo 下也生效**；
- **bash 危险命令分类器**（纯函数）：按 shell 词法切段逐段保守判定——只读白名单（git status/go test）免审批；危险模式（`rm -rf`、`curl | sh`、`sudo`、fork bomb）**yolo 下也强制弹窗**；不认识的命令回落询问（漏判危险是事故，误判只读只是多一次审批）；
- bash **超时 + 进程组回收**：默认 120s，SIGTERM 进程组 → 5s 后 SIGKILL（只杀直接子进程留不住管道孙进程）；env 自动剔除密钥类（`*API_KEY*/*TOKEN*/*SECRET*`）。

### 9. 评测与审计闭环

- JSONL 即 trace；eval 夹具 `prompt.md + input/ + expected/`，**字节 diff 验证，坚持不用 LLM judge**；
- **最有价值的部分：脚本化 fake model 回归套件**——不联网、不花钱、几秒钟跑完，钉死 harness 自身行为（yield 三态、schema 重试、idle 阶梯、软预算三段、状态机、并发闸缩容、后台投递恰好一次）。harness 的正确性不依赖真实模型。

---

## 架构总览

```
einoclaw-build/
├── cmd/
│   ├── agent/        TUI / headless 入口（装配层：配置、工具工厂、Manager）
│   └── eval/         评测入口
└── internal/
    ├── message/      共享消息类型（零依赖：Role + ContentBlock 四类块）
    ├── model/        【唯一 import 模型框架的包】流式模型客户端 + 错误分类
    ├── agent/        事件驱动三层循环 + AgentEvent + 流累积器 + Context 接口
    ├── session/      追加式 JSONL 会话（entry 树/回放/压缩/fork）+ 多会话 Manager
    ├── context/      上下文真相源：Build/Record/ShouldCompact/Compact/RecoverOverflow
    ├── memory/       SQLite + FTS5 多信号召回 + file_notes 项目知识
    ├── instructions/ L1 项目指令层（AGENTS.md 层级 + @import + RULES 粘性）
    ├── tool/         Tool 接口 + Registry + Executor（审批+并发+截断）+ MCP 桥
    ├── runtime/      Sink（截断落盘）+ ArtifactStore（URL 路由）+ Bash（独立 cwd + 分类器）
    ├── permission/   纯函数审批策略（tier × mode × 规则引擎）
    ├── subagent/     委派运行时：发现/预检/驱动/yield/schema/名册/邮箱/后台作业/hub
    ├── bus/          进程内发布订阅（非阻塞，满即丢，只服务观测）
    ├── paths/        数据落点：$CODECLAW_HOME、按 cwd 分桶的项目目录
    ├── trace/        JSONL 聚合统计
    ├── eval/         夹具 + 字节 diff
    └── tui/          BubbleTea TUI + markdown 渲染 + 审批弹窗 + Agent Hub 面板
```

依赖方向单向无环：`cmd` → `agent`/`subagent` → `{model, session, context, memory, tool, permission, runtime, bus}` → `message`。

## 快速开始

```bash
# 配置（复制模板并填入模型 API key）
cp example.yaml config.yaml
# 示例模型：deepseek-v4-flash（见 config.yaml）

# 交互式 TUI
go run ./cmd/agent

# Headless（非交互，适合脚本/CI；等待后台作业最多 N 个）
go run ./cmd/agent -p "你的任务" --wait-jobs 3

# 评测
go run ./cmd/eval <eval-name>

# 跑测试（含 fake model 回归套件）
go test ./...
```

常用命令：`/new` 新会话 · `/sessions` 列会话 · `/resume <id>` 恢复 · `/agent <Name> <文本>` 唤醒 parked 子 agent · `ctrl+a` Agent Hub 面板 · `ctrl+e` steering 注入。

## 配置要点

```yaml
approval_mode: write        # write | always-ask | yolo（默认 write：read/write 放行、exec 询问）
delegation_mode: preferred  # conservative | preferred | always（always = 主 agent 只有只读 + task/remember）
permissions:
  allow: ["bash(git status*)", "bash(go test*)", "read(**)"]
  # deny:  ["bash(rm -rf *)", "read(./.env*)"]
bash:
  timeout: 120s             # 超时 SIGTERM 进程组、5s 后 SIGKILL；env 自动剔除密钥类
subagent:
  max_concurrency: 4        # 并行子 agent 上限（可运行时 resize）
  soft_budget: 200          # 模型请求软预算，0 = 关闭护栏
  max_recursion_depth: 2    # 委派递归深度上限
  min_task_chars: 40        # 任务描述最短长度（拒绝一句话派发）
memory:
  global: true              # 项目库之外再开全局库
  recall_top_k: 5
```

## 开发进度

| 里程碑 | 内容 | 状态 |
|---|---|---|
| P0–P7 | 地基 / 循环 / 会话 / 上下文 / 工具运行时 / HITL / 记忆 / 子 agent+MCP / trace+eval | ✅ |
| P8 (M1) | 项目作用域数据目录、Session v2、压缩正确性、子 agent 运行时修正 | ✅ |
| P9 (M2) | 委派运行时：frontmatter agent、TaskBatch 预检、yield 三态、Agent Hub、后台作业、hub 邮箱 | ✅ |
| P10 (M3) | 记忆正确性、前缀缓存、L1 项目层、剪枝阶梯、项目知识、read 去重 | ✅ |
| P11 (M4) | 审批规则引擎 + bash 分类器/超时/进程组/env 脱敏（P11.1）；edit / hooks / trace.db / eval v2（后续） | 🚧 P11.1 完成 |

详细演进方案见 [docs/specs/2026-08-24-evolution-plan.md](docs/specs/2026-08-24-evolution-plan.md)，完整开发记录见 [docs/DEVELOPMENT_LOG.md](docs/DEVELOPMENT_LOG.md)。
