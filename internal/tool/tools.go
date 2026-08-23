package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
)

// Builtins 返回 P4 首批内置工具。
func Builtins(bash *runtime.Bash) []Tool {
	return []Tool{
		readFileTool{},
		writeFileTool{},
		globTool{},
		grepTool{},
		bashTool{bash: bash},
	}
}

// ---------- read_file ----------

type readFileTool struct{}

func (readFileTool) Name() string        { return "read_file" }
func (readFileTool) Description() string { return "读取文件内容，可指定 offset/limit" }
func (readFileTool) Parameters() map[string]any {
	return map[string]any{
		"file_path": map[string]any{"type": "string"},
		"offset":    map[string]any{"type": "integer"},
		"limit":     map[string]any{"type": "integer"},
	}
}
func (readFileTool) Tier() permission.Tier        { return permission.TierRead }
func (readFileTool) Concurrency() Concurrency     { return ConcurrencyShared }

func (readFileTool) Execute(ctx context.Context, args map[string]any, sink *runtime.Sink) error {
	path, _ := args["file_path"].(string)
	if path == "" {
		return fmt.Errorf("file_path 必填")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(data)
	if off, ok := args["offset"].(float64); ok && off > 0 {
		start := int(off)
		if start < len(text) {
			text = text[start:]
		}
	}
	if lim, ok := args["limit"].(float64); ok && lim > 0 {
		l := int(lim)
		if l < len(text) {
			text = text[:l]
		}
	}
	sink.Write([]byte(text))
	return nil
}

// ---------- write_file ----------

type writeFileTool struct{}

func (writeFileTool) Name() string        { return "write_file" }
func (writeFileTool) Description() string { return "写入文件（覆盖）" }
func (writeFileTool) Parameters() map[string]any {
	return map[string]any{
		"file_path": map[string]any{"type": "string"},
		"content":   map[string]any{"type": "string"},
	}
}
func (writeFileTool) Tier() permission.Tier        { return permission.TierWrite }
func (writeFileTool) Concurrency() Concurrency     { return ConcurrencyExclusive }

func (writeFileTool) Execute(ctx context.Context, args map[string]any, sink *runtime.Sink) error {
	path, _ := args["file_path"].(string)
	content, _ := args["content"].(string)
	if path == "" {
		return fmt.Errorf("file_path 必填")
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return err
	}
	sink.Write([]byte(fmt.Sprintf("wrote %d bytes to %s", len(content), path)))
	return nil
}

// ---------- glob ----------

type globTool struct{}

func (globTool) Name() string        { return "glob" }
func (globTool) Description() string { return "按 pattern 匹配文件名" }
func (globTool) Parameters() map[string]any {
	return map[string]any{"pattern": map[string]any{"type": "string"}}
}
func (globTool) Tier() permission.Tier        { return permission.TierRead }
func (globTool) Concurrency() Concurrency     { return ConcurrencyShared }

func (globTool) Execute(ctx context.Context, args map[string]any, sink *runtime.Sink) error {
	pattern, _ := args["pattern"].(string)
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		sink.Write([]byte("(no matches)"))
		return nil
	}
	sink.Write([]byte(strings.Join(matches, "\n")))
	return nil
}

// ---------- bash ----------

type bashTool struct {
	bash *runtime.Bash
}

func (bashTool) Name() string        { return "bash" }
func (bashTool) Description() string { return "执行 shell 命令" }
func (bashTool) Parameters() map[string]any {
	return map[string]any{"command": map[string]any{"type": "string"}}
}
func (bashTool) Tier() permission.Tier        { return permission.TierExec }
func (bashTool) Concurrency() Concurrency     { return ConcurrencyExclusive }

func (b bashTool) Execute(ctx context.Context, args map[string]any, sink *runtime.Sink) error {
	command, _ := args["command"].(string)
	if command == "" {
		return fmt.Errorf("command 必填")
	}
	return b.bash.Execute(ctx, command, sink)
}
