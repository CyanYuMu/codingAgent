# codeclaw 核心职责 → 代码位置 → 问答

> 用途：把简历上的四条核心职责落到可指认的代码，并预演会被追问的问题。
> 原则：**能核实的说满，没做的直接承认并给排期**——被问出来比主动说出来伤害大得多。

---

## 一、Agent Harness 架构设计

### 代码位置

| 能力点 | 位置 |
|---|---|
| 三层循环（run/turn/step） | `internal/agent/loop.go:16` `Run` / `:36` `loop` |
| 真相源抽象（解耦循环与上下文治理） | `internal/agent/agent.go:15` `Context` 接口 |
| 流式增量 → 定稿消息累积 | `internal/agent/state.go:23` `add` / `:49` `message` |
| 事件契约（13 种） | `internal/agent/event.go:13` |
| 模型接入（唯一 import eino 的包） | `internal/model/eino.go:26` `Stream` / `:195` `New` |
| 错误分类（溢出 / 可重试） | `internal/model/errors.go:21` `:35` |
| 工具定义生成 | `internal/tool/registry.go:65` `Specs` |
| 运行工件与 URL 路由 | `internal/runtime/artifact.go:76` `AddScheme` / `:86` `Resolve` |
| 装配层（一切粘合处） | `cmd/agent/main.go:147` `main` / `:208` `workerTools` 工厂 |

### 问答

**Q：每一步都从 session 重建输入，不浪费吗？直接在内存里维护一个 msgs 切片不好吗？**

不行，会错。有三个写入方在改真相源：steering 注入（`loop.go:41`）、turn 内压缩（`loop.go:49`）、工具结果记录（`loop.go:102`）。内存副本一旦存在就会过期——最典型的是 mid-turn 压缩：压缩改的是 session，如果循环还拿着旧切片，这次压缩等于白做，下一次请求照样撑爆。

代价其实很小：session 在 `Open` 时把条目加载成内存镜像（`session.go:21`），`Replay` 是在镜像上沿 parentId 回溯 + 展开，不读文件。相对一次网络模型调用，这点开销可以忽略。

**Q：为什么"是否终止 run"要按本次调用判定，而不是按工具类型？**

因为同一个工具的不同调用语义不同。`yield` 带 `section` 是增量提交（不终止），不带才是终止提交（`yield.go:139` `IsTerminal`）。顺带修掉了另一个隐患：如果按注册表里的工具类型静态判定，注册表里同名工具被替换后判定会失真。所以 `Result` 带 `Terminal` 字段（`executor.go:32`），循环直接读（`loop.go:106`）。

**Q：事件 emit 为什么有 1 秒超时、允许丢事件？**

`loop.go:20-26`。持久化已经在循环内完成了（`cc.Record`），事件只服务渲染。如果不加超时，TUI 卡一下就会**阻塞整个 agent 循环**。用超时把"渲染慢"和"执行正确性"解耦：最坏情况是界面少显示一帧，执行链路不受影响。

**Q：为什么把 eino 关在一个包里？这是过度设计吗？**

不是，是为了可测。`internal/model` 对外只暴露 `Stream(msgs, tools) → ModelStream + Usage`，业务代码只依赖自己的 `message.Message`。带来两个直接收益：换 provider 或换框架只改一个包；**harness 的 231 个测试可以用脚本化 fake model 跑，完全不 import eino、不联网**。如果 eino 的类型渗透进 agent/tool/session，这些测试就得起 mock server。

---

## 二、多 Agent 委派、并行与完成度保障

### 代码位置

