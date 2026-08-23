package tool

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
)

// MCPConfig 描述一个 stdio MCP server。
type MCPConfig struct {
	Name    string   `yaml:"name"`
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
}

// mcpTool 包装一个 MCP 工具，归一成 mcp__<server>_<tool>。
type mcpTool struct {
	server   string
	name     string
	desc     string
	schema   map[string]any
	callTool func(ctx context.Context, args map[string]any) (string, error)
}

func (m mcpTool) Name() string               { return "mcp__" + m.server + "_" + m.name }
func (m mcpTool) Description() string        { return m.desc }
func (m mcpTool) Parameters() map[string]any { return m.schema }
func (m mcpTool) Tier() permission.Tier      { return permission.TierRead }
func (m mcpTool) Concurrency() Concurrency   { return ConcurrencyShared }

func (m mcpTool) Execute(ctx context.Context, args map[string]any, sink *runtime.Sink) error {
	result, err := m.callTool(ctx, args)
	if err != nil {
		return err
	}
	sink.Write([]byte(result))
	return nil
}

// ConnectMCP 连接一个 stdio MCP server，把工具归一注册进 registry。
// client 保持打开（进程退出时随子进程一起结束），不在此关闭。
func ConnectMCP(ctx context.Context, reg *Registry, cfg MCPConfig) error {
	c, err := client.NewStdioMCPClient(cfg.Command, nil, cfg.Args...)
	if err != nil {
		return err
	}

	if _, err := c.Initialize(ctx, mcp.InitializeRequest{}); err != nil {
		return err
	}
	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return err
	}

	for _, t := range res.Tools {
		t := t // 避免闭包捕获循环变量
		mt := mcpTool{
			server: cfg.Name,
			name:   t.Name,
			desc:   t.Description,
			schema: inputSchemaToMap(t.InputSchema),
			callTool: func(ctx context.Context, args map[string]any) (string, error) {
				cr, err := c.CallTool(ctx, mcp.CallToolRequest{
					Params: mcp.CallToolParams{Name: t.Name, Arguments: args},
				})
				if err != nil {
					return "", err
				}
				return toolResultText(cr), nil
			},
		}
		reg.Register(mt)
	}
	return nil
}

// inputSchemaToMap 从 MCP 工具的 JSON Schema 里提取 properties（给模型的参数定义）。
func inputSchemaToMap(schema any) map[string]any {
	b, err := json.Marshal(schema)
	if err != nil {
		return nil
	}
	var full map[string]any
	if err := json.Unmarshal(b, &full); err != nil {
		return nil
	}
	if props, ok := full["properties"].(map[string]any); ok {
		return props
	}
	return nil
}

// toolResultText 提取 MCP 工具结果的文本内容。
func toolResultText(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}
