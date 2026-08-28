# Phase 10 记忆与上下文 Implementation Plan

> **For agentic workers:** 按任务顺序实施，每个任务先写失败测试再实现（`- [ ]` 复选框跟踪）。任务之间有依赖，不要跳序。

**Goal:** 修掉「记忆召回静默失败」这个 crit，让提示词前缀在会话内稳定（prompt cache 不再每轮失效），给上下文收缩加一级零成本手段（剪枝），并把项目知识沉淀成跨会话可复用的项目地图。

**Architecture:** 记忆 = 两个 SQLite 库（项目 + 全局）经 `Union` 合并召回；查询永远经 `FTSQuery` 清洗后再进 `MATCH`，没有可用实词时走 importance×recency 兜底。上下文 = `context.Manager` 缓存 system 前缀（只在首轮/压缩后/换会话时失效），`Compact()` 先做零模型调用的**剪枝**（边界作为 `prune` 条目落盘、回放时应用），不够再调模型摘要。项目知识 = explorer 的结构化产出确定性 upsert 成 `file_notes`，会话首轮注入项目地图。

**Tech Stack:** Go 1.26；modernc.org/sqlite（FTS5 trigram）；不新增外部依赖。

**Spec:** `docs/specs/phase-10-memory-context.md`

## Global Constraints

- 只有 `internal/model` 可以 import eino。
- 每个任务结束：`env -u GOROOT go build ./... && env -u GOROOT go vet ./... && env -u GOROOT go test ./...` 通过。
- 记忆库是用户数据：**迁移必须幂等**，旧表不删；任何写入路径失败都不能让会话崩。
- 召回、剪枝、指令加载全部拆成**纯函数**再接线（这三块的 bug 只能靠单测发现）。
- 提示词前缀的稳定性是硬要求：改 `system()` 相关代码时必须跑前缀稳定性测试。
- 提交信息用中文前缀 `feat/fix/refactor/test:`，每个子阶段至少一次提交。

---

## File Structure

| 文件 | 职责 |
|---|---|
| `internal/memory/query.go`（新） | `FTSQuery`（分词 + CJK 滑窗 + 引号转义）、`Similarity`（trigram Jaccard）、`BuildRecallQuery` |
| `internal/memory/query_test.go`（新） | 清洗与相似度的表驱动测试 |
| `internal/memory/memory.go` | Schema v2 + 幂等迁移；`Remember`（upsert/近重复/脱敏/限量）；`Forget`/`Invalidate`/`ClearProject`；`Touch` |
| `internal/memory/retrieval.go` | 清洗后 MATCH + 兜底召回 + 新打分 + 访问回写；`Union` |
| `internal/memory/notes.go`（新） | `file_notes` 表与 `UpsertNote`/`ProjectMap` |
| `internal/tool/memory_tool.go` | `remember` 参数扩展 + 新增 `forget` 工具 |
| `internal/instructions/instructions.go`（新） | L1 项目层：层级发现 + `@import` 展开 + RULES 粘性 |
| `internal/context/manager.go` | system 前缀缓存 + `InvalidateSystem`；`Compact` 先剪后摘；压缩后 `<recent-files>` |
| `internal/context/prune.go`（新） | `PlanPrune` / `ApplyPrune` |
| `internal/context/compaction.go` | `<files>` 树 |
| `internal/session/{entry,replay,session}.go` | `prune` 自定义条目 + 回放期应用 |
| `internal/tool/tools.go` | `read_file` 会话内去重 |
| `internal/subagent/driver.go` | explorer 产出 → `file_notes` upsert |
| `cmd/agent/main.go`、`config.go` | 装配全局库/L1/项目地图；`memory` 与 `context` 配置段 |

---

# P10.1 记忆正确性

### Task 1: `FTSQuery` 与 `Similarity`（纯函数）

**Files:** Create `internal/memory/query.go`、`internal/memory/query_test.go`

**Interfaces:** `memory.FTSQuery(q string) string`、`memory.Similarity(a, b string) float64`、`memory.BuildRecallQuery(history []message.Message) string`