| 能力点 | 位置 |
|---|---|
| 派发前纯函数预检 | `internal/subagent/preflight.go:36` `Preflight` |
| agent 定义三层发现（项目/用户/内置） | `discovery.go:28` `Discover` / `:174` `ParseAgentFile` / `:95` `Bundled` |
| 每 Run 运行时装配 | `manager.go:350` `setup` / `:394` `buildRuntime` / `:426` `buildTools` |
| 独立 bash 实例（消除共享 cwd） | `cmd/agent/main.go:208` 工厂 + `runtime/bash.go:19` |
| 审批继承（不是 yolo） | `manager.go:404` + `subagent/approver.go:11` `:20` |
| 可 resize 并发闸 | `jobs.go:19` `gate` / `:55` `setLimit` |
| 同步批次（结果按输入序回填） | `manager.go:304` `RunBatch` |
| 后台作业（挂 Manager 根 ctx） | `jobs.go:114` `StartBackground` |
| 投递恰好一次 | `jobs.go:173` `Jobs` / `:199` `TakeSettled` / `hub.go:125` `settledAmong` |
| 三通道通信 | 结果 `task.go:181`；事件 `bus/bus.go:31` + `driver.go:32`；邮箱 `mailbox.go` + `manager.go:175` `Deliver` |
| yield 三态 | `yield.go:146` `Execute` |
| Schema 校验器与派生 | `schema.go:21` `Validate` / `deriveDataSchema` |
| 驱动阶梯 + 软预算 | `driver.go:198` `drive` / `:298` `checkBudget` |
| 终态判定链 | `driver.go:331` |
| 唤醒续跑 | `jobs.go:241` `Revive` / `driver.go:183` `resetForRevive` |

### 问答

**Q：预检为什么是整批拒绝，而不是跳过有问题的那一项？**

两个理由。一是**回滚成本**：已经启动的子 agent 可能已经改了文件，半个批次跑起来比整批打回难收拾得多。二是**错误文本是给模型看的**——一次性返回"第 2 项任务描述太短，必须写 Target/Change/Acceptance"，比逐项报错更容易让它一次改对。所以 `Preflight` 是纯函数、无副作用，在起任何 goroutine 之前跑完。

**Q：40 字符的门槛是拍脑袋定的吗？会不会误伤正常任务？**

它不是长度检查，是"强制写清 Target/Change/Acceptance"的最便宜代理。选它是因为**误伤成本远低于漏放成本**：误伤了，错误文本直接告诉模型要写哪三段，重发一次就好；漏放了，子 agent 从空白上下文开始只能猜，整个 Run 的算力都浪费了。而且是可配置项 `min_task_chars`。

**Q：后台作业为什么挂 Manager 的根 ctx，而不是 task 工具调用的 ctx？**

`jobs.go:113` 注释写了原因：挂调用 ctx 的话，父 turn 一结束（或者用户按一下 Esc 停当前 turn），后台作业就被连带取消，"后台"就是假的。代价是必须显式回收——`main.go:282` 用 `defer mgr.Shutdown(5s)` 取消根 ctx 并等所有 Run 退出，超时会报还剩几个没退。

**Q："恰好一次投递"怎么保证？有三个消费者会不会打架？**

一个 `pending` 队列 + 一把锁。`TakeSettled()` 全部取走并清空；`Jobs()` 内部先调 `TakeSettled` 再渲染（**模型在 `hub jobs` 里看到结果就算已投递**，不会再单独送一遍）；`hub wait` 的 `settledAmong` 只挑关注的那几个，其余 `putBack` 放回队列留给正常通道。三条路径都在 `m.mu` 下操作同一个 `pending`，所以不会重复也不会丢。

**Q：强制子 agent 收尾，为什么不用 provider 的 tool_choice？**

两个原因。一是 **provider 无关**：不同后端对 tool_choice 的支持差异大，deepseek/qwen 上行为不一致。二是**可测**：做法是现场用"只含 yield 的注册表"构造一个 agent（`driver.go:222-225`），fake model 能直接断言"这一 turn 收到的工具定义只有 yield"；如果用 tool_choice，这条行为只能靠真实 provider 验证。这个决策记在 `phase-9` 详设的决策表第 1 条。

**Q：子 agent 死活不 yield，你凭什么保证它一定会结束？**

四道闸同时兜底，任何一条触发都会走到确定终态：

1. `MaxTurns`——单个 turn 内工具循环上限
2. idle 阶梯——turn 结束没提交就提醒，≤3 次，第 3 次工具集只剩 yield
3. 软预算三段式——越界通知 → 1.5× 掐断当前 turn → 宽限 5 次请求硬杀
4. wall-clock timeout——`runCtx` 上的超时

