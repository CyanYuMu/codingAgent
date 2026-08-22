# Phase 5 详细设计：Structured Memory（多信号召回）

> 状态：**待评审** · 所属 spec：[harness-rearchitecture.md](harness-rearchitecture.md) · 前置：P4（代码检索）
> 本阶段落地分层 Context 的 **L5 Retrieved Memory / L7 Long-term Memory**——跨会话记住偏好/事实，语义检索与代码检索（P4）分离。

---

## 0. 目标与边界

### 本阶段产出（P5 完成时）

1. `internal/memory/memory.go` —— SQLite 存储（`working_memory` 表 + FTS5 索引）+ `Remember`/`Recall`。
2. `internal/memory/retrieval.go` —— 多信号打分（FTS + importance + recency + veracity）。
3. `internal/agent` —— turn 开始时召回记忆，注入 `<memories>` 背景块。
4. `internal/tool` 增 `remember` 工具（模型显式写记忆）。
5. `cmd/agent` 接线（建 memory store，注入 agent）。

### 本阶段不做（stretch，后续再补）

- **episodic 巩固**（working → episodic 双级 + `sleep` 合并）——P5 先只做 `working_memory`。
- **向量嵌入检索**（embedding + cosine）——P5 用 FTS5 全文检索，向量是 stretch。
- **Weibull 衰减 / 多声部 recall / 贝叶斯 veracity 真值维护**——stretch。
- **auto-retain**（harness 每 N 轮自动写 transcript 摘要）——P5 只做「模型显式 remember + turn 开始 recall」。

### 验收标准

- `env -u GOROOT go build ./...` + `go vet ./...` + `go test ./...` 通过。
- **跨会话召回**：会话 1 说「我偏好用 Go」→ 模型调 `remember` 记录；会话 2 问「我之前偏好什么」→ recall 注入相关记忆，agent 答得出。
- **背景上下文、让位于活状态**：注入的 `<memories>` 块明确声明「当前消息/工具结果优先」。

---

## 1. 参照 oh-my-pi 的核心原则

### 1.1 记忆是「背景上下文」，让位于活状态

oh-my-pi 明确：recalled memory 是 **background context，不是 instructions**；当前用户消息和工具输出永远优先。注入的 `<memories>` 块要带上这句声明，避免旧记忆误导 agent。

### 1.2 代码检索 ≠ 记忆检索（P4 vs P5 的边界）

grep/LSP/AST（P4）是 **on-demand 工具返回进 transcript**；记忆（P5）是 **语义召回进 system prompt 的 `<memories>` 块**。二者是两个独立子系统，别把符号级代码事实当「可压缩的记忆」。

### 1.3 多信号打分，不是单一关键词匹配

oh-my-pi 的 recall 是混合打分：lexical/FTS + importance + recency + veracity（+ vector）。P5 用 FTS + importance + recency + veracity 四信号（vector 是 stretch）。

### 1.4 写入区分来源与置信度

model 写的（工具）与 harness 写的（transcript）用 `source`/`veracity` 区分；写入幂等、**永不阻塞会话**。

---

## 2. 存储（`internal/memory/memory.go`）

用 `modernc.org/sqlite`（纯 Go 无 CGO）+ FTS5。

```sql
CREATE TABLE IF NOT EXISTS working_memory (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    content     TEXT NOT NULL,
    source      TEXT NOT NULL DEFAULT 'user',  -- user/model/harness
    veracity    REAL NOT NULL DEFAULT 1.0,     -- 0-1
    importance  REAL NOT NULL DEFAULT 0.5,     -- 0-1
    memory_type TEXT NOT NULL DEFAULT 'fact',  -- fact/preference/decision/...
    created_at  INTEGER NOT NULL               -- unix 秒
);

CREATE VIRTUAL TABLE IF NOT EXISTS memory_fts USING fts5(content, content='working_memory', content_rowid='id');
```

```go
type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error)      // 打开/建表
func (s *Store) Remember(content string, opts MemoryOpts) error  // 写一条
func (s *Store) Recall(query string, topK int) ([]Memory, error) // 多信号召回
func (s *Store) Close() error

type Memory struct {
	ID         int64
	Content    string
	Source     string
	Veracity   float64
	Importance float64
	MemoryType string
	CreatedAt  int64
}

type MemoryOpts struct {
	Source     string
	Veracity   float64
	Importance float64
	MemoryType string
}
```

