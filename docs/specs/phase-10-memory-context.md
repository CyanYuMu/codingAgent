# Phase 10 详细设计：记忆与上下文（M3 · Memory & Context）

> 状态：**待评审** · 日期：2026-08-24 · 所属方案：[2026-08-24-evolution-plan.md](2026-08-24-evolution-plan.md) §9 M3（C1–C5 · D6 · D7 + D 节剩余）
> 前置：M1（P8 地基）、M2（P9 委派运行时）已完成并提交
> 目标：让**记忆真的能被召回**（今天含 `?` 的问句是静默失败）、让**提示词前缀稳定**（今天每轮都变，缓存全丢）、
> 让**上下文先便宜后昂贵地收缩**（今天只有"调模型摘要"一种手段）、让**项目知识跨会话复用**（今天每个会话从零 explore）。

---

## 0. 已拍板决策

| # | 决策点 | 结论 | 理由 |
|---|---|---|---|
| 1 | **FTS 查询清洗** | 永不把用户原文交给 `MATCH`：分词 → 丢弃 <3 字符的词 → CJK 连续段按 **3 字滑窗** 展开 → 每项加双引号（内部 `"` 转义为 `""`）→ `OR` 连接，最多 24 项 | trigram 分词器要求词长 ≥3；`?`/`(`/`-`/`*` 直传会触发 FTS 语法错误 |
| 2 | **清洗后没有可用词怎么办** | 不返回空，改为**按 importance×recency 兜底召回**（不走 MATCH） | 「你还记得什么」这类无实词问句也该有结果 |
| 3 | **召回失败不再静默** | `Recall` 的错误一路上报到装配层，打一次日志并降级为兜底召回 | 今天 `err != nil` 被丢掉，问题一年都发现不了 |
| 4 | **记忆分库** | `scope = project`（默认，落 `<项目桶>/memory.db`）与 `scope = global`（落 `<Home>/memory/global.db`）；召回时并查两库、按分数合并 | 「我偏好中文回复」属于跨项目事实；项目约定不该外溢 |
| 5 | **去重与覆盖** | 有 `key` → 按 `(scope, project_id, key)` upsert；无 key → **trigram Jaccard 相似度 ≥0.85** 视为近重复，更新旧条目而不是新增 | 同一偏好说三次只该留一条 |
| 6 | **失效而非删除** | `forget(id\|key)` 置 `veracity=0` 并记 `superseded_by`；`/forget` 只清**当前项目**且需确认 | 过时记忆要能追溯为什么过时 |
| 7 | **注入位置与频率** | 记忆块固定在 system 前缀**末尾**；只在**会话首轮**与**每次压缩后**刷新（`Manager` 缓存 system 前缀 + `InvalidateSystem()`） | 前缀每轮变化 = prompt cache 每轮失效，这是当前最大的隐性成本 |
| 8 | **L6 剪枝落在哪** | **回放期剪枝**：`Compact()` 先做零模型调用的剪枝，够省就直接返回；剪枝边界作为一条 `prune` 条目**落盘**，`Build()` 回放时对边界之前的工具结果做替换 | 不重写 JSONL（审计完整），边界单调递增所以不会来回抖动、不churn 缓存 |
| 9 | **压缩后恢复** | 注入 `<recent-files>` **路径与区间清单**（不自动重读内容）+ 一句 auto-continue | 自动重读是 Claude Code 行为，但在本项目的窗口预算下代价高于收益；先给"你刚才读过什么"，模型要就自己读 |
| 10 | **read 是否用 file_notes 顶替内容** | **不**。默认只做两件安全的事：①**会话内**重复读未变更文件 → 返回"未变更，上次在第 N 轮读过第 a–b 行"；②启动注入 ≤1.5k token 的**项目地图**。用笔记替代真实内容需 `memory.read_notes: true` 显式开启 | 拿摘要冒充文件内容会让模型基于旧信息改代码，这是正确性问题 |
| 11 | **后台巩固管线** | M3 **不做**两阶段 LLM 巩固与 `MEMORY.md`；只做**确定性沉淀**：explorer 的结构化产出 → `file_notes` upsert | LLM 巩固的收益依赖评测闭环（M4 的 trace/eval），先把确定性的部分做实 |
| 12 | **split-turn 双摘要** | 本期**不做**（切点不在用户 turn 起点时，按现有规则继续向旧回退到安全切点） | 现有回退已保证配对安全；双摘要的收益要等真实长会话数据支撑 |

---

## 1. 本阶段产出与边界

### 产出

