# 工具体系全解：定义 → 注册 → 暴露 → 调用 → 参数校验（含面试叙述稿）

> 日期：2026-08-28 · 用途：把「所有工具如何定义和调用、参数怎么校验」讲成一条能指认到代码的完整链路，后半部分是可直接照着讲的面试叙述稿。

---

# 第一部分：代码链路全景（精确到行号）

## 1. 工具全家福：一共 12 个工具，三种来源

| 工具 | 定义处 | 装配处 | Tier | 并发 | 说明 |
|---|---|---|---|---|---|
| `read_file` | `internal/tool/tools.go:30-122` | `Builtins` | read | Shared | 按行读 + 会话内 URL + 去重 |
| `write_file` | `tools.go:126-165` | `Builtins` | write | Exclusive | 覆盖写 + 写后更新守卫指纹 |
| `edit` | `tools.go:171-244` | `Builtins` | write | Exclusive | 先读后改 + 逐字节唯一替换 |
| `glob` | `tools.go:248-271` | `Builtins` | read | Shared | 文件名匹配 |
| `grep` | `internal/tool/search.go:58-87` | `Builtins` | read | Shared | 正则搜内容，坏正则降级字面量 |
| `bash` | `tools.go:275-308` + `runtime/bash.go:45-82` | `Builtins` | exec（按参数动态） | Exclusive | 独立实例 + 分类器 + 超时进程组 |
| `remember` | `internal/tool/memory_tool.go:14-87` | `main.go:264-267` | write | Shared | 写事实记忆 |
| `forget` | `memory_tool.go:90-137` | `main.go:264-267` | write | Shared | 记忆失效 |
| `mcp__*` | `internal/tool/mcp.go:27-93` | `main.go:268-272` | write（默认） | Shared | 外部 MCP 工具归一，60s 超时 |
| `task` | `internal/subagent/task.go:16-138` | `main.go:322` / `manager.go:468-470` | write | Shared | 批量派子 agent |
| `hub` | `internal/subagent/hub.go:30-63` | `main.go:323` / `manager.go:462` | read | Shared | 名册/邮箱/作业协调 |
| `yield` | `internal/subagent/yield.go:67-131` | `manager.go:460-461` | read | Exclusive | 子 agent 三态结果提交 |

`Builtins` 是内置 6 件的出厂装配：`internal/tool/tools.go:16-26`——read/write/edit 共享同一个 `fileGuard`（会话级文件状态，`fsguard.go:13-16`）。

## 2. 链路一：定义——`Tool` 接口 + 5 个可选接口

**主接口**（`internal/tool/tool.go:11-20`）：

```go
type Tool interface {
	Name() string                                  // 唯一名字（MCP 归一为 mcp__<server>_<tool>）
	Description() string                           // 给模型的"什么时候用、怎么用"
	Parameters() map[string]any                    // JSON Schema properties（给模型的参数契约）
	Tier() permission.Tier                         // 基线危险等级 read/write/exec
	Concurrency() Concurrency                      // Shared 可并行 / Exclusive 必须串行
	Execute(ctx, args map[string]any, sink *runtime.Sink) error
}
```

**5 个可选接口**（同一文件 `tool.go:22-36`，按需实现）：

| 接口 | 作用 | 谁实现 |
|---|---|---|
| `RequiredParams` | `Required() []string` 进工具定义的 required | 几乎所有 |
| `Terminal` | `IsTerminal(args, err)`——本次调用是否终止 run，**按调用判定** | `yield` |
| `Decisioner` | `Decision(args) ToolDecision`——按参数自检审批 | `bash` |
| `ConvState` | `ResetConv()` 换会话清状态 | `read_file`（清 fileGuard） |
| `ReadHistoryInvalidator` | `InvalidateReadHistory()` 压缩后清已读区间 | `read_file` |

`ToolDecision`（`permission/rules.go:20-27`）= `{Tier, Policy(allow|deny|prompt), Override, Reason}`——固定 Tier 的扩展形态。

## 3. 链路二：注册——从工厂到 registry

```
Builtins(bash, store) ──┐
remember/forget ────────┤→ workerTools(cwd, store) 工厂（main.go:259-274）
MCP 工具 ───────────────┘       │ 每次调用返回一套新工具（新 bash 实例）
                                ▼
      主 agent：mainRegistry（main.go:309-327）
      ├─ delegation=always：只留 read 工具 + remember
      ├─ 其它模式：全套 + task(depth=0) + hub
      └─ exec.SetRules(rules)
      子 agent：buildTools（subagent/manager.go:436-475）
      ├─ 默认集 → def.Tools 白名单裁剪 → ReadOnly 裁到 read/glob/grep
      ├─ +yield +hub
      └─ spawns 非空且深度未满才 +task
```