最后 `settle` 的判定链（`driver.go:331`）按优先级排：父取消 → killed → 预算未 yield → 超时 → runErr → yield error → strict 违约 → terminal → 阶梯耗尽。**每条路径都有归属，不存在"没人管"的状态**。

**Q：Schema 校验为什么自己写一个子集，不用现成库？**

`schema.go:10-16` 写了取舍。这个校验器只服务一个目的：**给模型一条能自己改对的反馈**。所以支持 type/required/properties/items/enum，故意忽略 `$ref`/`oneOf`/`pattern`/`min-max`——这些在自定义 agent 里罕见，而且误报代价高于漏报（默认 permissive 模式下漏报只是少一次提醒，误报会让模型在格式上空转三轮）。附带好处是零依赖、纯函数、可以被 fake model 测试直接调。

**Q：父 agent 为什么不订阅事件总线？看不到子 agent 在干嘛不是更好？**

看得到的是**人**（TUI Agent Hub），不是父 agent。如果父订阅原始事件，子 agent 的每一次工具调用和结果都会进父的上下文——委派的全部意义（省父的上下文）就没了。所以设计上明确：父只消费结果通道（结构化产出 + 工件指针），事件总线只服务 TUI 与 Manager 记账。

---

## 三、长上下文治理与结构化记忆

### 代码位置

| 能力点 | 位置 |
|---|---|
| 阈值与保留预算 | `context/manager.go:82` `threshold` / `:116` `keepBudget` |
| 估算比值校准 | `context/manager.go:138` `keepInEstimateUnits` |
| 全块类型估算 | `context/tokenizer.go:7` `EstimateTokens` |
| 安全切点 | `context/compaction.go:13` `findCutPoint` / `:30` `safeCut` |
| 六字段摘要指令 | `context/compaction.go:101` |
| 摘要输入完整序列化 | `context/compaction.go:53` `serializeConversation` |
| turn 内触发 | `agent/loop.go:49` |
| 溢出恢复（与重试互斥） | `agent/loop.go:123` → `context/manager.go:124` `RecoverOverflow` |
| 截断落盘 + 指针 | `runtime/sink.go:39` `Write` / `:68` `Result` |
| URL 读回入口 | `runtime/artifact.go:86` + `tool/tools.go:54` |
| 项目分桶 | `paths/paths.go:52` `EncodeCWD` / `:68` `ProjectDir` |
| 会话树与回放 | `session/replay.go:41` `buildContext` |
| 悬空 tool_call 修复 | `session/replay.go:88` `repairDangling` |
| 压缩条目（指针而非重追加） | `session/session.go:194` `Compact` |
| 记忆写入（两表事务） | `memory/memory.go:65` `Remember` |
| 多信号召回 | `memory/retrieval.go:14` `scoreMemory` / `:26` `Recall` |
| 记忆注入位置 | `cmd/agent/main.go:266` |

### 问答

**Q：为什么不用 tiktoken 精确算 token？**

真值来自 provider 返回的 usage（`manager.go:108` 直接看 `PromptTokens`），本地估算只用于两件事：找切点、审计。既然不是判断依据，就没必要引精确分词器——不同 provider 分词器不同，引一个也不准。

但粗估有个真实问题：按 rune/2 算，中文和代码会低估好几倍。所以做了**比值校准**（`manager.go:138`）：用"上次 prompt 真值 ÷ 本地估算总量"得到 ratio，把 provider token 预算换算成估算单位。ratio 夹在 0.25–8 之间，样本小于 2000 时不校准——小样本下这个比值是噪声。

**Q：切点一路往回退，最坏退到 0 怎么办？那不就压不了了？**

`findCutPoint` 返回 0 表示无可压内容，`compact` 返回 `(false, nil)`，这一次不压。但溢出通道会再试一次：`RecoverOverflow` 先把保留量减半重压，还不行就 `compact(ctx, 1)` 只留最后一段（`manager.go:124-131`）。所以有兜底，不会出现"撑爆了但压不动"的死角。