| 包 | 改动 |
|---|---|
| `internal/memory/query.go`（新） | `ftsQuery` 清洗、CJK 滑窗、`similarity`（trigram Jaccard） |
| `internal/memory/memory.go` | Schema v2（scope/project_id/kind/key/why/updated_at/last_accessed/access_count/superseded_by/tags）+ 一次性迁移；`Remember` 走 upsert/近重复；`Forget`/`Invalidate`；`Touch`（访问回写） |
| `internal/memory/retrieval.go` | 清洗后 MATCH；无实词兜底召回；打分加 `access`/`scope` 两个信号；`Union` 多库合并 |
| `internal/memory/notes.go`（新） | `file_notes` 表：`UpsertNote`/`Notes`/`ProjectMap`（≤1.5k token 的项目地图） |
| `internal/tool/memory_tool.go` | `remember` 参数扩展（`kind`/`key`/`why`/`scope`）；新增 `forget` 工具 |
| `internal/instructions`（新） | L1 项目层：逐级 `AGENTS.md`/`CLAUDE.md` + `@import`（≤5 跳、防环、跳过代码块）+ `RULES.md` 粘性 |
| `internal/context/manager.go` | system 前缀缓存 + `InvalidateSystem()`；`Compact` 先剪枝后摘要；压缩后注入 `<recent-files>`；`fileOps` 统计 |
| `internal/context/prune.go`（新） | 纯函数剪枝：保护窗、最小节省、`[输出已省略]` 占位、artifact 指针保留 |
| `internal/context/compaction.go` | 摘要输入附 `<files>` 树 |
| `internal/session` | `prune` 自定义条目 + 回放期应用 |
| `internal/tool/tools.go` | `read_file` 会话内去重（未变更文件返回提示而非全文） |
| `cmd/agent/main.go` | 装配：全局记忆库、L1 项目层、记忆块固定位置、压缩后失效缓存 |
| `cmd/agent/config.go` | `memory` 配置段（`global`/`recall_top_k`/`recall_budget`/`read_notes`/`project_map`） |

### 不做（留给 M4 或后置）

- 两阶段 LLM 后台巩固、`MEMORY.md` 索引（依赖评测闭环，M4 后再看）
- 向量嵌入检索（trigram + 多信号在单机规模够用）
- split-turn 双摘要、snapcompact、预阈值后台摘要（M3 只保证"够省、正确、稳定"）
- LLM judge 决定召回

### 验收（可观察行为）

1. **crit 修复**：`recall "这个项目的构建命令是什么？(go build)"` 不再报 FTS 语法错误，能召回相关条目；`Recall` 返回的错误在装配层被打印而不是吞掉。
2. **去重**：同一偏好 `remember` 三次（措辞略有差异）→ 库里只有一条，`updated_at` 前进、`access_count` 保留。
3. **失效**：`forget key=build-cmd` 后该条不再进召回，但行还在（`veracity=0` + `superseded_by`），可查。
4. **分库**：`scope=global` 的记忆在另一个项目里能召回；`scope=project` 的不能。
5. **缓存稳定**：连续 10 个 turn，system 前缀字节完全一致（压缩后才变）；用 `-p` 跑一轮打印前缀哈希验证。
6. **剪枝**：制造 10 个 8KB 工具结果 + 小窗口 → 第一次触发压缩时先走剪枝（`[compaction: prune]`），会话继续可用；再涨才走摘要。
7. **L1 项目层**：项目根放 `AGENTS.md`（含 `@docs/x.md` 导入）→ 系统提示里出现该文件绝对路径与展开后的内容；`RULES.md` 出现在最后且压缩后仍在。
8. **项目地图**：第二个会话启动时系统提示里有 `<project-map>`（来自上个会话 explorer 的 file_notes）。
9. **会话内不重复读**：同一会话第二次 `read_file` 同一未变更文件 → 返回"未变更（上次在第 N 轮读过 1–120 行）"而不是整篇内容。
10. `env -u GOROOT go build ./... && go vet ./... && go test ./...` 全绿。

---

## 2. 记忆 v2

### 2.1 Schema 与迁移

```sql
CREATE TABLE memories (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  scope         TEXT NOT NULL DEFAULT 'project',   -- project | global
  project_id    TEXT NOT NULL DEFAULT '',
  kind          TEXT NOT NULL DEFAULT 'fact',      -- user | feedback | project | reference | decision | fact
  key           TEXT,                              -- 稳定键，用于 upsert
  content       TEXT NOT NULL,
  why           TEXT,
  source        TEXT NOT NULL DEFAULT 'user',      -- user | model | harness
  veracity      REAL NOT NULL DEFAULT 1.0,         -- 0 = 已失效
  importance    REAL NOT NULL DEFAULT 0.5,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  last_accessed INTEGER,
  access_count  INTEGER NOT NULL DEFAULT 0,
  superseded_by INTEGER REFERENCES memories(id),
  tags          TEXT
);
CREATE UNIQUE INDEX memories_key ON memories(scope, project_id, key) WHERE key IS NOT NULL AND key <> '';
CREATE VIRTUAL TABLE memories_fts USING fts5(content, tags, tokenize='trigram');
```

