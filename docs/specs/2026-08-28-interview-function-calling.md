# Function Calling 场景面试深挖：问题拆解 → 我的实际设计

> 日期：2026-08-28 · 用途：预演面试官沿 function calling 这条线追问时的应答
> 每个场景的结构：**面试官在考什么 → 通用解法地图（业界共识）→ 我的实际设计（含 `文件:行号`）→ 踩过的坑 → 追问预判**。
> 原则沿用 interview-qa：**能核实的说满，没做的直接承认并给排期**。

---

## 0. 项目背景与定位（开场怎么讲）

**一句话**：一个从零手写的编程智能体 harness（agent runtime + 会话 + 工具运行时 + 记忆 + 子 agent 编排 + 评测闭环），学习对象是 Claude Code 与 oh-my-pi，Go 实现，模型层只借 eino 做客户端适配。

**为什么做**（讲动机要落在技术判断上，不要只说"练手"）：原项目靠 eino 的 adk 全包办循环/会话/工具/权限，"会用但没自己写过"——所以把 eino 退到一个点（只做模型调用与流式适配），其余全部手写，每一层都要能回答"为什么这么设计"。

**规模与证据**（开场 30 秒可抛的数字）：

- Go 1.26，零新增运行时依赖（SQLite 用纯 Go 驱动 `modernc.org/sqlite`）
- 17 个包、**317 个测试函数**（含表驱动子用例更多），全量回归几秒跑完
- 11 个阶段（P0–P11.2），每个阶段以「能编译 + 能观察到一个行为 + 一次提交」收尾
- 会话 JSONL 即 trace 即 eval 输入，一份文件三用

---

## 1. 技术选型（每项都要能说出"替代方案是什么、为什么不要"）

| 选择 | 替代方案 | 理由 | 追问预判 |
|---|---|---|---|
| **Go** | Python/TS（oh-my-pi 是 TS） | 单二进制部署、goroutine 天然适合事件流与子 agent 并发；TS 生态更熟，但并发资源回收（进程组、信号）Go 更好落地 | "Go 写 agent 生态不如 Python 吧？"——答：依赖只有 1 个框架（eino 客户端），其余全是标准库，生态不是短板 |
| **eino 只做模型层** | LangChain 全包办 / 全自研客户端 | 全包办=黑盒（原项目教训）；全自研=每家 provider 的流式协议都要自己维护。折中：`internal/model` 是**唯一 import eino 的包**（`internal/model/eino.go:26,195`），业务代码只依赖自己的 `message.Message` | "换 provider 代价多大？"——答：改 `model.Config` 的 provider 字段即可（`main.go:193-198`）；错误分类靠特征串，provider 无关（`model/errors.go:6-18`） |
| **JSONL 追加式会话** | 内存态 + 定期快照 / SQLite 存会话 | 追加写 + 可变指针（leaf/compaction 指针）天然支持恢复/分支/压缩；会话文件=审计=trace=eval 输入，一份文件三用 | "随机回放不慢吗？"——答：session Open 时全量进内存镜像，Replay 是内存操作 |
| **SQLite + trigram FTS** | 向量检索 | 单机规模（每 scope ≤500 条）trigram + 多信号打分够用；向量要嵌入调用+索引维护，收益在"背景上下文让位于活状态"的定位下很低 | 见 interview-qa「记忆召回为什么不上向量检索」 |
| **BubbleTea TUI** | 无 UI（headless only） | 审批 HITL 需要弹窗；但 headless `-p` 模式与 TUI 完全同构（同一事件流），CI 可用 |
| **零新增依赖的自研 schema 校验** | jsonschema 库 | 校验器只服务一个目的：给模型一条能改对的反馈（`subagent/schema.go:10-16`），子集足够且零依赖 |

---

## 2. 工具选错怎么办（模型调用了一个不该用的工具/形态）

### 面试官在考什么

- 你是否理解 **function calling 本质上是"模型输出不可靠"的容错工程**，而不是"给模型配好工具就行"
- 你的防线是否分层：事前（不给机会选错）→ 事中（选错要有代价低的反馈）→ 事后（选错了不能造成破坏）

### 通用解法地图

