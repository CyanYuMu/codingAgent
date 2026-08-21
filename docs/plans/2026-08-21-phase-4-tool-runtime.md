# Phase 4 Tool Runtime + Permission 实现计划

> **Goal:** 把「工具」从「调用函数」升级成「运行时」：审批纯策略 + bash 子进程 + OutputSink 压缩/落盘 + 结构化检索 + 三档中断 + agent 工具循环。
>
> **Architecture:** `permission`（纯策略）→ `runtime`（Sink/bash）→ `tool`（Tool 接口/Registry/executor/search/cache）→ `agent`（工具循环）。
>
> **Tech Stack:** Go stdlib（os/exec、regexp、go/ast）+ `github.com/dlclark/regexp2`（PCRE 兜底，P8 grep 时才加）+ 自建包。
>
> **Spec / 设计:** [../specs/phase-4-tool-runtime.md](../specs/phase-4-tool-runtime.md)（§2-§9 含完整代码）。

## Global Constraints

- 构建/运行统一 `env -u GOROOT go ...`。
- 只有 `internal/model` 可 import eino。
- 每任务末尾 `go build ./...` + `go test ./...` 通过。

---

## 文件结构

| 文件 | 职责 |
|---|---|
| `internal/permission/policy.go` | `Resolve`/`Tier`/`Mode`/`Decision`（§2） |
| `internal/runtime/sink.go` | `Sink` 头尾窗口 + artifact 落盘（§3） |
| `internal/runtime/sandbox.go` | `nonInteractiveEnv` env 硬化（§4） |
| `internal/runtime/bash.go` | `Bash.Execute` 子进程 + cwd 持久化（§4） |
| `internal/tool/tool.go` | `Tool` 接口（§5） |
| `internal/tool/registry.go` | `Registry` 注册表（§5） |
| `internal/tool/tools.go` | 内置工具（read/write/glob/grep/bash） |
| `internal/tool/executor.go` | `executeTool`（审批+执行）（§6.2） |
| `internal/tool/search.go` | grep 双引擎（§7.1） |
| `internal/tool/cache.go` | `ScanCache` 目录缓存（§8） |
| `internal/agent/event.go` | 工具事件（toolStart/toolEnd） |
| `internal/agent/loop.go` | 工具循环（§6.1） |
| `internal/agent/agent.go` | Agent 加 tools/permission/mode/maxIterations |
| `cmd/agent/config.go` + `main.go` + `tui.go` | 接线（审批 mode、工具展示） |

---

## Task 1: 审批纯策略（`permission/policy.go`，TDD）

- [ ] **Step 1 写失败测试** `policy_test.go`

```go
package permission

import "testing"

func TestResolve(t *testing.T) {
	cases := []struct {
		tier Tier
		mode Mode
		want Decision
	}{
		{TierRead, ModeYolo, DecisionAllow},
		{TierExec, ModeYolo, DecisionAllow},
		{TierRead, ModeWrite, DecisionAllow},
		{TierWrite, ModeWrite, DecisionAllow},
		{TierExec, ModeWrite, DecisionPrompt},
		{TierRead, ModeAlwaysAsk, DecisionAllow},
		{TierWrite, ModeAlwaysAsk, DecisionPrompt},
		{TierExec, ModeAlwaysAsk, DecisionPrompt},
	}
	for _, c := range cases {
		if got := Resolve(c.tier, c.mode); got != c.want {
			t.Errorf("Resolve(%v,%v) = %v, want %v", c.tier, c.mode, got, c.want)
		}
	}
}
```

- [ ] **Step 2 红** → **Step 3 实现**（§2）→ **Step 4 绿**

```bash
cd /Users/cyanyumu/Projects/GoProject/einoclaw-build && env -u GOROOT go test ./internal/permission/ -v
```

---

## Task 2: OutputSink（`runtime/sink.go`，TDD）

- [ ] **Step 1 写失败测试** `sink_test.go`

