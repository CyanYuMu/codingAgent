# Phase 6 Subagent + MCP 实现计划

> **Goal:** Context Isolation（子 agent 独立 context 交还结论）+ 接入 MCP 外部工具。
>
> **Architecture:** `subagent.Manager`（派发子 agent）→ `task` 工具 → `mcp__<server>_<tool>` 归一进 registry。
>
> **Tech Stack:** `github.com/mark3labs/mcp-go/mcp`（已在 module cache）+ Go stdlib。
>
> **Spec / 设计:** [../specs/phase-6-subagent-mcp.md](../specs/phase-6-subagent-mcp.md)（§2-§5 含完整代码）。

## Global Constraints

- 构建/运行统一 `env -u GOROOT go ...`。
- **task 工具放 `internal/subagent` 包**（不在 `internal/tool`），避免 tool→subagent→tool 循环依赖。
- 每任务末尾 `go build ./...` + `go test ./...` 通过。

---

## 文件结构

| 文件 | 职责 |
|---|---|
| `internal/subagent/manager.go` | `Definition` + `Manager`（派发子 agent，独立 session）（§2） |
| `internal/subagent/task.go` | `task` 工具（实现 tool.Tool）（§3） |
| `internal/subagent/manager_test.go` | 「unknown subagent」单测 |
| `internal/tool/mcp.go` | MCP 客户端 + `mcpTool` + `ConnectMCP`（§4） |
| `internal/tool/mcp_test.go` | 工具名归一单测 |
| `cmd/agent/config.go` + `main.go` | `mcp_servers` 配置 + 接线（§5） |

---

## Task 1: subagent.Manager（TDD「unknown subagent」）

- [ ] **Step 1 写失败测试** `manager_test.go`

```go
func TestManagerUnknownSubagent(t *testing.T) {
	m := NewManager(nil, nil, nil, []Definition{{Name: "reviewer"}})
	if _, err := m.Run(context.Background(), "nope", "x"); err == nil || !strings.Contains(err.Error(), "unknown subagent") {
		t.Fatalf("未知子 agent 应报错，got %v", err)
	}
}
```

- [ ] **Step 2 红** → **Step 3 实现**（§2：`Definition`/`Manager`/`NewManager`/`Run`，`Run` 里 `agent.New(..., ModeYolo, nil, memory)` + 独立 `session.New(def.Name, &MemoryStorage{})`，收集最后一个 `EventMessageEnd` 文本）→ **Step 4 绿**

---

## Task 2: `task` 工具（`subagent/task.go`）

- [ ] 实现 `taskTool`（`run func(ctx,name,prompt)(string,error)` 字段 + `Execute` 读 args 调 run 写 sink）；`NewTaskTool(run)`。

---

## Task 3: MCP 客户端（`tool/mcp.go`，TDD 归一）

- [ ] **Step 1 加依赖** `env -u GOROOT go get github.com/mark3labs/mcp-go/mcp`。

- [ ] **Step 2 写失败测试** `mcp_test.go`

```go
func TestMCPToolNameNormalization(t *testing.T) {
	mt := mcpTool{server: "filesystem", name: "read_file"}
	if mt.Name() != "mcp__filesystem_read_file" {
		t.Fatalf("Name = %q", mt.Name())
	}
}
```

- [ ] **Step 3 红** → **Step 4 实现**（`mcpTool` + `ConnectMCP`：stdio `mcp.NewClient` + `Initialize` + `ListTools`，包装每个工具注册）→ **Step 5 绿**

---

## Task 4: 接线（config + main）

- [ ] `config.go` 加 `MCPServers []MCPConfig yaml:"mcp_servers"` + `MCPConfig{Name, Command, Args}`。
- [ ] `main.go`：建 `subagent.NewManager(...)`（内置 reviewer 定义）+ `registry.Register(subagent.NewTaskTool(mgr.Run))`；遍历 `cfg.MCPServers` 调 `tool.ConnectMCP`（失败 log 不崩）。

- [ ] 构建 + vet + test。

---

## 自检

- **spec 覆盖**：P6 的 4 项产出 → Task 1-4 全覆盖。
- **类型一致性**：`subagent.Manager.Run`（Task 1）被 `task` 工具（Task 2）消费；`tool.ConnectMCP`（Task 3）被 main（Task 4）消费。
- **无占位符**：可测部分（Task 1/3）测试全量内联。