1. **事前——能力边界硬约束**：不该给的工装就别给。Claude Code 的 orchestrator/worker 分工、oh-my-pi 的 `without` 工具裁剪都是这个思路。
2. **事前——描述即契约**：工具 description 写清"什么时候用、什么时候别用"（when-to-use）。
3. **事中——错误反馈闭环**：工具执行失败返回**可操作的错误文本**，模型下一步调用会看到并自行修正——这是 function calling 的核心纠错回路。
4. **事中——按参数审批**：工具选对了但参数危险（如 `bash` 的 `rm -rf`），在参数级拦一道。
5. **事后——副作用守卫**：写操作加前置校验（read-before-edit、mtime 检查），把"选错工具的破坏"变成"选错工具被拒绝"。

### 我的实际设计（四道防线）

| 防线 | 机制 | 位置 |
|---|---|---|
| 能力边界 | `always` 委派模式下主 agent **只挂只读工具 + task + remember**——写与执行必须派 worker，主 agent 物理上选不到 bash/write（"不委派 = 无法完成"） | `cmd/agent/main.go:309-323` |
| 能力边界 | 子 agent 工具集裁剪：`read_only` 裁到 read/glob/grep；深度到上限移除 `task` 防递归派发；强制收尾 turn 工具集**只剩 yield** | `internal/subagent/manager.go` `buildTools`、`driver.go:221-224` |
| 描述即契约 | `edit` 描述写明"先 read 后 edit、old_string 须逐字节唯一"；`remember` 描述写清"什么该记什么不该记"；`task` 描述按发现到的 agent 动态枚举（每个一段 when-to-use） | `internal/tool/tools.go:182-186`、`memory_tool.go:26-32`、`subagent/task.go:31-59` |
| 反馈闭环 | 选了个不存在的工具 → `"tool not found: <name>"`（IsError 落盘，模型可见）；artifact id 非法 → `"artifact id 必须是数字"`；未知 URL 方案 → `"不认识的 URL 方案，可用：…"`——错误文本全部带"怎么改" | `internal/tool/executor.go:75-78`、`internal/runtime/artifact.go:109-113` |
| 参数级审批 | bash 分类器按**调用参数**判定：`git status` 判只读免审批，`rm -rf`/`curl\|sh`/`sudo` 判危险强制询问（yolo 下也拦）——模型选对了工具但给了危险参数时在这里兜住 | `internal/tool/tools.go:290-300` + `internal/runtime/classify.go:13-20` |
| 副作用守卫 | edit 必须先 read（本会话指纹一致），mtime 变了拒绝并要求重读——选错工具（想改没读过的文件）在这里被 fail-safe 拒绝 | `internal/tool/fsguard.go:72-77`、`tools.go:214-217` |
| 委派预检 | `task` 派了个不存在的 agent / 一句话任务 / 深度超限 → `Preflight` **整批拒绝**（纯函数、不起子进程），错误文本直接还给模型 | `internal/subagent/preflight.go:36-98` |
| 完成协议 | yield 三态（data=终止 / data+section=增量 / error=放弃）——模型"用错形态"（比如该放弃时硬交 data）在工具内互斥校验（data 与 error 不能同给） | `internal/subagent/yield.go:154-156` |

### 踩过的坑（讲出来最有说服力）

1. **工具没传给模型**（DEVELOPMENT_LOG bug #1）：早期 `Stream` 忽略 `[]ToolSpec`，模型只能口头答"你可以用 ls"。教训：工具定义是否真的进了请求要有测试钉住。
2. **MCP 工具一律 TierRead**（演进方案 E4）：外部未知工具自动放行。已修：MCP 默认 write 档（`internal/tool/mcp.go:42`）——"未知副作用不能当只读放行"。
3. **委派模式实测模型不委派**（P6-L1 的缘起）：有 task 工具+指令，模型仍然自己探索——prompt 劝不动，最后用**能力边界硬约束**解决（always 模式拿掉写工具）。

### 追问预判

- "模型反复选错工具，靠错误反馈就能收敛吗？"——答：单次选错的纠错回路是反馈文本；反复选错的兜底是步数上限 `maxIterations=50`（`internal/agent/agent.go:41`）与子 agent 的提醒阶梯（`driver.go:247-256`）；再上层是评测——如果某模型在这个任务上选错率高，eval 数据会说话（见 §5）。
- "能力边界会不会让 agent 变笨？"——答：这是刻意的取舍。always 模式的代价是必须走委派（多一次 handoff），收益是"写与执行被 worker 的审批与转录隔离"；可配置（conservative/preferred/always 三档）。

