# Phase 5 Structured Memory 实现计划

> **Goal:** 跨会话记住偏好/事实，多信号召回（FTS + importance + recency + veracity），注入 `<memories>` 背景块。语义检索与代码检索分离。
>
> **Architecture:** `memory.Store`（SQLite + FTS5）→ `Recaller` 接口 → agent 注入 → `remember` 工具写。
>
> **Tech Stack:** `modernc.org/sqlite`（纯 Go 无 CGO）+ Go stdlib。
>
> **Spec / 设计:** [../specs/phase-5-memory.md](../specs/phase-5-memory.md)（§2-§6 含完整代码）。

## Global Constraints

- 构建/运行统一 `env -u GOROOT go ...`。
- 记忆召回是纯代码（SQL + 算术），不靠模型。
- 每任务末尾 `go build ./...` + `go test ./...` 通过。

---

## 文件结构

| 文件 | 职责 |
|---|---|
| `internal/memory/memory.go` | `Store`（Open/Remember/Close）+ `Memory`/`MemoryOpts`/`Recaller` 接口（§2） |
| `internal/memory/retrieval.go` | `score` + `Recall`（§3） |
| `internal/memory/memory_test.go` | 召回/打分单测 |
| `internal/agent/agent.go` | 注入 `Recaller` + turn 开始召回（§4） |
| `internal/agent/agent.go` 或新文件 | `renderMemories` |
| `internal/tool/memory_tool.go` | `remember` 工具（§5） |
| `cmd/agent/main.go` | 建 memory store + 注入 + 注册工具（§6） |

---

## Task 1: 存储（`memory.go`，加依赖）

- [ ] **Step 1** 加依赖：`env -u GOROOT go get modernc.org/sqlite`。

- [ ] **Step 2** 实现 `memory.go`：
  - `Memory`/`MemoryOpts`/`Recaller` 接口。
  - `Open(path)`：`sql.Open("sqlite", path)` + 建 `working_memory` 表 + `memory_fts` FTS5 表（`content='working_memory'`）+ 同步触发器。
  - `Remember(content, opts)`：INSERT 到 working_memory（触发器同步 FTS）。
  - `Close()`。

> FTS5 若 modernc 有坑，降级：`memory_fts` 换成普通表 + `Recall` 用 `LIKE '%q%'`（召回逻辑不变）。

---

## Task 2: 多信号召回（`retrieval.go`，TDD）

- [ ] **Step 1 写失败测试** `memory_test.go`

```go
func TestScoreMultiSignal(t *testing.T) {
	now := int64(1000000)
	m := Memory{Importance: 0.9, Veracity: 0.8, CreatedAt: now - 3600} // 1 小时前
	high := scoreMemory(m, 0.8, now)
	m2 := m
	m2.Importance = 0.1
	low := scoreMemory(m2, 0.8, now)
	if high <= low {
		t.Fatalf("高 importance 应得更高分：high=%v low=%v", high, low)
	}
}

func TestRememberRecall(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "mem.db"))
	defer s.Close()
	now := time.Now().Unix()
	_ = s.Remember("用户偏好用 Go 语言", MemoryOpts{Source: "user", Importance: 0.9, Veracity: 1.0, MemoryType: "preference"})
	_ = s.Remember("用户昨天喝了咖啡", MemoryOpts{Source: "user", Importance: 0.1, Veracity: 1.0, MemoryType: "fact"})

	mems, err := s.Recall("偏好 语言", 5)
	if err != nil || len(mems) == 0 {
		t.Fatalf("recall = %d, err = %v", len(mems), err)
	}
	if mems[0].Content != "用户偏好用 Go 语言" {
		t.Fatalf("top1 应为偏好记忆，got %q", mems[0].Content)
	}
	_ = now
}
```

- [ ] **Step 2 红** → **Step 3 实现**（§3，`Recall` 用 FTS5 `MATCH` + bm25，`scoreMemory` 融合）→ **Step 4 绿**

```bash
cd /Users/cyanyumu/Projects/GoProject/einoclaw-build && env -u GOROOT go test ./internal/memory/ -v
```

---

## Task 3: agent 注入记忆

- [ ] `agent.go`：`Agent` 加 `memory memory.Recaller`（nil = 无记忆）；`New` 加参数；`Run` 里 turn 开始召回 + 注入 `<memories>` system 块（§4，含 `renderMemories` + `lastUserText`）。
- [ ] 更新 `main.go` 里 `agent.New` 调用（临时 `nil`，Task 5 换真 store）。

---

## Task 4: `remember` 工具（`memory_tool.go`）

- [ ] 实现 `rememberTool{store}`：`Name()="remember"`、`Tier()=TierWrite`、`Execute` 读 `content` 写 `store.Remember(content, {Source:"model"})`。
- [ ] 系统指令加「当用户表达偏好/关键事实时，调用 remember 记录」。
- [ ] `tool.Builtins` 增 remember（或单独注册）。

---

## Task 5: 接线 + 验收

- [ ] `main.go`：`memory.Open("memory.db")` → 注入 agent + 注册 remember 工具。
- [ ] 构建 + vet + test。
- [ ] headless 两轮验收：会话 1 说「我偏好用 Go」→ 模型 remember；会话 2 问「我之前偏好什么」→ recall 注入，agent 答得出。

---

## 自检

- **spec 覆盖**：P5 的 5 项产出 → Task 1-5 全覆盖。
- **类型一致性**：`memory.Recaller`（Task 1）被 agent（Task 3）消费；`memory.Store` 被 remember 工具（Task 4）/main（Task 5）消费。
- **无占位符**：可测部分（Task 2）测试全量内联。