**Q：压缩之后，保留的那段消息为什么不重新写进文件？**

`session.go:193-198`：只写一条 compaction 条目 + `FirstKeptEntryID` 指针，回放时从指针位置展开（`replay.go:60-76`）。如果重新追加，同一段消息在文件里会出现两次——既浪费磁盘，又让 trace 统计翻倍（同一条 assistant 消息的 usage 被算两遍）。

**Q：记忆召回为什么不上向量检索？**

单机规模下 trigram FTS + 多信号打分够用，而向量要引依赖 + 嵌入调用 + 索引维护。更重要的是定位：**记忆是背景上下文，要让位于活状态**——注入块只有 5 条、明确标注"当前对话优先"，在这个体量上精度提升的边际收益很有限。方案里排在 M3 之后按需再做。

**Q：你说"减少重复读取"，具体是哪几个机制在起作用？怎么量化的？**

诚实答：目前生效的是三条——工件指针替代重跑（长输出不用重新执行命令）、`agent://` / `history://` 让父与其它子 agent 复用已完成 agent 的完整结论、六字段摘要里的"文件/产物"段让压缩后仍知道哪些文件看过。

**跨会话那一层还没做**：file_notes 项目地图（新会话注入"这个仓库长什么样"）、read 命中未变更文件先返回笔记、会话内 read 缓存，都在 M3 排期。所以现在还没有"重复读取从 N 降到 M"的量化数据——要拿这个数字，得先把 M3 做完再跑对照。

**Q：这套记忆现在有什么已知问题？**

两个，都在 M3 修：

1. **FTS 查询原文直传**（`retrieval.go:33`）：用户问句里的 `? ( ) " -` 会触发 SQLite 全文检索的语法错误，而 `main.go:271` 用 `err == nil` 把错误吞掉了——表现是"一条都搜不到"，且没有任何提示。修法是分词后每个 term 加引号转义、丢弃 <3 rune 的词（trigram 限制）、用 OR 连接，永不把原文交给 MATCH。
2. **每步都重新召回**：`system()` 在每次 `Build()` 被调用，导致发给模型的前缀每轮都在变，prompt cache 一直命中不了。修法是只在会话首轮和每次压缩后刷新，把召回块钉在 system prompt 的固定位置。

---

## 四、工具安全与评测审计闭环

### 代码位置

| 能力点 | 位置 |
|---|---|
| Tool / Runtime 分离 | `tool/tool.go:11` `Tool` 接口 / `:25` `Terminal` |
| 审批纯函数（3×3 决策表） | `permission/policy.go:31` `Resolve` |
| 执行器（查表→审批→执行→塑形） | `tool/executor.go:55` `Execute` |
| 并发分档调度 | `tool/tool.go:32` `Concurrency` + `executor.go:100` `ExecuteAll` |
| 可读的拒绝原因 | `tool/executor.go:26` `DenyReasoner` + `cmd/agent/headless.go:21` |
| 子 agent 审批策略 | `subagent/approver.go:11` `denyApprover` / `:20` `labeledApprover` |
| bash 实例隔离 + env 硬化 | `runtime/bash.go:19` `:38` + `runtime/sandbox.go` |
| 目录扫描缓存（写后失效） | `tool/cache.go:22` `:48` |
| 审计聚合 | `trace/tracer.go:17` `Analyze` |
| 评测夹具 + 字节 diff | `eval/evaluator.go:41` `Run` / `:111` `verify` |
| harness 行为回归 | `subagent/*_test.go`、`context/context_test.go`、`agent/loop_test.go` |

### 问答

**Q：审批为什么非要做成纯函数？**

因为它是安全边界，必须能穷举验证。`internal/permission` 整个包无副作用、无依赖，3 档等级 × 3 档模式 = 9 条决策可以逐条断言。真正难测的是"弹窗交互"，把它隔离在 `Approver` 接口后面——测执行路径时塞一个永远拒绝或永远同意的实现就行，不需要起 UI。