```go
package runtime

import (
	"os"
	"strings"
	"testing"
)

func TestSinkNoTruncation(t *testing.T) {
	s := NewSink(100, 100)
	s.Write([]byte("short"))
	if s.Result() != "short" {
		t.Fatalf("Result = %q", s.Result())
	}
}

func TestSinkTruncatesHeadTail(t *testing.T) {
	s := NewSink(4, 4)
	s.Write([]byte("0123456789abcdef")) // 16 bytes，头4尾4，中间8被 elide
	r := s.Result()
	if !strings.Contains(r, "0123") || !strings.Contains(r, "cdef") {
		t.Fatalf("Result = %q", r)
	}
	if !strings.Contains(r, "elided") {
		t.Fatalf("Result 缺少 elided 标记: %q", r)
	}
}

func TestSinkOffloadsArtifact(t *testing.T) {
	dir := t.TempDir()
	s := NewSink(4, 4)
	s.SetArtifactDir(dir)
	s.Write([]byte("0123456789abcdef"))
	r := s.Result()
	if !strings.Contains(r, "artifact://") {
		t.Fatalf("Result 缺少 artifact 指针: %q", r)
	}
	// artifact 文件应存在且内容完整
	files, _ := os.ReadDir(dir)
	if len(files) != 1 {
		t.Fatalf("artifact 文件数 = %d, want 1", len(files))
	}
}
```

- [ ] **Step 2 红** → **Step 3 实现**（§3，含 `SetArtifactDir`）→ **Step 4 绿**

---

## Task 3: bash 运行时（`runtime/sandbox.go` + `bash.go`）

- [ ] **Step 1** `sandbox.go`：`nonInteractiveEnv()` 返回 `PAGER=cat`/`EDITOR=true`/`TERM=dumb`/`NO_COLOR=1`/`CI=true` 等（纯函数，加个简单断言测试）。

- [ ] **Step 2** `bash.go`：`Bash.Execute(ctx, command, sink)` 用 `exec.CommandContext` 跑 `bash -c`，stdout/stderr 进 sink；解析前缀 `cd <path> &&` 更新 cwd。

- [ ] **Step 3 验证**

```bash
env -u GOROOT go build ./... && env -u GOROOT go test ./internal/runtime/
```

---

## Task 4: Tool 接口 + 注册表（`tool.go` + `registry.go`，TDD）

- [ ] **Step 1 写失败测试** `registry_test.go`（用假 Tool 实现）

```go
package tool

import "testing"

type fakeTool struct{ name string }

func (f fakeTool) Name() string               { return f.name }
func (f fakeTool) Description() string        { return "d" }
func (f fakeTool) Parameters() map[string]any { return nil }
func (f fakeTool) Tier() permission.Tier      { return permission.TierRead }
func (f fakeTool) Execute(ctx context.Context, args map[string]any, sink *runtime.Sink) (string, error) {
	return "ok", nil
}

func TestRegistryRegisterGet(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeTool{name: "read"})
	if _, ok := r.Get("read"); !ok {
		t.Fatal("Get(read) 应为 ok")
	}
	if _, ok := r.Get("nope"); ok {
		t.Fatal("Get(nope) 应为 !ok")
	}
}

func TestRegistrySpecs(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeTool{name: "read"})
	specs := r.Specs()
	if len(specs) != 1 || specs[0].Name != "read" {
		t.Fatalf("specs = %+v", specs)
	}
}
```

- [ ] **Step 2 红** → **Step 3 实现**（§5）→ **Step 4 绿**

---

## Task 5: 内置工具（`tools.go`）

- [ ] 实现 `read_file`/`write_file`/`glob`/`bash`（`grep` 在 Task 8）。每个都实现 `Tool` 接口：
  - `read_file`：读文件，支持 offset/limit，结果进 sink。
  - `write_file`：写文件（TierWrite），返回确认。
  - `glob`：`filepath.Glob` 文件名匹配（TierRead）。
  - `bash`：包装 `runtime.Bash`（TierExec）。

- [ ] 验证构建

---

## Task 6: executeTool（`executor.go`，TDD）

- [ ] **Step 1 写失败测试**（用 fakeTool + yolo mode 断言执行；exec+write mode 断言被拒）