---

## 3. 参数非法怎么办（缺字段、类型错、JSON 损坏、语义非法）

### 面试官在考什么

- 参数校验放在哪一层（声明层 / 运行时 / 语义层）的分工是否清楚
- 校验失败后的反馈是否**可操作**（actionable）——报"参数错"没用，报"缺 file_path"有用
- 是否有**重试协议**（feedback loop）而不是一次失败就判死

### 我的实际设计（三层校验，各管一段）

**第 1 层 · 声明层：JSON Schema 喂给模型（契约）**

- 工具 `Parameters()` 声明 properties + `Required()` 声明必填（`internal/tool/tool.go:11-36`），经 `Registry.Specs()`（`registry.go:92-113`）转成 provider 的 tool schema（`model/eino.go:134-171`）。
- 这一层是**给模型的契约**，不是给代码的校验器——描述质量直接决定参数质量。

**第 2 层 · 运行时解析：宽容 + 快速失败**

- `json.Unmarshal(call.Args)` 到 `map[string]any`；**非法 JSON 按空参处理**（`executor.go:80-83`）——模型手抖产生的坏 JSON 不该中断 run，工具会在首行报"xx 必填"，反馈回去让模型重发。
- 类型断言宽容（`argString` 等），数值按 JSON 形态断言 `float64`（`tools.go:98-104`）。

**第 3 层 · 语义校验：工具内首行 + 带指导的错误文本**

| 工具 | 语义校验 | 错误文本示范 |
|---|---|---|
| read_file | path 必填；URL 方案合法性 | `"file_path 必填"`、`"不认识的 URL 方案…"` |
| edit | old_string 必填且**逐字节唯一匹配**（出现 N 次要报告次数）；先读后改守卫 | `"old_string 出现 3 次（要求唯一）；请扩大上下文使其唯一，或设 replace_all=true"` |
| yield | data 与 error 互斥；section 名合法性；**schema 校验 ≤3 次退回重试**，超限 permissive 放行+警告 / strict 判失败 | `"产出不符合 schema：$.files 类型应为 array…修正形状后再调一次 yield（还剩 2 次机会）"` |
| task | 整批预检：空 context / 一句话任务 / 未知 agent / 深度超限 | 整批拒绝，一次性返回全部问题 |
| remember | 空 content；密钥模式**整条拒绝** | `"内容疑似包含 OpenAI 风格 key，拒绝写入记忆；记引用而不是值"` |

**yield 的 schema 校验是整套里最完整的参数校验闭环**（`internal/subagent/schema.go` + `yield.go:189-234`）：

- 自研 JSON Schema 子集校验器（type/required/properties/items/enum，递归，返回带路径的问题列表，`schema.go:21-77`）——只服务"给模型一条能改对的反馈"，有意忽略 `$ref`/`oneOf`/`pattern`（误报代价高于漏报）。
- **线格式放松**：`deriveDataSchema` 递归去 required、放开 additionalProperties（`schema.go:198-228`）——增量分段提交的是部分数据，线格式必须容得下；完整性由工具内用原 schema 校验。
- 重试预算：schema 不符 ≤3 次、空提交 ≤3 次（`yield.go:17-20`）；超限后 permissive 放行（带 warning 回父 agent）/ strict 判 `schema_violation` 失败——**两条路都终止，不无限循环**。

### 踩过的坑

1. **eval 空指令 400**（bug #11）：`agent.New` 传空 system 产生空 content 消息被模型拒绝——教训：边界值（空串）要在装配层显式处理。
2. **int vs float64 静默忽略**：read_file 的 offset 断言 `args["offset"].(float64)`，测试里传 `int` 会被**静默当成没传**（读全文件而不是越界）——生产路径（JSON 解码）永远是 float64 所以没炸，但这是"类型断言宽容"的双刃剑：宽容意味着静默，静默意味着难发现。现在靠"非法 JSON 按空参 + 工具首行必填报错"把静默面收窄。

### 追问预判

- "为什么不调工具前先做严格 schema 校验？"——答：严格拦截的失败信息对模型无用（它只知道"bad request"，不知道改哪）；把校验放工具首行，错误文本可以带上下文（"还差哪几个字段、还剩几次机会"），这是 feedback loop 而不是校验器。
- "坏 JSON 按空参处理，不会把 run 带沟里吗？"——答：空参会在工具首行触发必填错误，这个错误作为 tool result 落盘（`loop.go:103`），模型下一轮就看到了。失败路径总长度 = 一次工具往返，可控。

