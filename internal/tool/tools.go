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

// Builtins 返回内置工具。bash 为该 agent 专属实例（cwd 隔离）；store 为会话产物存储（可 nil）。
func Builtins(bash *runtime.Bash, store *runtime.ArtifactStore) []Tool {
	return []Tool{
		readFileTool{store: store},
		writeFileTool{},
		globTool{},
		grepTool{},
		bashTool{bash: bash},
	}
}

// ---------- read_file ----------

type readFileTool struct {
	store *runtime.ArtifactStore
}

func (readFileTool) Name() string { return "read_file" }
func (readFileTool) Description() string {
	return "按行读取文件内容；offset 为起始行号（1 起），limit 为读取行数（默认 300）。file_path 支持 artifact://N 读取被截断的完整工具输出。"
}
func (readFileTool) Parameters() map[string]any {
	return map[string]any{
		"file_path": map[string]any{"type": "string", "description": "文件路径，或 artifact://N"},
		"offset":    map[string]any{"type": "integer", "description": "起始行号（1 起）"},
		"limit":     map[string]any{"type": "integer", "description": "读取行数，默认 300"},
	}
}
func (readFileTool) Required() []string       { return []string{"file_path"} }
func (readFileTool) Tier() permission.Tier    { return permission.TierRead }
func (readFileTool) Concurrency() Concurrency { return ConcurrencyShared }

const defaultReadLines = 300

func (t readFileTool) Execute(ctx context.Context, args map[string]any, sink *runtime.Sink) error {
	path, _ := args["file_path"].(string)
	if path == "" {
		return fmt.Errorf("file_path 必填")
	}
	if strings.HasPrefix(path, runtime.ArtifactScheme) {
		if t.store == nil {
			return fmt.Errorf("本会话没有产物目录，无法读取 %s", path)
		}
		p, err := t.store.Resolve(path)
		if err != nil {
			return err
		}
		path = p
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	start := 0
	if off, ok := args["offset"].(float64); ok && off > 1 {
		start = min(int(off)-1, len(lines))
	}
	limit := defaultReadLines
	if lim, ok := args["limit"].(float64); ok && lim > 0 {
		limit = int(lim)
	}
	end := min(start+limit, len(lines))
	sink.Write([]byte(strings.Join(lines[start:end], "\n")))
	if end < len(lines) {
		fmt.Fprintf(sink, "\n[共 %d 行，已显示 %d-%d；继续读取请用 offset=%d]", len(lines), start+1, end, end+1)
	}
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
func (writeFileTool) Required() []string       { return []string{"file_path", "content"} }
func (writeFileTool) Tier() permission.Tier    { return permission.TierWrite }
func (writeFileTool) Concurrency() Concurrency { return ConcurrencyExclusive }

func (writeFileTool) Execute(ctx context.Context, args map[string]any, sink *runtime.Sink) error {
	path, _ := args["file_path"].(string)
	content, _ := args["content"].(string)
	if path == "" {
		return fmt.Errorf("file_path 必填")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(sink, "wrote %d bytes to %s", len(content), path)
	return nil
}

// ---------- glob ----------

type globTool struct{}

func (globTool) Name() string        { return "glob" }
func (globTool) Description() string { return "按 pattern 匹配文件名" }
func (globTool) Parameters() map[string]any {
	return map[string]any{"pattern": map[string]any{"type": "string"}}
}
func (globTool) Required() []string       { return []string{"pattern"} }
func (globTool) Tier() permission.Tier    { return permission.TierRead }
func (globTool) Concurrency() Concurrency { return ConcurrencyShared }

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
func (bashTool) Required() []string       { return []string{"command"} }
func (bashTool) Tier() permission.Tier    { return permission.TierExec }
func (bashTool) Concurrency() Concurrency { return ConcurrencyExclusive }

func (b bashTool) Execute(ctx context.Context, args map[string]any, sink *runtime.Sink) error {
	command, _ := args["command"].(string)
	if command == "" {
		return fmt.Errorf("command 必填")
	}
	return b.bash.Execute(ctx, command, sink)
}
