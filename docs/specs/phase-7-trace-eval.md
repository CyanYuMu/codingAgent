# Phase 7 详细设计：Trace + Eval（审计闭环）

> 状态：**待评审** · 所属 spec：[harness-rearchitecture.md](harness-rearchitecture.md) · 前置：P2（session JSONL）、P3（usage）
> 本阶段收尾 roadmap：让 JSONL 同时是审计追踪和评测输入，形成「跑 → 测 → 改」闭环。

---

## 0. 目标与边界

### 本阶段产出

1. `internal/session` 增 usage 持久化（assistant 消息带用量）。
2. `internal/trace/tracer.go` —— 读 JSONL 聚合用量/消息/工具统计。
3. `internal/eval/evaluator.go` —— 任务夹具（prompt + input + expected）+ 字节级 verify。
4. `evals/` 放 fixture。

### 本阶段不做（defer）

- SQLite 派生索引（P7 先 JSONL 扫描聚合，SQLite 是 refine）。
- 反事实回放优化器（read_optimizer）。
- LLM judge（评测只用字节 diff / reward，不用模型打分）。

### 验收标准

- `env -u GOROOT go build ./...` + `go test ./...` 通过。
- 跑一个 fixture 出 pass/fail + 用量报告。

---

## 1. 参照 oh-my-pi 的核心原则

1. **JSONL 就是 trace**：不搞独立的 trace 格式，session 转录 = 审计追踪 = 评测输入。
2. **评分用字节精确 diff，不用 LLM judge**：开放式 agent 任务的评分是「格式化后字节比较」或「任务自带 reward」，不靠模型打分。
3. **SQLite 是派生索引**：磁盘 JSONL 是真相源，SQLite 是重建出来的查询索引（P7 先不做 SQLite，直接扫 JSONL 聚合）。

---

## 2. Trace（JSONL 即 trace + 用量聚合）

### 2.1 usage 持久化（补 session）

当前 session 只存 `message.Message`（Role + Blocks），**不存用量**。P7 补上：

```go
// entry.go：Entry 加 Usage（assistant 消息的用量）
type Entry struct {
	...
	Usage model.Usage `json:"usage,omitempty"`
}
```

```go
// session.go：Append 增一个带用量的变体
func (s *Session) AppendWithUsage(m message.Message, u model.Usage) error
```

### 2.2 tracer（读 JSONL 聚合）

```go
package trace

type Stats struct {
	Turns            int // assistant 消息数
	ToolCalls        int // 工具调用数
	PromptTokens     int
	CompletionTokens int
}

// Analyze 读 session 文件，聚合用量/消息/工具统计。
func Analyze(s *session.Session) (Stats, error) {
	entries, _ := s.Entries() // 需要 session 暴露 Entries
	var st Stats
	for _, e := range entries {
		if e.Type == session.EntryMessage && e.Message.Role == message.RoleAssistant {
			st.Turns++
			st.PromptTokens += e.Usage.PromptTokens
			st.CompletionTokens += e.Usage.CompletionTokens
			for _, b := range e.Message.Blocks {
				if b.Kind == message.BlockToolCall {
					st.ToolCalls++
				}
			}
		}
	}
	return st, nil
}
```

---

## 3. Eval（任务夹具 + 字节级 verify）

### 3.1 夹具结构

```
evals/<name>/
  prompt.md      # 任务描述
  input/         # 起始文件（agent 看到的）
  expected/      # 期望的最终文件（agent 应产出）
```

### 3.2 评估流程（edit benchmark）

```go
package eval

type Fixture struct {
	Name    string
	Prompt  string            // prompt.md 内容
	Input   map[string]string // 相对路径 → 内容（input/）
	Expected map[string]string // 相对路径 → 内容（expected/）
}

// Run 在隔离 workdir 里跑 agent，比较产出文件 vs expected。
func Run(ctx context.Context, fx Fixture, ag *agent.Agent, bash *runtime.Bash) Result {
	// 1. 建临时 workdir，写入 input/
	// 2. 跑 agent（bash cwd = workdir），收集结果
	// 3. 读 workdir 文件，逐文件字节比较 expected
	// 4. pass = 所有 expected 文件字节一致；fail = 任一不一致
}

type Result struct {
	Name   string
	Pass   bool
	Detail string // 失败时 diff 摘要
}
```

### 3.3 字节级 verify

```go
// verify 逐文件字节比较，返回不一致清单。
func verify(workdir string, expected map[string]string) []string {
	var diffs []string
	for path, want := range expected {
		got, err := os.ReadFile(filepath.Join(workdir, path))
		if err != nil || string(got) != want {
			diffs = append(diffs, path)
		}
	}
	return diffs
}
```

> 字节精确（不格式化）是 P7 的简化；oh-my-pi 会先格式化再比（容空白差异），这是 refine。

---

## 4. 接线（`cmd/eval` 子命令或 `go test`）

P7 的 eval 用一个独立入口（不进 TUI）：

```go
// cmd/eval/main.go：跑 evals/ 下所有 fixture，打印 pass/fail + 用量
func main() {
	// 加载 fixture 目录 → 逐个 Run → 打印结果
}
```

---

## 5. 待评审点

1. **usage 持久化加到 session Entry**（`AppendWithUsage`）——接受吗？
2. **eval 用「字节精确比较」不做格式化**（oh-my-pi 的 format-then-compare 是 refine）——接受吗？
3. **eval 用独立 `cmd/eval` 入口**（不进 TUI）——接受吗？
4. **trace 先 JSONL 扫描聚合，不做 SQLite 派生索引**（refine）——接受吗？