---

## 4. 调用失败怎么重试、怎么降级（错误分类 → 重试策略 → 降级阶梯）

### 面试官在考什么

- 错误是否**分类**（瞬时/永久/溢出）——分类错了重试就是烧钱
- 重试是否**有上限、有退避、不破坏上下文**（重试的请求不落盘、不重复计步）
- 永久失败是否有**降级路径**（不能只报错退出）
- **partial 是否保留**（失败的中间产物是否还能用）

### 我的实际设计：三条错误通道 + 一条降级原则

**① 模型层错误三分类**（`internal/model/errors.go:6-46`，`internal/agent/loop.go:126-153`）：

```
模型出错
├─ ctx 已取消           → 安静退出（不报错不重试）
├─ IsContextOverflow    → 溢出恢复通道：RecoverOverflow（保留量减半压缩），与重试互斥
├─ IsRetryable           → 指数退避重试 500ms·2^(n-1)，≤3 次，EventRetry 可见
└─ 其它                  → EventError，run 结束
```

- 分类靠**特征串**（provider 无关）：溢出 markers（"context_length_exceeded"…）、瞬时 markers（429/5xx/网络措辞）。`IsRetryable` 先排除溢出（`errors.go:35-37`）——溢出重试只会 400 循环。
- **重试不计步**（`loop.go:63-66` 的 `step--`）、**重试的失败请求不落盘**（assistant 消息只有成功消费完才 `Record`）——重试不污染会话、不消耗 MaxTurns。

**② 工具失败：错误即反馈**

- 工具 Execute 返回 error → `Result{IsError:true, Content: 原文 + [tool error: …]}`（`executor.go:133-136`）→ 作为 tool 消息落盘（`loop.go:100-106`）→ 模型下一步调用看到并自行修正。**这就是 function calling 的降级**：不是 harness 替模型决定怎么办，而是把失败信息完整交还给模型。
- bash 超时：进程组回收（SIGTERM→5s→SIGKILL）+ 明确错误"命令超时（120s），已终止进程组"（`runtime/bash.go:59-84`）——超时不是悬挂，是有确定终态。

**③ 子 agent 失败：状态机 + 四道闸**

- 终态判定链（`internal/subagent/driver.go:318-384`）优先级：父取消 → killed → 软预算未 yield → 超时 → runErr → yield(error) → strict 违约 → terminal → 阶梯耗尽。**每条路径都有归属**。
- 四道闸兜底（`driver.go:24-29` 常量）：MaxTurns、idle 提醒 ≤3（最后一次只剩 yield）、软预算三段式（通知→1.5× 停 turn→宽限 5 次硬杀）、wall-clock 超时。
- **partial 保留**：失败/超时的 Run 结算后变 parked，`Result` 带 `SessionFile`/`OutputFile`/`lastText`/`Sections`——父 agent 可以 `history://<name>` 追问细节，可以 `hub send` 唤醒续跑（`driver.go:183-194` `resetForRevive`）。

**④ 降级阶梯清单**（"坏了一块不崩全盘"是明确的装配原则）：

| 故障 | 降级行为 | 位置 |
|---|---|---|
| 项目/全局记忆库打不开 | 打印日志，**禁用记忆**继续跑 | `main.go:214-235` |
| 单个 MCP server 连不上 | 跳过该 server 的工具，其余照常 | `main.go:268-272` |
| 记忆召回失败 | 跳过记忆块，**不再静默**（日志） | `main.go:348-351` |
| 上下文超阈值 | 先剪枝（零模型调用）→ 不够再摘要 → 溢出再减半 → 只留最后一段 | `context/manager.go:166-176` |
| 摘要器不可用 | 剪枝照常可用（剪枝不需要模型） | `manager.go:215-217` |
| 子 agent 笔记沉淀失败 | 不阻塞结算（下次探索再沉淀） | `driver.go:386-408` |
| schema 反复不符 | permissive 放行+警告 / strict 判失败（都终止） | `yield.go:207-219` |
| headless 遇到审批 | 拒绝 + 说明怎么放行（--yolo） | `cmd/agent/headless.go:18-23` |