> FTS5 用 `content='working_memory'`（外部内容表）+ 触发器或手动同步。若 modernc 的 FTS5 有坑，降级 `LIKE '%query%'` 检索（召回逻辑不变，只是匹配方式换）。

---

## 3. 多信号召回（`internal/memory/retrieval.go`）

```go
// score 融合四个信号，返回 0-1 的分数。
func score(m Memory, ftsRank float64, now int64) float64 {
	age := float64(now - m.CreatedAt)
	recency := math.Exp(-age / halfLife)        // 半衰期 72h
	return 0.5*ftsRank + 0.3*m.Importance + 0.2*recency // veracity 作为乘子
	// 实际：最终 = (0.5*ftsRank + 0.3*importance + 0.2*recency) * veracity
}
```

`Recall` 流程：
1. FTS5 查询匹配（拿 bm25 rank，归一化到 0-1）。
2. 每个候选算 `score`。
3. 按 score 排序，取 topK。
4. 附带回 `veracity`/`importance` 元数据。

---

## 4. 注入（`internal/agent`）

turn 开始时，基于最后一条 user 消息召回，注入一个 `<memories>` system 块：

```go
func (a *Agent) Run(ctx, input) <-chan AgentEvent {
	...
	msgs := []message.Message{message.NewSystemMessage(a.instruction)}
	if a.memory != nil {
		q := lastUserText(input)
		if mems := a.memory.Recall(ctx, q, 5); len(mems) > 0 {
			msgs = append(msgs, message.NewSystemMessage(renderMemories(mems)))
		}
	}
	msgs = append(msgs, input...)
	...
}
```

`renderMemories` 产出：

```
<memories>
- [偏好] 用户偏好用 Go 语言（置信 0.9）
- [事实] 项目基于 eino 框架（置信 0.8）
</memories>
（以上是背景上下文，当前用户消息和工具结果优先。）
```

---

## 5. `remember` 工具（`internal/tool`）

```go
type rememberTool struct{ store *memory.Store }

func (rememberTool) Name() string        { return "remember" }
func (rememberTool) Tier() permission.Tier { return permission.TierWrite }
func (rememberTool) Execute(ctx, args, sink) error {
	content, _ := args["content"].(string)
	// 写一条 source=model 的记忆，importance/type 从 args 可选
	s.store.Remember(content, memory.MemoryOpts{Source: "model"})
	sink.Write([]byte("remembered"))
	return nil
}
```

> 系统指令里加一句「当用户表达偏好/关键事实/决策时，调用 remember 工具记录」，引导模型主动记忆。

---

## 6. 接线（`cmd/agent`）

```go
mem, err := memory.Open("memory.db")      // 持久化，跨会话
ag := agent.New(..., mem)                  // agent 注入 memory store
registry.Register(tool.NewRememberTool(mem)) // 注册 remember 工具
```

---

## 7. 边界情况与错误处理

| 情况 | 处理 |
|---|---|
| 记忆库首次打开（无文件） | `Open` 建表，空库正常 |
| FTS5 查询无匹配 | 返回空，不注入 `<memories>` |
| 记忆写入失败 | 忽略错误（记忆永不阻塞会话） |
| 召回结果为空 | 不注入，正常跑 |
| 记忆库文件损坏 | `Open` 报错，main 降级为无记忆（log 提示） |

---

## 8. 对外契约（后续阶段依赖）

| 类型/函数 | 包 | 后续谁依赖 |
|---|---|---|
| `memory.Store`（Open/Remember/Recall） | `internal/memory` | `internal/agent`（P5 注入）、`internal/tool`（remember 工具） |
| `memory.Memory` / `MemoryOpts` | `internal/memory` | 上述 |
| `<memories>` 注入契约 | `internal/agent` | P6 subagent（复用召回） |

---

## 9. 待评审点

1. **P5 只做 `working_memory` 单级**（episodic 巩固 `sleep` 留 stretch）——接受吗？
2. **召回用 FTS5 全文，不做向量嵌入**（vector 是 stretch）——接受吗？
3. **写记忆靠「remember 工具 + 系统指令引导」**，不做 auto-retain（harness 自动写 transcript）——接受吗？
4. **记忆库存 `./memory.db`**（项目本地，跨 TUI 会话持久）——接受吗？
5. **veracity/importance 用 0-1 float 简化**（oh-my-pi 的完整 veracity 分类/Weibull 是 stretch）——接受吗？