- [x] **Step 1: 写失败测试**
  - `TestFTSQueryEscapesAndDropsShortTerms`：`构建命令是什么？(go build)` → 结果里没有裸 `?`/`(`；每项被双引号包裹；`go` 被丢、`build` 保留。
  - `TestFTSQueryCJKSlidingWindow`：`记忆系统` → 含 `"记忆系"` 与 `"忆系统"`；`记忆`（2 字）→ 被丢。
  - `TestFTSQueryQuoteEscape`：含 `"` 的输入 → 输出里是 `""`。
  - `TestFTSQueryEmptyWhenNoUsableTerms`：`？！。` / `go` / 空串 → `""`。
  - `TestFTSQueryCapsTerms`：60 个长词 → 最多 24 项。
  - `TestSimilarity`：同义改写 ≥0.85；无关文本 <0.5；空串返回 0 且不 panic；完全相同 = 1。
  - `TestBuildRecallQueryUsesRecentUserTurns`：只取最近 3 条 user 文本、截断到 4000 字符。

- [x] **Step 2: 实现**
  - `tokenize`：按 Unicode 空白与标点切；`unicode.Is(unicode.Han, r)` 判 CJK（含日文假名一并按 CJK 处理）。
  - CJK 连续段 → 长度 ≥3 时生成 3 字滑窗；ASCII 词长度 ≥3 保留（按 rune 计）。
  - 去重保序，上限 24；每项 `"` 包裹并把内部 `"` 变 `""`；`OR` 连接。
  - `Similarity`：把两串切成 3-rune 集合算 Jaccard（任一为空 → 0；长度 <3 时退化为整串相等判定）。

- [x] **Step 3: 验证** `go test ./internal/memory/ -run 'FTSQuery|Similarity|BuildRecall'`

---

### Task 2: Schema v2 与幂等迁移

**Files:** Modify `internal/memory/memory.go`；Test `internal/memory/memory_test.go`

**Interfaces:** `memory.Open(path string, scope, projectID string) (*Store, error)`、`Store.Scope()`、`Store.ProjectID()`

- [x] **Step 1: 写失败测试**
  - `TestOpenCreatesV2Schema`：新库有 `memories` 与 `memories_fts`，唯一索引存在。
  - `TestMigratesLegacyWorkingMemory`：先手工建旧表塞 3 行 → `Open` 后 `memories` 有 3 行、FTS 能召回、旧表仍在。
  - `TestMigrationIsIdempotent`：连开两次，条数不变。

- [x] **Step 2: 实现**
  - 建表 + 索引（`IF NOT EXISTS`）。
  - 迁移：`SELECT count(*) FROM memories` == 0 且 `working_memory` 存在 → 事务内搬运 + 写 FTS。
  - `Store` 记住 `scope`/`projectID`，写入时作为默认值。

- [x] **Step 3: 验证** `go test ./internal/memory/ -run 'Open|Migrat'`

---

### Task 3: 写入路径（upsert / 近重复 / 脱敏 / 限量 / 失效）

**Files:** Modify `internal/memory/memory.go`；Test 同上

**Interfaces:** `Store.Remember(content string, o Opts) (int64, bool, error)`、`Store.Forget(idOrKey, reason string) error`、`Store.Invalidate(id, supersededBy int64) error`、`Store.ClearProject() error`、`Store.Get(id int64) (Memory, error)`

- [x] **Step 1: 写失败测试**
  - `TestRememberUpsertByKey`：同 key 写两次 → 1 行，content 更新，`created_at` 不变，`updated_at` 前进。
  - `TestRememberNearDuplicateUpdates`：「用户偏好中文回复」与「用户希望用中文回复」→ 只剩 1 行且 `updated=true`。
  - `TestRememberDistinctFactsCoexist`：两条无关事实 → 2 行。
  - `TestRememberRejectsSecrets`：含 `sk-` 长串 / `-----BEGIN OPENSSH PRIVATE KEY-----` → 返回错误且库里没有。
  - `TestRememberTruncatesLongContent`：>2000 字符 → 截断且打 `truncated` 标记。
  - `TestForgetKeepsRowButExcludesFromRecall`：`Forget` 后 `Recall` 不返回它，`Get` 仍能读到 `veracity=0`。
  - `TestMaxPerScopeEvictsLowest`：上限设 3，写 4 条 → 剩 3 条且被淘汰的是分数最低那条。