迁移（一次性、幂等）：建好 `memories` 后，若旧表 `working_memory` 存在且 `memories` 为空 → 逐行搬运（`content/source/veracity/importance/kind=memory_type/created_at`，`updated_at=created_at`）并重建 FTS。**旧表保留不删**（出问题能回查）。

### 2.2 写入：upsert、近重复、脱敏、限量

```go
type Opts struct {
    Scope, Kind, Key, Why, Source, Tags string
    Veracity, Importance                float64
}
func (s *Store) Remember(content string, o Opts) (id int64, updated bool, err error)
```

1. `content` 去首尾空白；空则报错；>2000 字符截断并标 `tags+=truncated`。
2. **脱敏**：命中 `sk-[A-Za-z0-9]{16,}`、`ghp_…`、`AKIA…`、`-----BEGIN .* PRIVATE KEY-----` 等模式 → 整条拒绝写入并返回原因（不做部分打码：半条密钥仍是泄漏面）。
3. `Key != ""` → `INSERT … ON CONFLICT(scope, project_id, key) DO UPDATE`：更新 content/why/importance/updated_at，保留 created_at 与 access_count。
4. `Key == ""` → 近重复检测：用清洗后的查询取 top 5 候选，`similarity(content, cand) ≥ 0.85` → 更新该条（取 importance 的较大值），返回 `updated=true`。
5. 每个 (scope, project_id) 上限 500 条：超限时按 `importance×recency×veracity` 最低者淘汰（真删，且删 FTS 行）。

```go
func (s *Store) Forget(idOrKey string, reason string) error  // veracity=0 + why 追加原因
func (s *Store) Invalidate(id int64, supersededBy int64) error
func (s *Store) ClearProject() error                         // /forget：只清当前项目
```

### 2.3 召回

```go
// internal/memory/query.go
// FTSQuery 把自然语言问句清洗成 FTS5 安全查询；返回 "" 表示没有可用实词。
func FTSQuery(q string) string

// Similarity 返回两段文本的 trigram Jaccard 相似度（0–1）。
func Similarity(a, b string) float64
```

清洗规则（纯函数，重点测试对象）：

| 输入 | 处理 |
|---|---|
| 空白与标点 | 切分符：Unicode 空白 + `?!,.;:()[]{}"'“”‘’、。，；：！？（）【】` 等 |
| ASCII 词 | 长度 ≥3 才保留（`go`、`ID` 被丢；`build` 保留） |
| CJK 连续段 | 长度 ≥3 → 3 字滑窗（`记忆系统` → `记忆系`、`忆系统`）；长度 <3 → 丢 |
| 每一项 | 包双引号，内部 `"` 转义为 `""`（FTS5 字符串字面量规则） |
| 组合 | `OR` 连接，去重，最多 24 项（超出按出现序截断） |
| 结果为空 | 返回 `""`，调用方走**兜底召回**（不带 MATCH，按 `importance×recency` 取 topK） |

打分（纯函数）：

```
score = (0.45·fts + 0.20·importance + 0.15·recency + 0.10·log1p(access_count) + 0.10·scopeBoost) · veracity
scopeBoost: project = 1.0, global = 0.6      // 同分时项目内的更贴题
recency: exp(-age / 72h)
veracity = 0 的行直接不进候选（SQL 层过滤）
```

召回后**回写访问**：`access_count += 1`、`last_accessed = now`（批量一条 UPDATE，失败只记日志不影响主流程）。

多库合并：

```go
// Union 把多个库当一个召回源：各自取 topK，合并后按分数排序取 topK，按 content 去重。
func Union(stores ...*Store) Recaller
```

### 2.4 查询构造与注入

```go
// BuildRecallQuery 用最近 3 个用户 turn（含当前）拼召回查询，截断到 4000 字符。
func BuildRecallQuery(history []message.Message) string
```

注入形态（固定在 system 前缀末尾）：

```
<memories>
- [user · global · 0.9] 用户偏好中文回复 (id=12)
- [project · project · 1.0] 构建命令是 env -u GOROOT go build ./... (id=45)
</memories>
（以上是背景上下文；当前用户消息与工具结果优先。要看某条的来龙去脉：read_file memory://45）
```