`Registry`（`internal/tool/registry.go:11-46`）是 map + 读写锁；`Register` 同名覆盖（后注册者胜）；`Without(name)` 供防递归裁剪。

## 4. 链路三：暴露给模型——从 map 到 provider 的 tool schema

```
Registry.Specs()（registry.go:92-113）
  按名字排序 → model.ToolSpec{Name, Description, Parameters, Required}
  → einoModel.Stream 里 cmodel.WithTools(toSchemaTools(tools))（model/eino.go:26-36）
  → toolJSONSchema 把 map 序列化成 JSON Schema（eino.go:151）
  → 随请求发给 provider
```

两个要点：**按名字排序**保证工具定义顺序稳定（prompt 前缀稳定的前提之一）；`task` 的 Description 是**动态的**——运行时枚举发现到的子 agent 清单（`task.go:31-58`），所以"能用哪些子 agent"天然进了模型上下文。

## 5. 链路四：调用——从流式增量到结果落盘

```
① 模型流式返回 FunctionToolCall 块（含 StreamingMeta.Index）
   → Stream.Recv 转成 ToolCallDelta（model/eino.go:56-67）
② streamAccumulator.add 按 Index 合并（agent/state.go:23-45）
   ——CallID 只在首个分块出现，Args 是多个分块拼接
③ consumeStream 产出定稿 assistant 消息 → cc.Record 落盘（loop.go:69-86）
④ toolCallsOf 提取调用（loop.go:183-190）→ EventToolStart
⑤ ctx 已取消则跳过（三档中断之"跳过"，loop.go:92-94）
⑥ executor.ExecuteAll(calls)（loop.go:98）
   ├─ Shared：goroutine 并行，Semaphore 上限 8（executor.go:47,139-163）
   ├─ Exclusive：先 wg.Wait() 等并行批次完成再串行（executor.go:146-149）
   └─ 结果按调用序回填
⑦ 每条结果 → EventToolEnd + NewToolMessage 落盘（loop.go:100-106）
⑧ Result.Terminal → EventTerminated 结束 run（loop.go:111-114）
```

`Execute` 单次调用的七步（`internal/tool/executor.go:75-137`）：

```
Get(name) 查表 → 未找到：Result{"tool not found: <name>", IsError}
→ json.Unmarshal(call.Args)（坏 JSON 按空参）
→ Decisioner 自检（可选）→ ToolDecision
→ ResolveRules 五步决策 → allow / deny / prompt
→ Sink(4000,4000) + SetArtifactStore
→ t.Execute(ctx, args, sink)
→ Terminal 判定 + 错误塑形 → Result{Content, IsError, Terminal}
```

## 6. 链路五：参数校验——四层，各管一段

| 层 | 位置 | 管什么 | 失败表现 |
|---|---|---|---|
| **声明层** | `Parameters()` + `Required()` → `Specs()`（registry.go:92-113）→ provider schema | 给模型的契约（不是代码校验器） | 模型生成参数的质量 |
| **协议层** | `json.Unmarshal`（executor.go:80-83） | 坏 JSON 不崩 run | 按空参处理 → 工具首行报必填 |
| **解析层** | 各工具首行类型断言 + 必填检查（tools.go:74-76、151-153、204-206、304-306） | 字段缺/空/类型错 | 返回带"怎么改"的错误文本 |
| **语义层** | edit 的 old 唯一性（tools.go:223-229）；yield 的 data/error 互斥（yield.go:154-156）；task 的 Preflight（preflight.go:36-98） | 业务约束 | 同上，且 yield 有 ≤3 次重试协议 |

**yield 的 schema 闭环**（参数校验的完整形态，`subagent/schema.go` + `yield.go:189-234`）：

```
outputSchema（agent 定义里的契约）
  → deriveDataSchema 递归去 required、放开 additionalProperties（线格式，schema.go:198-228）
  → 模型提交 data → Validate(原 schema)（schema.go:21-77，type/required/properties/items/enum）
  → 不符：带路径 + 剩余次数退回（"$.files 类型应为 array，还剩 2 次机会"）
  → ≤3 次；超限：permissive 放行+警告 / strict 判 schema_violation（都终止）
```

**task 的 Preflight**（参数校验在"起子进程之前"的范例，`preflight.go:36-98`）：空 context / 一句话任务（<40 字符）/ 未知 agent / 深度超限 / 同名递归——纯函数整批拒绝，错误文本一次性还给模型。