- [x] **Step 2: 实现**（按 spec §2.2；所有写入包在一个事务里，FTS 与主表同进同退）
  - 脱敏用一组预编译正则；命中直接 `return error`（错误文本告诉模型"别把密钥写进记忆"）。
  - 近重复：`FTSQuery(content)` 取 top5 候选后算 `Similarity`。
  - 淘汰：`ORDER BY importance*veracity*exp(-age/72h) ASC LIMIT n`（在 Go 侧算分，SQL 只取候选）。

- [x] **Step 3: 验证** `go test ./internal/memory/`

---

### Task 4: 召回（清洗 + 兜底 + 新打分 + 访问回写 + Union）

**Files:** Modify `internal/memory/retrieval.go`；Test 同上

**Interfaces:** `Store.Recall(query string, topK int) ([]Memory, error)`、`memory.Union(stores ...*Store) Recaller`

- [x] **Step 1: 写失败测试**
  - `TestRecallWithPunctuationQuery`：库里有「构建命令是 go build」→ `Recall("这个项目的构建命令是什么？(build)")` 能召回且无错误。
  - `TestRecallFallsBackWithoutTerms`：`Recall("？？")` → 返回按 importance 排序的 topK，无错误。
  - `TestRecallExcludesInvalidated`、`TestRecallWritesBackAccess`（`access_count` 从 0 → 1，`last_accessed` 非空）。
  - `TestRecallScoreOrder`：高 importance + 新 vs 低 importance + 旧 → 前者在前。
  - `TestUnionMergesAndDedupes`：两个库各有一条相同 content → 合并后 1 条；项目库的排在全局库前（scopeBoost）。

- [x] **Step 2: 实现**
  - SQL 加 `WHERE veracity > 0`；`MATCH` 参数用 `FTSQuery` 的结果；为空则走兜底 SQL（`ORDER BY importance DESC, updated_at DESC LIMIT topK*2` 再在 Go 侧打分）。
  - 打分按 spec §2.3；`Union` 按 content 去重（保留分高者）。
  - 访问回写：一条 `UPDATE … WHERE id IN (…)`，错误只记不抛。

- [x] **Step 3: 验证** `go test ./internal/memory/ -race`

---

### Task 5: `remember` / `forget` 工具与装配

**Files:** Modify `internal/tool/memory_tool.go`、`cmd/agent/main.go`、`cmd/agent/config.go`；Test `internal/tool/`

**Interfaces:** `tool.NewRememberTool(store *memory.Store)`（参数扩展）、`tool.NewForgetTool(store *memory.Store)`

- [x] **Step 1: 写失败测试**
  - `TestRememberToolPassesFields`：`{content, kind, key, why, scope}` 全部落库。
  - `TestRememberToolReportsUpdate`：近重复第二次调用 → 结果文本说明"已更新已有记忆"。
  - `TestForgetTool`：按 key 失效，结果文本确认。
  - `TestRememberToolRejectsSecret`：工具返回错误文本而不是静默吞。

- [x] **Step 2: 实现**
  - `remember` 描述写清"什么该记、什么不该记（密钥、临时状态、代码本身不要记）"。
  - `main.go`：按 `memory.global` 开 `<Home>/memory/global.db`；`Union(project, global)` 作为 Recaller；**召回错误打日志**（当前是静默 `err == nil` 判断）。
  - `config.go`：`memory` 段（global / recall_top_k / recall_budget / project_map / read_notes / max_per_scope）。

- [x] **Step 3: 验证** `go test ./...`
- [x] **Step 4: 冒烟**（已通过）：写入 build-cmd（project）与用户偏好（global）后，含标点问句 `这个项目的构建命令是什么？(直接答…)` 能召回；另一个项目里能召回 global 那条、召不到 project 那条。
- [x] **Step 5: 提交** `feat: P10.1 记忆正确性（FTS 清洗 + schema v2 + upsert 去重 + 失效 + 双库召回）`

---

# P10.2 注入与项目层

### Task 6: system 前缀缓存与固定注入位置

