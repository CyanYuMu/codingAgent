package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
)

// Builtins 返回内置工具。bash 为该 agent 专属实例（cwd 隔离）；store 为会话产物存储（可 nil）。
func Builtins(bash *runtime.Bash, store *runtime.ArtifactStore) []Tool {
	return []Tool{
		&readFileTool{store: store},
		writeFileTool{},
		globTool{},
		grepTool{},
		bashTool{bash: bash},
	}
}

// ---------- read_file ----------

type readFileTool struct {
	store *runtime.ArtifactStore

	// 会话内已读记录：同一文件、mtime+size 未变、请求区间已被已读区间覆盖时不再重复返回内容
	// ——模型在长会话里反复整读同一文件是最常见的 token 浪费之一。记录随工具实例（≈会话）存活，
	// 换会话（/new /resume）由宿主调用 ResetConv 清空，否则「内容仍在上文中」对新会话是谎话。
	mu    sync.Mutex
	reads map[string]*readRecord
}

// readRecord 一个文件的已读状态：内容指纹（mtime+size）与已读行区间（1 起闭区间，不重叠有序）。
type readRecord struct {
	mtime, size int64
	ranges      []lineRange
}

type lineRange struct{ from, to int }

// ResetConv 清空会话级状态（宿主在换会话时调用）。
func (t *readFileTool) ResetConv() {
	t.mu.Lock()
	t.reads = nil
	t.mu.Unlock()
}

func (*readFileTool) Name() string { return "read_file" }
func (*readFileTool) Description() string {
	return "按行读取文件内容；offset 为起始行号（1 起），limit 为读取行数（默认 300）。" +
		"file_path 还支持会话内 URL：artifact://N（被截断的完整工具输出）、agent://<子agent名>（它的完整产出）、history://<子agent名>（它的转录）。"
}
func (*readFileTool) Parameters() map[string]any {
	return map[string]any{
		"file_path": map[string]any{"type": "string", "description": "文件路径，或 artifact://N"},
		"offset":    map[string]any{"type": "integer", "description": "起始行号（1 起）"},
		"limit":     map[string]any{"type": "integer", "description": "读取行数，默认 300"},
	}
}
func (*readFileTool) Required() []string       { return []string{"file_path"} }
func (*readFileTool) Tier() permission.Tier    { return permission.TierRead }
func (*readFileTool) Concurrency() Concurrency { return ConcurrencyShared }

const defaultReadLines = 300

func (t *readFileTool) Execute(ctx context.Context, args map[string]any, sink *runtime.Sink) error {
	path, _ := args["file_path"].(string)
	if path == "" {
		return fmt.Errorf("file_path 必填")
	}
	sessionURL := strings.Contains(path, "://") // 会话内 URL：artifact / agent / history 都由产物存储路由
	if sessionURL {
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
	var mtime, size int64
	if st, err := os.Stat(path); err == nil {
		mtime, size = st.ModTime().UnixNano(), st.Size()
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
	// 空文件或 offset 越过 EOF 时没有可读内容，跳过去重记录（否则会产生「读过第 1-1 行」这类误导提示）
	hasRange := start < end && size > 0

	// 会话内去重只对真实文件路径生效（会话内 URL 的内容不会在上文里重复）
	if !sessionURL && hasRange && t.alreadyRead(path, mtime, size, start+1, end) {
		fmt.Fprintf(sink, "文件未变更（上次读过第 %d-%d 行），内容仍在上文中；需要其它区间就带 offset/limit 再读。", start+1, end)
		return nil
	}
	sink.Write([]byte(strings.Join(lines[start:end], "\n")))
	if end < len(lines) {
		fmt.Fprintf(sink, "\n[共 %d 行，已显示 %d-%d；继续读取请用 offset=%d]", len(lines), start+1, end, end+1)
	}
	if !sessionURL && hasRange {
		t.recordRead(path, mtime, size, start+1, end)
	}
	return nil
}

// alreadyRead 判断文件未变更且请求区间已被已读区间并集覆盖。
func (t *readFileTool) alreadyRead(path string, mtime, size int64, from, to int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.reads[path]
	if r == nil || r.mtime != mtime || r.size != size {
		return false
	}
	need := to - from + 1
	got := 0
	for _, rg := range r.ranges { // 区间互不重叠：剪裁后直接累加即并集大小
		got += max(0, min(rg.to, to)-max(rg.from, from)+1)
	}
	return got >= need
}

// recordRead 合并记录一次读取；文件变更（指纹不符）则重置区间历史。
func (t *readFileTool) recordRead(path string, mtime, size int64, from, to int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.reads == nil {
		t.reads = map[string]*readRecord{}
	}
	r := t.reads[path]
	if r == nil || r.mtime != mtime || r.size != size {
		r = &readRecord{mtime: mtime, size: size}
		t.reads[path] = r
	}
	r.ranges = insertRange(r.ranges, from, to)
}

// insertRange 把 [from,to] 并入有序不重叠的区间列表（相邻合并）。
func insertRange(ranges []lineRange, from, to int) []lineRange {
	merged := lineRange{from, to}
	out := make([]lineRange, 0, len(ranges)+1)
	for _, r := range ranges {
		if r.to+1 < merged.from || merged.to+1 < r.from {
			out = append(out, r)
		} else {
			merged.from = min(merged.from, r.from)
			merged.to = max(merged.to, r.to)
		}
	}
	out = append(out, merged)
	slices.SortFunc(out, func(a, b lineRange) int { return a.from - b.from })
	return out
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