### 踩过的坑（重试相关的教训最贵）

1. **consumeStream 错误被吞**（bug #4）：`assistant, _ := consumeStream(...)` 不返回错误，流中途断掉仍执行**截断的工具调用**（参数可能只有一半）。教训：流错误和消息一样是返回值，绝不能 `_`。
2. **超时报 completed**（演进方案 A2）：ctx 超时后循环静默 break、不发 EventError——父 agent 把半成品当成功结果综合。修法是终态判定链 + 每条失败路径都落 `session_exit`（`driver.go:370-374`）。
3. **yield 不终止**（A1）：工具只回写 "done"，循环继续烧 MaxTurns。修法是"终止 = 循环级语义"（`tool.Terminal` 按调用判定，`loop.go:111-114`）。
4. **Sink 大输出 nil panic**（bug #5）：`SetArtifactDir` 从未接线，大输出落盘时 nil 解引用——降级路径自己就是 bug 源，所以降级路径也要有测试。

### 追问预判

- "重试为什么不加重试计数到 MaxTurns？"——答：MaxTurns 管的是**任务推进**（工具循环失控），瞬时网络错误与任务无关，混在一起会让一个网络抖动吃光任务预算。分开计数是刻意的。
- "瞬时错误的判定靠字符串匹配可靠吗？"——答：不完美但方向对。关键是**误分类的单向代价**：溢出被当瞬时→400 循环（所以溢出判定更宽且优先级最高）；瞬时被当致命→一次 run 提前结束（可接受，模型看到 error 后用户可重试）。权衡偏向"宁可少重试一次，不可 400 死循环"。

---

## 5. 如何评估 Agent 是否真实可用（这是最该准备好的问题）

### 面试官在考什么

- 是否区分**两层正确性**：模型层（任务完成度）与 harness 层（循环/压缩/委派行为正确）
- 评测是否**可复现**（LLM judge 的方差问题）、是否**可量化**（pass@k、成本、token）
- 是否有**失败路径**的评测（真模型测不了"永远不 yield"）

### 我的实际设计：三层评测金字塔

**第 1 层 · harness 行为回归（317 个测试函数，不调模型、不联网）**

- 脚本化 fake model：`fakeModel` 按"第 N 次调用"返回预设事件流，且能**断言这一次收到的工具定义列表**（`internal/agent/loop_test.go:39-63`）。
- 覆盖真实模型无法稳定复现的行为：第 3 次提醒时工具集只有 yield；schema 连错 3 次后的 permissive/strict 行为；`soft_budget: 4` 时第 6 次请求被掐断；任意 keepTokens 下压缩切出的消息序列合法（tool 配对不拆）；压缩阶梯 `[mid-turn:prune, mid-turn:summary]` 各一次；read 去重第二次返回"未变更"；deny 规则在 yolo 下拦截。
- **为什么这是核心**：harness 的 bug（压缩切点、yield 终止、超时状态）比模型答错更难复现也更贵——只能靠 fake model 把时序行为钉死。

**第 2 层 · 任务级 eval（模型真实参与，字节级可复现）**

- 夹具 = `prompt.md + input/ + expected/`（`evals/write-file`），`eval.Run` 在隔离 workdir 跑完整 agent，最后**字节 diff**（去末尾空白，`internal/eval/evaluator.go:41-120`）。
- 坚持字节 diff + verify 命令，**不用 LLM judge**：judge 自己有方差，还得为它再做一套评测来证明它靠谱——成本复利；字节 diff 覆盖"文件对不对"，verify 命令覆盖"能不能跑通"。
- 每次 run 记录完整 session（JSONL 即 trace），失败可回放。

**第 3 层 · 真实模型冒烟（每阶段收尾的仪式）**

- P9：派 2 个后台子 agent → 主 agent 立刻可答追问；hub send 唤醒已完成子 agent 续跑。
- P10：真实模型确认能读到 AGENTS.md/@import/RULES；压缩阶梯在真模型上兜底（idle 提醒确实触发）。
- P11：`git status` 免审批执行（第二次调用 `cached=2304`，前缀缓存命中）；`rm -rf` 被分类器拦截，模型正确报告未执行。