**Files:** Modify `internal/context/manager.go`、`cmd/agent/main.go`；Test `internal/context/context_test.go`

**Interfaces:** `(*Manager).InvalidateSystem()`

- [x] **Step 1: 写失败测试**
  - `TestSystemPrefixStableAcrossBuilds`：注入一个每次调用都返回不同内容的 `system()` → 连续 10 次 `Build` 前缀完全一致；`InvalidateSystem()` 后才变。
  - `TestCompactInvalidatesSystem`：`Compact` 成功后前缀重新计算。
  - `TestSetSessionInvalidates`：换会话后重新计算。

- [x] **Step 2: 实现**：`Manager` 缓存 `[]message.Message` 前缀与一个 `dirty` 标记；`Compact`/`RecoverOverflow`/`SetSession` 置脏。
- [x] **Step 3: 验证** `go test ./internal/context/`

---

### Task 7: `internal/instructions`（L1 项目层）

**Files:** Create `internal/instructions/instructions.go`、`internal/instructions/instructions_test.go`

**Interfaces:** `instructions.Load(cwd, home string, limit int) (Block, error)`

- [x] **Step 1: 写失败测试**（全部用临时目录搭真实文件树）
  - `TestLoadHierarchyOrder`：git root 的 `AGENTS.md` 在前、子目录的在后；同级 `AGENTS.md` 优先于 `CLAUDE.md`。
  - `TestImportExpansion`：`@docs/x.md` 展开；相对路径相对导入者目录；`~/x.md` 展开家目录。
  - `TestImportDepthAndCycle`：5 跳后停止；A↔B 互相导入不死循环。
  - `TestImportSkippedInCodeBlocks`：围栏块与行内 code 里的 `@x.md` 原样保留。
  - `TestImportIgnoresEmailAndGit`：`git@github.com:o/r.git`、`a@b.com` 不展开。
  - `TestMissingImportKeptLiteral`、`TestBudgetTruncatesFarthest`（超预算时先丢最远的祖先层）。
  - `TestRulesAreSticky`：`RULES.md` 内容出现在渲染文本末尾且 `Sticky=true`。

- [x] **Step 2: 实现**（按 spec §3；`expand` 是纯函数，输入 `(content, dir, depth, seen)`）
- [x] **Step 3: 验证** `go test ./internal/instructions/`

---

### Task 8: 接线（记忆块 + 项目层 + 召回查询）

**Files:** Modify `cmd/agent/main.go`

- [x] **Step 1: 实现**
  - system 前缀顺序固定：`[基础指令 + env] [<project-instructions>…] [<memories>] [<project-map>] [<sticky-rules>]`。
  - 召回查询用 `BuildRecallQuery(session.Replay())`，预算裁剪到 `recall_budget`。
  - `Compact` 之后（TUI/headless 收到 `EventCompaction`）不需要额外动作——`Manager` 自己置脏。
- [x] **Step 2: 验证** 单测钉住「10 次 Build 前缀不变、system() 只算一次」；真机冒烟确认模型能读到 AGENTS.md、@import 展开的内容与 RULES.md 粘性规则。
- [x] **Step 3: 提交** `feat: P10.2 注入位置与项目指令层（前缀缓存 + AGENTS.md 层级 + @import + RULES 粘性）`

---

# P10.3 上下文治理

### Task 9: 剪枝纯函数

**Files:** Create `internal/context/prune.go`、`internal/context/prune_test.go`

**Interfaces:** `PlanPrune(msgs []message.Message, o PruneOpts) ([]int, int)`、`ApplyPrune(msgs []message.Message, idx []int) []message.Message`

- [x] **Step 1: 写失败测试**
  - `TestPlanPruneProtectsRecent`：最近 40k 内的结果不进候选。
  - `TestPlanPruneSkipsSmallResults`、`TestPlanPruneRequiresMinSavings`（省不够 → 空集）。
  - `TestApplyPruneKeepsArtifactPointer`：原文含 `artifact://7` → 占位里保留该指针。
  - `TestApplyPruneDoesNotMutateInput`、`TestApplyPruneKeepsPairing`（tool 消息还在，只是内容变了）。