**Q：默认 `approval_mode: write` 意味着 `write_file` 不弹窗。这安全吗？**

诚实答：**现在不够。** `write_file` 是整文件覆盖、路径不限工作区，`bash` 也没有危险命令分类。当前的护栏只在 exec 这一层（bash 要审批）。

M4 的计划是补齐四件事：`edit` 工具 + read-before-write + mtime 冲突检查（防止模型凭记忆整文件重写把代码写没）、路径边界（工作区外的写强制询问）、bash 分类器（`rm -rf`、`curl | sh`、写 `/etc` 强制询问）、allow/deny/ask 规则引擎。在此之前，跑不受信任的任务应该显式用 `always-ask`。

**Q：为什么 bash 和 write_file 要串行，不能并行？**

共享可变状态。bash 的 cwd 存在实例里（`bash.go:14`），两个 bash 并发执行、其中一个 `cd`，另一个的工作目录就变了；write_file 可能写同一个文件。实现上（`executor.go:106`）是：遇到 Exclusive 工具先 `wg.Wait()` 等前面并行的那批完成，再串行执行——**保证结果仍按调用序回填**，同时没有数据竞争。

**Q：231 个测试怎么在不调模型的情况下测 agent 行为？**

脚本化 fake model：按"第 N 次调用"返回预设的事件流，并且能断言这一次收到的工具定义列表。所以能覆盖真实模型很难稳定复现的时序行为——

- 第 3 次提醒时，工具集里**只有** yield
- schema 连错 3 次后，permissive 放行 + 打 warning，strict 判 failed
- `soft_budget: 4` 时，第 4 次请求收到通知、第 6 次（1.5×）当前 turn 被掐断
- 任意 keepTokens 下，压缩切出来的消息序列都合法（tool 配对不拆）
- 并发闸缩容时不打断在跑的 Run
- 后台结果被 `Jobs()` 消费后，`TakeSettled()` 拿不到第二份

比 e2e 便宜三个数量级，而且**是唯一能覆盖失败路径的方式**——你没法让真实模型可靠地"永远不 yield"。

**Q：评测为什么坚持字节 diff，不用 LLM judge？**

可复现。LLM judge 自己有方差，还得为它单独做一套评测来证明它靠谱，成本是复利的。字节 diff 覆盖"文件对不对"，配合 verify 命令覆盖"能不能跑通"，两者组合足够。代价是夹具设计更费劲——但夹具是一次性成本，judge 的不确定性是每次都要付的。

---

## 五、两道综合题

**Q：这个项目里最难的一个 bug 是什么？**

压缩切点落在 tool 消息上，导致下一次请求被 API 拒（400 insufficient tool messages）。

难在三点：只在"长会话 + 恰好切在工具调用中间"时出现；报错是 provider 的通用消息，看不出根因；表现是"聊着聊着突然报错"，重启就好，很容易被当成偶发。

修法的关键不是加重试，而是**把"安全切点"变成不变量**：`safeCut` 只认用户消息或无 tool_call 的 assistant 消息，候选切点一路向前回退直到满足（`compaction.go:23-26`）。然后用 fake model 断言"任意 keepTokens 下切出的序列都合法"，把它钉死在回归里。

**Q：如果重做一遍，哪里会不一样？**

三处：

1. **工具的危险等级应该按参数判定，而不是固定一档。** 现在 `Tier()` 是工具级常量，导致 MCP 工具一律按 read 处理——外部未知工具自动放行是个洞。应该一开始就是 `Approval(args) → {tier, policy, reason}`。
2. **记忆的 scope/key 应该一开始就进表结构。** 现在靠目录分桶解决了跨项目串味，但没有 `key` 就做不了 upsert，同一条偏好 remember 三次会占三个 topK 名额。事后加列比一开始设计贵。
3. **评测隔离不该用 `os.Chdir`。** 它是进程全局的（`eval/evaluator.go:49`），直接导致夹具不能并行跑。应该让工具通过 `ToolContext{CWD}` 解析相对路径。