- 预算 ≤ `recall_budget`（默认 1500 token 估算），超出按分数截断。
- **刷新时机**：会话首轮、每次压缩后、`/new` `/resume` 切会话。其余轮次复用缓存 → 前缀字节稳定。

---

## 3. L1 项目层（`internal/instructions`）

```go
type File struct{ Path, Content string; Sticky bool }
type Block struct{ Files []File; Text string } // Text 是渲染好的注入文本

// Load 从 git 根到 cwd 逐级收集项目指令文件，并展开 @import。
func Load(cwd, home string, limit int) (Block, error)
```

规则：

1. **层级**：`<Home>/AGENTS.md`（用户级，最前）→ git root 到 cwd 的每一级目录，每级取第一个存在的 `AGENTS.md` / `CLAUDE.md`（本项目原生名优先 `AGENTS.md`）→ 祖先在前、近者在后（近者后置 = 更强）。
2. **@import**：行首或空白后的 `@path` 展开为文件内容；相对路径相对**导入它的文件**所在目录；`~` 展开家目录；≤5 跳；已展开过的文件不再展开（防环）；**围栏代码块与行内代码里的 `@` 不展开**；`git@github.com:…`、`user@example.com` 不当导入；目标不存在则原样保留。
3. **RULES.md**：`<Home>/RULES.md` 与 `<cwd>/RULES.md` 标 `Sticky`，渲染在整块**最后**，并在每次压缩后重新贴（借 §2.4 的失效机制自然做到）。
4. **预算**：整块 ≤ `limit`（默认 32KB 字符）；超出按"近者优先"截断，并在块尾注明被截断的文件数。
5. 渲染：每个文件一段 `<project-instructions path="/abs/path">…</project-instructions>`，模型能看到绝对路径。

---

## 4. 上下文治理

### 4.1 剪枝（L6，零模型调用）

```go
// internal/context/prune.go
type PruneOpts struct {
    ProtectRecent int // 保护最近 N 估算 token 的工具结果（默认 40000）
    MinSavings    int // 至少省这么多才执行（默认 20000）
    MinResult     int // 小于这个的结果不剪（默认 50）
}

// PlanPrune 从新到旧扫描，返回应被省略的工具结果所在的**消息下标集合**与预计节省量。
func PlanPrune(msgs []message.Message, o PruneOpts) (idx []int, savings int)

// ApplyPrune 把指定下标的工具结果内容替换成占位（保留 artifact:// 指针），返回新切片（不改原切片）。
func ApplyPrune(msgs []message.Message, idx []int) []message.Message
```

- 占位文本：`[输出已省略：约 N tokens]`；原内容里若含 `artifact://X` 则追加 `（完整内容 artifact://X）`。
- **落盘**：剪枝一旦发生，向会话追加 `prune` 自定义条目 `{beforeEntryID, savings}`；`Session.Replay()` 回放到该条目之前的消息时，对其中的工具结果应用占位。边界只前进不后退 → 前缀单调，不会来回抖。
- **接进 Compact**：`Compact()` 先 `PlanPrune`，省够 `MinSavings` 就落一条 `prune` 并返回 `true`（事件 `reason=prune`）；否则走摘要。溢出恢复路径同样先剪枝。

### 4.2 摘要里的 `<files>`

压缩时统计文件活动（从 assistant 的 tool_call 里抽 `read_file`/`write_file`/`edit` 的 path）：

```
<files>
internal/subagent/
  manager.go (RW)
  driver.go (Read)
cmd/agent/main.go (Write)
[…3 files elided…]
</files>
```

- `Read` 只读过；`Write` 写过没读过；`RW` 都有。上限 20 个，按最近活动排序。
- 附在摘要文本之后，一起作为 compaction 条目落盘。

### 4.3 压缩后

- 追加一条 user 消息：`[上下文已压缩] 继续当前任务。最近涉及的文件：<recent-files> …（需要内容就用 read_file 重新读）`。
- `InvalidateSystem()` → 下一次 `Build` 重新算记忆块与 sticky RULES。

### 4.4 会话内 read 去重

`read_file` 在同一会话内记录 `path → {mtime, size, 首次读的 turn, 行区间}`（`ToolContext` 级的小 map，随会话生命周期）：
- 同一路径、mtime+size 未变、请求区间被已读区间覆盖 → 返回 `文件未变更（上次在第 N 轮读过第 a–b 行），内容仍在上文中。需要其它区间就带 offset/limit 再读。`
- 变更过 → 正常读，并更新记录。

---

## 5. 项目知识（file_notes）