- [x] **Step 2: 实现**（按 spec §4.1）
- [x] **Step 3: 验证** `go test ./internal/context/ -run Prune`

---

### Task 10: `prune` 条目 + 回放应用 + 接进 Compact

**Files:** Modify `internal/session/{entry,replay}.go`、`internal/context/manager.go`；Test 两包

**Interfaces:** `session.EntryCustom` 的 `prune` 类型 + `Session.Prune(beforeEntryID string, savings int) error`

- [x] **Step 1: 写失败测试**
  - `TestReplayAppliesPrune`：写入若干工具结果 + 一条 prune 条目 → `Replay()` 里边界前的结果被占位替换、边界后的完整。
  - `TestPruneIsMonotonic`：两条 prune 条目（边界前进）→ 回放按最新边界。
  - `TestCompactPrunesBeforeSummarizing`：造出足够可剪的历史 → `Compact` 返回 "prune" 且**没有调用 summarizer**（计数断言）；剪无可剪后第二次压缩落摘要。
  - `TestCompactFallsBackToSummary`：可剪内容不足 → 正常摘要。
  - 附加：`TestNilSummarizerStillPrunes`（无摘要器也能剪）、`TestPlanPruneSkipsPlaceholders`（剪枝幂等：占位不再是候选，防前缀 churn）、`TestSessionPruneReplayMatchesApplyPrune`（回放与 ApplyPrune 占位逐字节一致）、压缩 × 剪枝边界交叠两个用例、`TestPrunedPlaceholderKeepsArtifactRef`。

- [x] **Step 2: 实现**
  - `session.Prune(beforeEntryID, savings)` 落 `prune` 自定义条目；`buildContext` 回放期应用（`applyPruneBoundaries`，深拷贝 Blocks 不污染 JSONL）。
  - 占位单一事实源：`session.PrunedPlaceholder`（context 侧复用）；`session.PrunedMarker` 让 PlanPrune 跳过已占位结果（幂等）。
  - `Manager.compact` 先剪后摘；`Compact/RecoverOverflow` 返回方式（"prune"/"summary"/""），`agent.Context` 接口跟进，事件 reason 变为 `mid-turn:prune` / `overflow:summary`。
- [x] **Step 3: 验证** `go test ./internal/session/ ./internal/context/ ./internal/agent/ -race` 全绿

---

### Task 11: `<files>` 树与压缩后恢复

**Files:** Modify `internal/context/{compaction,manager}.go`；Test `internal/context/`

- [x] **Step 1: 写失败测试**
  - `TestFileOpsTree`：read/write 混合 → `(Read)`/`(Write)`/`(RW)` 标记正确、目录分组、上限 20、超出有省略行、会话内 URL 不进树。
  - `TestSummaryIncludesFilesTag`：摘要文本里有 `<files>`。
  - `TestPostCompactionRecentFiles`：压缩后会话里多出一条含 `<recent-files>` 的用户消息。

- [x] **Step 2: 实现**：`internal/context/files.go`（文件活动收集 + 树渲染纯函数）；摘要附 `<files>` 树落盘；压缩后追加 `<recent-files>` 恢复消息。
- [x] **Step 3: 验证** `go test ./internal/context/`
- [x] **Step 4: 冒烟**：`TestMidTurnCompactionLadderSmoke`（agent 包全链路：真 session + 真 Manager + 真 循环 + 脚本化模型）——压缩阶梯 `[mid-turn:prune, mid-turn:summary]` 各一次、用量回落后不再压缩、落盘 prune/compaction 条目、回放收缩且 tool 配对完整。无真实 API 不可用，按演进方案 F.3 用脚本化模型验证 harness 行为。
- [x] **Step 5: 提交** `feat: P10.3 上下文治理（剪枝阶梯 + <files> + 压缩后恢复）`

---

# P10.4 项目知识

### Task 12: `file_notes` 与项目地图

**Files:** Create `internal/memory/notes.go`、`internal/memory/notes_test.go`；Modify `internal/subagent/driver.go`、`cmd/agent/main.go`

**Interfaces:** `Store.UpsertNote(path, summary, symbols string) error`、`Store.ProjectMap(budget int) string`