---

# 第二部分：讲给面试官听的叙述稿

> 下面这段是第一人称口语稿，你可以直接照着讲。段落之间留了「为什么」的补充，讲的时候挑着用。

## 开场：一句话定位

"工具这块，我用一句话概括我的设计：**给模型的是一份契约，给代码的是五层防线，中间用'错误反馈闭环'把两边接起来**。我展开讲，从一条工具调用完整走一遍。"

## 第一段：工具怎么到模型手里——契约

"先说定义。我项目里一共十二个工具——读文件、写文件、精确编辑、glob、grep、bash、记记忆、忘记忆、MCP 外部工具、派子 agent、子 agent 间通信 hub、还有子 agent 交结果用的 yield。

每个工具只实现一个很小的接口：名字、描述、参数 schema、危险等级、能不能并发、以及一个 Execute 执行函数。**没有别的了**。执行函数不负责截断输出、不负责落盘、不负责审批——那些是运行时的事。这个分离是我设计的第一原则：工具是"面向模型的入口"，runtime 是"进程引擎"，两者各管各的。

重点在参数 schema。比如 edit 这个工具，它的参数定义是 `file_path / old_string / new_string / replace_all`，描述里我写了三句话：old_string 必须和文件内容**逐字节**一致、出现多次要么扩大上下文要么开 replace_all、**edit 之前必须先 read 过这个文件**。这三句话就是给模型的契约——模型是靠读描述来决定怎么调用的，描述写得不好，后面全白搭。

这套 schema 经过 Registry 汇总、按名字排序，转成 provider 的标准 JSON Schema 跟着请求发出去。这里有两个细节我特意做的：一是**排序**，工具定义的顺序每次请求都一样，这样 provider 的 prompt 缓存才能命中——缓存是长会话里最贵的东西，实测第二次请求 cached token 直接命中；二是 task 工具的**描述是动态生成的**，它会在运行时把'当前有哪些子 agent 可用、每个是干嘛的、是不是只读'枚举进描述里，模型不用猜。"

## 第二段：模型怎么把工具还回来——流式合并

"模型那边是流式返回的，工具调用参数是一块一块来的。这里我踩过一个很典型的坑：**流式分块是按 Index 分组的，CallID 只在第一个分块里出现**。我第一版按 CallID 分组，结果一次调用被拆成了两次。后来改成按 `StreamingMeta.Index` 合并：同 Index 的参数片段拼起来，CallID 出现就填上。这个细节看起来小，但它决定了工具调用能不能成环——拼错了，工具收到半截参数，执行的就是错误的命令。"

## 第三段：调用进来先过审批——纯函数决策

"模型返回的调用进 Executor，第一步是查表，第二步是审批。审批是**纯函数**，没有状态、没有 I/O，输入是四样东西：工具声明的危险等级、用户的模式配置、用户写的规则、工具自己按参数做的自检。

为什么要有第四样？因为**危险等级是跟着参数走的，不是跟着工具走的**。`bash` 这个工具，`git status` 是只读的、`rm -rf` 是危险的、`node server.js` 是未知的——固定一档管不了。所以我让 bash 实现了一个 `Decision(args)` 接口：拿参数去跑一个分类器纯函数，只读白名单里的命令降成 read 档、危险命令打上 Override 强制询问——**危险命令在 yolo 全放行模式下也拦**。这个分类器有四十多个样例的单测钉着，保守方向很明确：漏判危险是事故，误判只读只是多一次弹窗，所以不认识的命令一律询问。

审批决策我数过，一共五步：工具自己说 deny 永远 deny；用户 deny 规则在任何模式都生效；yolo 模式下只认显式规则；危险 Override 在非 yolo 模式强制弹窗；最后才轮到工具显式策略、用户规则、等级乘模式。空规则的时候，它和最简单的那张 3×3 决策表逐字节等价——这条回归不变量有测试钉死。"

## 第四段：参数校验为什么分四层

"参数校验，面试官最爱问'你在哪一层校验'。我的答案是四层，每层管不同的事：

**第一层是声明层**，就是刚才说的 JSON Schema。它管的是'模型生成参数的质量'，不是'拦住坏参数'——它是给模型看的契约，不是给代码的校验器。这是很多人混淆的地方：以为 schema 传出去 provider 会帮你校验，其实 provider 只负责把它喂给模型，模型照样可能给出缺字段的参数。

