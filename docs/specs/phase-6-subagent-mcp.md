# Phase 6 详细设计：Subagent + MCP

> 状态：**待评审** · 所属 spec：[harness-rearchitecture.md](harness-rearchitecture.md) · 前置：P4（工具）、P4.5（interrupt/resume）、P5（记忆）
> 本阶段落地 **Context Isolation**：子 agent 独立 context 完成任务、只交还结构化产出；并接入 MCP 外部工具。

---

## 0. 目标与边界

### 本阶段产出（P6 完成时）

1. `internal/subagent/manager.go` —— `Definition` + `Manager`（派发子 agent，独立 session）。
2. `internal/tool/task_tool.go` —— `task` 工具（模型调用它派子 agent）。
3. `internal/tool/mcp.go` —— MCP 客户端（stdio）+ 工具名归一 `mcp__<server>_<tool>`。
4. `cmd/agent/main.go` —— 建 subagent manager + 接 MCP server。

### 本阶段不做（stretch）

- **markdown frontmatter 声明**（先内置 Definition，从文件加载是 stretch）。
- **递归深度门控 / yield 结构化产出 schema 校验 / run monitor（软预算）**——P6 先做「子 agent 跑完返回最终文本」。
- **MCP http/sse 传输 + 重连/健康检查**——P6 只做 stdio。
- **MCP OAuth / 资源 / prompts**——只做 tools。

### 验收标准

- `env -u GOROOT go build ./...` + `go vet ./...` + `go test ./...` 通过。
- **Context Isolation**：主 agent 调 `task` 让子 agent「分析数据库设计」，子 agent 返回几 KB 的结构化结论，主 agent 的上下文不被子 agent 的历史污染。
- **MCP**：配一个 stdio MCP server 后，其工具以 `mcp__<server>_<tool>` 可被调用。

---

## 1. 参照 oh-my-pi 的核心原则

### 1.1 Context Isolation：独立 context，只交还结构化产出

子 agent 有**自己的 context / tools / task**，跑完只把结构化结果（几 KB）交还父 agent，而非复制父的整个 context。P6 落地：子 agent = 新 `agent.New` + 独立 `session`（空历史），父 agent 只拿到它的最终文本。

### 1.2 借用，而非复制

子 agent **借用**父的模型客户端 + 工具注册表（共享同一份，不重建），但 session/context 独立。子 agent 跑 **headless**（yolo，无 approver）——父的审批是边界。

### 1.3 MCP = 多一个工具源

MCP 的工具归一成 `mcp__<server>_<tool>`，进同一个 registry，和内置工具一起给模型。**每 server 故障隔离**——一个 server 挂不影响其它。

---

## 2. Subagent 声明 + 派发（`internal/subagent/manager.go`）

```go
package subagent

type Definition struct {
	Name         string
	Description  string
	SystemPrompt string
}

type Manager struct {
	model  model.Model
	tools  *tool.Registry
	memory memory.Recaller // 借用父的记忆，子 agent 也能召回
	defs   map[string]Definition
}

func NewManager(m model.Model, tools *tool.Registry, mem memory.Recaller, defs []Definition) *Manager

// Run 派发一个子 agent：独立 session、独立 context，返回最终结果文本。
func (m *Manager) Run(ctx context.Context, name, prompt string) (string, error) {
	def, ok := m.defs[name]
	if !ok {
		return "", fmt.Errorf("unknown subagent %q", name)
	}
	// 子 agent：同模型、同工具、同记忆，但 headless（yolo，无 approver）
	sub := agent.New(def.Name, def.SystemPrompt, m.model, m.tools, permission.ModeYolo, nil, m.memory)
	subSession := session.New(def.Name, &session.MemoryStorage{}) // 独立空上下文

	var result string
	for ev := range sub.Run(ctx, []message.Message{message.NewUserMessage(prompt)}) {
		if ev.Type == agent.EventMessageEnd {
			result = messageText(ev.Ended.Message) // 最后一个定稿消息 = 最终结论
		}
	}
	return result, nil
}
```

> **这就是 Context Isolation 的核心**：`subSession` 是全新的空 session，子 agent 看不到父的对话历史；父只拿到 `result` 字符串，不拿子 agent 的中间过程。

---

## 3. `task` 工具（`internal/tool/task_tool.go`）