- [x] **Step 1: 写失败测试**
  - `TestUpsertNoteAndMap`：写 3 条笔记 → `ProjectMap` 按目录分组、预算内截断。
  - `TestProjectMapMarksStale`：文件 mtime/size 变了 → 该行带"(可能已过时)"；重新沉淀后恢复。
  - `TestExplorerOutputUpsertsNotes`：驱动器结算时把 `files:[{path, role}]` 写进笔记（假 Store 断言）；`TestNonExplorerOutputDoesNotUpsert`（无 files 字段不沉淀）。
  - `TestExplorerNotesFlowToProjectMap`：端到端——explorer 运行 → 真实 Store 沉淀 → 项目地图可见（验收「第二个会话有项目地图」的数据通路）。
  - 附加：`TestUpsertNoteOverwritesAndKeepsSymbols`（summary 覆盖、symbols 非空不丢）、`TestProjectMapEmpty`、`TestProjectMapBudgetTruncates`。

- [x] **Step 2: 实现**：`memory/notes.go`（file_notes 表 + `UpsertNote`/`NoteHit`/`ProjectMap` + 分组渲染纯函数）；`subagent.Options.Notes`（NoteSink 接口）在 settle 时确定性 upsert；`main.go` 注入 `<project-map>`（预算 1.5k，跟随前缀缓存）。
- [x] **Step 3: 验证** `go test ./internal/memory/ ./internal/subagent/`

---

### Task 13: `read_file` 会话内去重

**Files:** Modify `internal/tool/tools.go`；Test `internal/tool/`

- [x] **Step 1: 写失败测试**
  - `TestReadFileDedupesUnchanged`：连读两次同一文件 → 第二次返回"未变更（上次读过第 a–b 行）"。
  - `TestReadFileRereadsAfterChange`：改动文件后再读 → 返回真实内容。
  - `TestReadFileDifferentRangeStillReads`：请求未读过的区间 → 正常读。

- [x] **Step 2: 实现**（`readFileTool` 持一个带锁的 `map[string]readRecord`，随工具实例生命周期 = 会话/Run 级）
- [x] **Step 3: 验证** `go test ./internal/tool/ -race`
- [x] **Step 4: 提交** `feat: P10.4 项目知识（file_notes + 项目地图 + read 会话内去重）`

---

## 验收对照表

| # | 验收 | 覆盖任务 |
|---|---|---|
| 1 | 含标点问句能召回、错误不再静默 | 1、4、5 |
| 2 | 同一偏好三次只剩一条 | 3 |
| 3 | forget 后不召回但行还在 | 3 |
| 4 | global 记忆跨项目可见、project 不外溢 | 2、4、5 |
| 5 | 连续 10 轮前缀字节一致 | 6、8 |
| 6 | 先剪枝后摘要 | 9、10 |
| 7 | AGENTS.md 层级 + @import + RULES 粘性 | 7、8 |
| 8 | 第二个会话有项目地图 | 12 |
| 9 | 会话内不重复读同一文件 | 13 |
| 10 | build/vet/test 全绿 | 全部 |

## 风险与对策

| 风险 | 对策 |
|---|---|
| 迁移把用户记忆搞丢 | 旧表不删、迁移幂等、有测试；失败时 `Open` 返回错误让装配层降级为"禁用记忆"而不是崩 |
| 近重复判定误伤（把两条不同事实合并） | 阈值 0.85 偏保守 + 只在无 key 时生效；测试里放一组"相似但不同"的反例 |
| 剪枝把模型正需要的内容剪掉 | 保护最近 40k；占位保留 artifact 指针，可 `read_file` 读回；剪枝只发生在接近阈值时 |
| 前缀缓存导致记忆更新后不生效 | `remember` 之后不主动失效（记忆是背景信息，下一次压缩或新会话生效即可）——在 `remember` 的结果文本里说明这一点 |
| 项目地图注入把窗口吃掉 | 预算 1.5k 估算 token + 可配置关闭 |
| read 去重让模型看不到需要的内容 | 只在**同一会话内、mtime+size 未变、区间被覆盖**时触发，提示里写清"需要别的区间就带 offset/limit 再读" |