**第二层是协议层**：模型返回的参数是 JSON 字符串，我 Unmarshal 成 map。如果 JSON 坏了怎么办？——**按空参处理**，不中断整个 run。为什么？模型偶尔手抖是常态，为它中断一次 run 代价太大。按空参处理之后，工具的必填检查自然会报'xx 必填'，这个错误会作为工具结果送回模型，模型重发一次就好了。失败总长度就是一次工具往返，可控。

**第三层是解析层**：每个工具的执行函数第一行先做语义检查。read_file 查路径非空、edit 查 old_string 非空、bash 查命令非空。这里的关键是**错误文本要能指导模型改对**。比如 edit 发现 old_string 在文件里出现了三次，我的错误文本是'出现 3 次（要求唯一）；请扩大上下文使其唯一，或设 replace_all=true 全部替换'——模型看到这个，下一次调用就能改对。如果只报一个'参数错误'，模型只能瞎猜。

**第四层是语义层**，管业务约束。edit 的'先读后改'守卫就是这一层：工具内部查一下'本会话读过这个文件吗、读完之后文件有没有被外部改过'，没读过或者被改过就拒绝，要求模型先 read 再 edit。这是防'模型凭记忆整文件重写、把别人改的东西盖掉'这个真实事故。

最完整的例子是子 agent 交结果用的 yield 工具。它有个 schema 重试协议：提交的数据不符合约定 schema，就把**带路径的问题列表**和**剩余机会**一起退回去——'$.files 类型应为 array，修正后再交一次，还剩 2 次机会'。重试三次之后，宽松模式就放行但打警告，严格模式就判失败。两条路都**终止**，不会让模型在格式上空转无限循环。"

## 第五段：失败怎么收场——错误反馈闭环

"我把这四层校验和错误处理统称为**错误反馈闭环**：任何一层拦下问题，产物都是一条带着'为什么 + 怎么改'的文本，它作为工具结果落进会话，模型下一次调用就能看到并自我纠正。这就是 function calling 真正的容错机制——不是 harness 替模型做决定，而是**把失败信息完整、可操作地交还给模型**。

闭环外面还有兜底：模型反复错，有步数上限五十步；子 agent 死活不交结果，有四道闸——提醒三次、软预算强制收尾、墙钟超时、宽限后硬杀；bash 跑挂了，有超时和进程组回收，返回'命令超时，已终止进程组'而不是挂死。"

## 第六段：踩过的坑——三个最值得讲的

"最后讲三个坑，都是我实际踩过的，它们说明这套设计不是拍脑袋来的：

第一个，**工具根本没传给模型**。最早一版我把工具列表参数忽略了，模型只能口头说'你可以用 ls'。教训是工具定义有没有进请求，要有测试钉住。

第二个，**流式参数按 CallID 误拆**，就是刚才说的 Index 问题。

第三个最有意思——**工具结果的错误曾经被静默吞掉**。流式消费函数出错时我用了下划线忽略错误，结果流断在半截，还拿着截断的参数去执行了工具。后来我把'流错误'当返回值处理，流的错误和消息一样重要。"

## 收尾：把话接回大局

"所以你看，工具这条线串起了我项目的三个核心原则：**契约给模型、防线给代码、正确性全部纯函数化**——审批决策、bash 分类、schema 校验全是纯函数，所以 317 个测试函数里绝大部分不调模型、不联网，几秒跑完全量回归。这就是为什么我敢说这套 harness 的每一层行为都是被测试钉死的。"

---

# 附：追问速答表

| 追问 | 一句话答案 |
|---|---|
| 为什么工具要分 Shared/Exclusive？ | bash 的 cwd 是实例内可变状态、write/edit 写文件——并发会竞争；read/grep 无副作用可并行。Exclusive 执行前先 `wg.Wait()` 等并行批次收尾（executor.go:146-149） |
| 为什么执行函数不返回 string 而是写 Sink？ | 截断/落盘是运行时职责（Sink 头尾窗口 + artifact 指针），工具只管产出——Tool 与 Runtime 分离 |
| MCP 工具为什么默认 write？ | 外部未知工具的副作用不可知，"未知副作用不能当只读放行"（mcp.go:41-42）；60s 超时防慢 server 卡死 turn（mcp.go:17） |
| 坏 JSON 按空参，会不会误伤正常调用？ | 空参会触发工具首行必填报错，错误落盘模型可见，代价=一次往返；中断 run 代价=整个 turn 重来 |
| yield 为什么不用顶层 anyOf？ | 部分 strict provider 会拒绝整个工具定义；三态互斥放工具内校验（yield.go:131 注释） |