```sql
CREATE TABLE file_notes (
  project_id TEXT NOT NULL, path TEXT NOT NULL,
  summary TEXT NOT NULL, symbols TEXT,
  mtime INTEGER NOT NULL, size INTEGER NOT NULL,
  updated_at INTEGER NOT NULL, hit_count INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (project_id, path)
);
```

- **来源**：explorer 类子 agent 的结构化产出（`files:[{path, role}]`）在 Manager 结算时自动 upsert（`summary = role`）。这是确定性沉淀，不需要额外模型调用。
- **项目地图**：会话首轮注入 `<project-map>`：按目录分组的 `path — summary`，按 `hit_count desc, updated_at desc` 排序，预算 ≤1.5k token 估算。
- **失效**：注入前检查 mtime/size，变了的条目标记 `(可能已过时)` 而不是删除。

---

## 6. 配置

```yaml
memory:
  global: true          # 是否同时启用 <Home>/memory/global.db
  recall_top_k: 5
  recall_budget: 1500   # 记忆块 token 预算（估算）
  project_map: true     # 会话首轮注入项目地图
  read_notes: false     # true = read_file 命中未变更文件时用 file_notes 顶替内容（默认关）
  max_per_scope: 500    # 每个作用域的条数上限
context:
  prune_protect_recent: 40000
  prune_min_savings: 20000
```

---

## 7. 分期与验收映射

| 子阶段 | 内容 | 对应验收 |
|---|---|---|
| **P10.1 记忆正确性** | `FTSQuery`/`Similarity`；Schema v2 + 迁移；upsert/近重复/脱敏/限量；`Forget`/`Invalidate`；打分加信号 + 访问回写；`Union` 双库；`remember`/`forget` 工具 | 1、2、3、4 |
| **P10.2 注入与项目层** | system 前缀缓存 + `InvalidateSystem`；`BuildRecallQuery` + 固定位置注入；`internal/instructions`（层级 + @import + RULES 粘性） | 5、7 |
| **P10.3 上下文治理** | `PlanPrune`/`ApplyPrune` + `prune` 条目 + 回放应用；`Compact` 先剪后摘；`<files>` 树；压缩后 `<recent-files>` + auto-continue | 6 |
| **P10.4 项目知识** | `file_notes` 表 + explorer 产出自动 upsert + `<project-map>` 注入；`read_file` 会话内去重 | 8、9 |

每个子阶段结束：`build && vet && test` 全绿 + 一次提交；P10.1 与 P10.3 各做一次真实模型冒烟。

---

## 8. 测试策略

| 组 | 用例 |
|---|---|
| FTS 清洗 | `"构建命令是什么？(go build)"` → 不含裸标点、每项带引号；CJK 滑窗；<3 字符词被丢；纯标点 → `""`；引号转义；上限 24 项 |
| 相似度 | 同义改写 ≥0.85；不同事实 <0.5；空串安全 |
| 记忆写入 | key upsert 保留 created_at/access_count；无 key 近重复更新而非新增；密钥被拒；超限淘汰最低分 |
| 失效 | `Forget` 后不进召回、行还在；`ClearProject` 不动 global |
| 召回 | 含 `?`/引号/括号的问句不报错；无实词走兜底；打分排序符合预期；访问回写生效；`Union` 去重合并 |
| 迁移 | 旧 `working_memory` 有数据 → 迁移后条数一致、FTS 可召回；重复调用 `Open` 不重复迁移 |
| 前缀稳定 | 同一 Manager 连续 Build 10 次，system 前缀完全一致；`InvalidateSystem` 后才变 |
| 指令文件 | 层级顺序、同级 AGENTS 优先、@import 5 跳与环、代码块内不展开、`git@`/邮箱不展开、缺失原样保留、预算截断 |
| 剪枝 | 保护窗内不剪；小结果不剪；省不够不剪；artifact 指针保留；`prune` 条目回放幂等；剪枝后配对仍完整 |
| `<files>` | Read/Write/RW 标记正确；上限 20 |
| read 去重 | 同一会话重复读未变更文件 → 提示；文件变更后正常读 |
| 项目地图 | explorer 产出 → file_notes upsert；地图预算截断；mtime 变化标注可能过时 |

---

依据：`my_code_agent` 现状代码（`internal/memory/*`、`internal/context/*`、`internal/tool/{tools,memory_tool}.go`、`cmd/agent/main.go`）；oh-my-pi `docs/{memory,compaction,context-files}.md`（pruning 阈值 40k/20k/50、useless 省略、`<files>` 树形、@import 五跳与代码块豁免、RULES 粘性）；Claude Code 的 CLAUDE.md 层级与压缩后恢复行为；演进方案 §C/§D。