```go
func TestExecuteToolAllowAndPrompt(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeTool{name: "read", tier: permission.TierRead})
	r.Register(fakeTool{name: "bash", tier: permission.TierExec})

	e := NewExecutor(r, permission.ModeWrite)
	if out := e.Execute(context.Background(), toolCall{Name: "read", Args: "{}"}); out != "ok" {
		t.Fatalf("read 应执行，got %q", out)
	}
	if out := e.Execute(context.Background(), toolCall{Name: "bash", Args: "{}"}); !strings.Contains(out, "denied") {
		t.Fatalf("bash 应被拒，got %q", out)
	}
}
```

- [ ] **Step 2 红** → **Step 3 实现**（§6.2）→ **Step 4 绿**

---

## Task 7: agent 工具循环（`loop.go` + `event.go` + `agent.go`）

- [ ] **Step 1** `event.go` 加工具事件：`EventToolStart`/`EventToolEnd` + `ToolCall{Name, Args}`/`ToolResult{Name, Content}` 载荷。

- [ ] **Step 2** `agent.go`：`Agent` 加 `tools *tool.Registry`、`mode permission.Mode`、`maxIterations int`；`New` 加参数。

- [ ] **Step 3** `loop.go`：`consumeStream` 改为**返回累积消息**（`(message.Message, model.Usage)`，现有测试不改也能编译，因为调用处丢弃返回值）；`Run` 改成 §6.1 的工具循环（流式 → 累积 → 提工具调用 → 逐个执行 → 追加 tool 结果 → 循环直到无工具调用）。

- [ ] **Step 4** 补一个「工具循环」测试：fakeModel 第一次返回带工具调用、第二次返回纯文本，断言事件序列含 toolStart/toolEnd + 两轮 message。

- [ ] **Step 5 验证**

```bash
env -u GOROOT go test ./internal/agent/ -v && env -u GOROOT go build ./...
```

---

## Task 8: grep 结构化检索（`search.go`，TDD）

- [ ] **Step 1 写失败测试**：temp 目录写 `a.go`（含 `foo`）和 `b.go`（含 `bar`），断言 `Grep("foo", dir)` 只命中 a.go、`Grep("(bad` 非法 pattern 不报错降级字面量。

- [ ] **Step 2 红** → **Step 3 实现**（§7.1：`regexp` 主引擎，编译失败 fallback `regexp.QuoteMeta`；`regexp2` PCRE 兜底留作可选，P4 先 RE2+字面量）→ **Step 4 绿**

---

## Task 9: 目录清单缓存（`cache.go`，TDD）

- [ ] **Step 1 写失败测试**：`ScanCache` 首次 `Get` 未命中、写入后命中且内容一致、`Invalidate` 后未命中、TTL 过期后未命中。

- [ ] **Step 2 红** → **Step 3 实现**（§8）→ **Step 4 绿**

---

## Task 10: 接线（config + main + tui）

- [ ] **Step 1** `config.go` 加 `approval_mode`（默认 yolo）；`main.go` 建 registry（注册内置工具）+ 建 agent（带 tools/mode）+ 建 executor 传给 TUI。

- [ ] **Step 2** `tui.go` 处理 `EventToolStart`/`EventToolEnd`：显示「调用什么工具 + 结果截断预览」（复用阶段5 的工具渲染思路，但用 `agent.AgentEvent`）。

- [ ] **Step 3 构建 + vet + test + 手动验收**

```bash
env -u GOROOT go build ./... && env -u GOROOT go vet ./... && env -u GOROOT go test ./... 2>&1 | tail -10
# 手动：让 agent ls/读文件/跑 go version；把 approval_mode 设 write 看 bash 被拒
```

---

## 自检

- **spec 覆盖**：P4 的 9 项产出 → Task 1-10 全覆盖。
- **类型一致性**：`permission.Tier/Mode/Decision`（Task 1）被 `tool.Tier`/`executor`（Task 4/6）消费；`runtime.Sink`（Task 2）被 `tool.Execute`（Task 5/6）消费；`tool.Registry`（Task 4）被 `agent`（Task 7）消费。
- **无占位符**：可测部分（Task 1/2/4/6/8/9）测试全量内联。