**可观测性（trace）**：`trace.Analyze` 从 JSONL 聚合 turns/toolCalls/tokens（`internal/trace/tracer.go:17-39`）；子 agent 落 `session_exit` 条目带 status/requests/reminders/yielded/schema 标记（`driver.go:370-374`）。M4 的 trace.db 派生索引 + `codeclaw stats` 在计划里（回答"钱花在哪、哪个子 agent 最慢"）。

### 诚实清单（说比被问好）

1. **eval 不能并行**：用 `os.Chdir` 隔离（`evaluator.go:48-52`），进程全局导致夹具串行——计划用 `ToolContext{CWD}` 替代（P11.5）。
2. **没有 pass@k**：当前单次跑，计划 `runs: N`。
3. **verify 目前只有字节 diff**，命令式 verify（`go build`/`go test` 任一失败即 fail）在 P11.5 计划里。
4. **"重复读取下降 ≥30%"这类量化验收有部分欠账**：机制已落地（read 去重、file_notes、工件指针），但跨会话的 read 次数对照测量还没做。
5. trace 只有 4 个计数，成本/耗时/按工具统计在 M4。

### 追问预判

- "你的 eval 能证明 harness 正确，但怎么证明模型在这个任务上可用？"——答：任务级 eval 用真实模型（第 2 层）+ pass@k 的 k 次重跑统计成功率；harness 回归与任务 eval 分离是刻意的——混在一起测，失败时分不清是模型笨还是循环错。
- "字节 diff 会不会太脆弱（换行/格式变化就挂）？"——答：verify 去末尾空白（`evaluator.go:111-118`），夹具侧重"语义文件"（代码/配置）；真正的行为验收靠 verify 命令（P11.5 计划）。

---

## 6. 讲踩坑过程的标准框架（面试官听的是方法论）

按这个四段式讲，每个坑 90 秒内：

1. **现象**（用户视角，一句话）："长会话聊着聊着突然报 400，重启就好。"
2. **定位**（体现排查思路）："报错是 provider 的通用 insufficient tool messages；回放 session 发现压缩切点落在 tool 消息上，把 tool_call/tool_result 拆开了。"
3. **修复**（体现工程判断）："不是加重试，而是把'安全切点'变成不变量——`safeCut` 只认 user 或无 tool_call 的 assistant 消息（`context/compaction.go:30-45`）。"
4. **防回归**（体现测试意识）："用 fake model 断言'任意 keepTokens 下切出的序列都合法'，钉死在回归套件里——这个 bug 真模型测不出来，因为它需要恰好切在工具调用中间。"

**最难 bug 排行榜**（讲 1-2 个就够）：压缩切点拆配对（400）、FTS 查询静默失败（"记忆功能看起来在，实际永远召回不到"）、超时子 agent 报 completed（父把半成品当成功）、共享 bash/cwd（并行子 agent 互相 `cd`）。

---

## 7. 一页速查（面试前 5 分钟过一遍）

| 追问 | 一句话答案 | 证据位置 |
|---|---|---|
| 工具选错 | 四道防线：能力边界（不给工具）→ 描述契约 → 错误反馈闭环 → 参数级审批 + 副作用守卫 | `main.go:309`、`executor.go:78`、`tools.go:290`、`fsguard.go:72` |
| 参数非法 | 三层：schema 声明喂模型 → 运行时宽容（坏 JSON 按空参+首行必填）→ 语义校验带"怎么改"；yield 有 ≤3 次重试协议 | `registry.go:92`、`executor.go:80`、`yield.go:189` |
| 失败重试 | 三分类互斥：溢出→压缩恢复、瞬时→退避 ≤3 不计步、致命→事件；工具失败=落盘反馈让模型自纠；子 agent 四道闸+状态机 | `model/errors.go:21`、`loop.go:126`、`driver.go:318` |
| 降级 | 原则"坏一块不崩全盘"：记忆禁用、MCP 跳过、剪枝→摘要阶梯、permissive 放行、partial 保留可 revive | `main.go:214`、`manager.go:166`、`driver.go:183` |
| 评估可用 | 三层金字塔：fake model 回归（317 测试）→ 任务 eval（字节 diff，不用 judge）→ 真模型冒烟；诚实承认并行/pass@k/verify 命令的欠账 | `loop_test.go:39`、`evaluator.go:41`、`tracer.go:17` |
| 最难 bug | 压缩切点拆 tool 配对 → 400；修法=安全切点不变量 + fake model 钉死 | `compaction.go:30` |