```go
type taskTool struct {
	mgr *subagent.Manager
}

func NewTaskTool(mgr *subagent.Manager) Tool {
	return taskTool{mgr: mgr}
}

func (taskTool) Name() string        { return "task" }
func (taskTool) Description() string { return "派一个子 agent 完成独立任务，返回结构化结论" }
func (taskTool) Parameters() map[string]any {
	return map[string]any{
		"subagent": map[string]any{"type": "string"}, // 子 agent 名字
		"prompt":   map[string]any{"type": "string"},
	}
}
func (taskTool) Tier() permission.Tier { return permission.TierExec }

func (t taskTool) Execute(ctx context.Context, args map[string]any, sink *runtime.Sink) error {
	name, _ := args["subagent"].(string)
	prompt, _ := args["prompt"].(string)
	result, err := t.mgr.Run(ctx, name, prompt)
	if err != nil {
		return err
	}
	sink.Write([]byte(result))
	return nil
}
```

---

## 4. MCP 客户端（`internal/tool/mcp.go`）

用 `github.com/mark3labs/mcp-go`（已在 module cache）。

```go
// MCPConfig 描述一个 stdio MCP server。
type MCPConfig struct {
	Name    string   `yaml:"name"`
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
}

// ConnectMCP 连接一个 stdio server，把它的工具归一成 mcp__<name>_<tool> 注册进 registry。
func ConnectMCP(ctx context.Context, reg *Registry, cfg MCPConfig) error {
	// 1. mcp.NewClient (stdio) + Initialize + ListTools
	// 2. 每个工具包装成 mcpTool{server: name, name: toolName}，Name() = "mcp__"+name+"_"+toolName
	// 3. reg.Register(...)
}
```

```go
// mcpTool 包装一个 MCP 工具，实现 Tool 接口。
type mcpTool struct {
	server   string
	name     string
	desc     string
	schema   map[string]any
	callTool func(ctx context.Context, args map[string]any) (string, error)
}

func (m mcpTool) Name() string { return "mcp__" + m.server + "_" + m.name }
func (m mcpTool) Tier() permission.Tier { return permission.TierRead } // 默认 read，可后续按 schema 细分
```

> 工具名归一 `mcp__<server>_<tool>` 是纯函数，可单测；连接/协议用 mcp-go，P6 只做 stdio。

---

## 5. 接线（`cmd/agent/main.go` + `config.yaml`）

```yaml
# config.yaml 增
mcp_servers:
  - name: filesystem
    command: npx
    args: ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
```

```go
// main.go
defs := []subagent.Definition{
	{Name: "reviewer", Description: "代码审查", SystemPrompt: "你是代码审查专家，分析代码问题并给出结论。"},
}
mgr := subagent.NewManager(m, registry, mem, defs)
registry.Register(tool.NewTaskTool(mgr))

for _, srv := range cfg.MCPServers {
	if err := tool.ConnectMCP(context.Background(), registry, srv); err != nil {
		log.Printf("MCP server %s 连接失败: %v", srv.Name, err) // 故障隔离，不崩
	}
}
```

---

## 6. 边界情况与错误处理

| 情况 | 处理 |
|---|---|
| 子 agent 名不存在 | `task` 返回 `unknown subagent`，不崩 |
| 子 agent 跑挂（模型错误） | 返回错误，父 agent 看到 tool error |
| MCP server 连不上 | log 提示，跳过该 server，不影响其它工具 |
| MCP 工具调用失败 | 返回 tool error |
| 子 agent 递归（子调子） | P6 不限制（递归门控是 stretch），靠 maxIterations 兜底 |

---

## 7. 对外契约

| 类型/函数 | 包 | 后续谁依赖 |
|---|---|---|
| `subagent.Manager.Run` | `internal/subagent` | `internal/tool`（task 工具）、P6.5 steering |
| `tool.NewTaskTool` / `tool.ConnectMCP` | `internal/tool` | `cmd/agent` |
| `mcp__<server>_<tool>` 命名约定 | `internal/tool` | 工具展示 |

---

## 8. 待评审点

1. **子 agent 用「新 Agent + 独立空 session」实现 Context Isolation**（不建递归门控/yield/预算阶梯，这些是 stretch）——接受吗？
2. **子 agent headless（yolo，无 approver）**，父的审批是边界——接受吗？
3. **子 agent 返回「最终文本」而非 JSON schema 结构化产出**（yield+schema 是 stretch）——接受吗？
4. **MCP 只做 stdio transport**（http/sse、重连、OAuth 是 stretch）——接受吗？
5. **子 agent 递归不设深度门控**（靠 maxIterations 兜底，递归门控是 stretch）——接受吗？
