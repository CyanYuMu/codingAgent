package context

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"einoclaw-build/internal/message"
)

// 文件活动的读/写位标记。
const (
	opRead  uint8 = 1 << 0
	opWrite uint8 = 1 << 1
)

// fileTools 文件活动来源：tool 名 → 读/写位。edit（M4）落地后自然生效。
var fileTools = map[string]uint8{
	"read_file":  opRead,
	"write_file": opWrite,
	"edit":       opRead | opWrite,
}

const (
	filesTreeLimit = 20 // <files> 树的文件上限（超出计数省略）
	recentFilesMax = 5  // 压缩后恢复消息里列出的最近文件数
)

// fileAct 单个文件的累计活动：读写位 + 最后一次活动的消息位置（排序用）。
type fileAct struct {
	ops  uint8
	last int
}

// collectFileActivity 扫描 assistant 消息里的 tool_call，累计每个文件的读/写与最后活动位置。
// 纯函数；忽略非文件工具、空路径与会话内 URL（artifact:// 等不是文件）。
func collectFileActivity(msgs []message.Message) map[string]*fileAct {
	acts := map[string]*fileAct{}
	for i, m := range msgs {
		if m.Role != message.RoleAssistant {
			continue
		}
		for _, b := range m.Blocks {
			if b.Kind != message.BlockToolCall || b.ToolCall == nil {
				continue
			}
			ops, ok := fileTools[b.ToolCall.Name]
			if !ok {
				continue
			}
			path, ok := toolCallFilePath(b.ToolCall.Args)
			if !ok {
				continue
			}
			a := acts[path]
			if a == nil {
				a = &fileAct{}
				acts[path] = a
			}
			a.ops |= ops
			a.last = i
		}
	}
	return acts
}

// toolCallFilePath 从 JSON 参数里取 file_path；空串或会话内 URL 不算文件。
func toolCallFilePath(args string) (string, bool) {
	var a struct {
		FilePath string `json:"file_path"`
	}
	if json.Unmarshal([]byte(args), &a) != nil || a.FilePath == "" {
		return "", false
	}
	if strings.Contains(a.FilePath, "://") {
		return "", false
	}
	return filepath.Clean(a.FilePath), true
}

// sortedFilePaths 按最近活动降序（同位置按路径字典序）返回文件路径。
func sortedFilePaths(acts map[string]*fileAct) []string {
	files := make([]string, 0, len(acts))
	for p := range acts {
		files = append(files, p)
	}
	slices.SortFunc(files, func(a, b string) int {
		la, lb := acts[a].last, acts[b].last
		if la != lb {
			return lb - la
		}
		return strings.Compare(a, b)
	})
	return files
}

// FileOpsTree 渲染 <files> 树：按目录分组、(Read)/(Write)/(RW) 标记、按最近活动排序、
// 上限 limit 个文件（超出计数省略）。没有文件活动时返回空串。
// 压缩时附在摘要后落盘——六字段摘要里的「文件 / 产物」是模型的转述，这里是确定性事实。
func FileOpsTree(msgs []message.Message, limit int) string {
	acts := collectFileActivity(msgs)
	if len(acts) == 0 {
		return ""
	}
	files := sortedFilePaths(acts)
	elided := 0
	if limit > 0 && len(files) > limit {
		elided = len(files) - limit
		files = files[:limit]
	}

	type entry struct {
		path string
		act  *fileAct
	}
	var kept []entry
	for _, p := range files {
		kept = append(kept, entry{p, acts[p]})
	}

	var lines []string
	i := 0
	for i < len(kept) {
		dir := filepath.Dir(kept[i].path)
		j := i
		for j < len(kept) && filepath.Dir(kept[j].path) == dir {
			j++
		}
		group := kept[i:j]
		if dir == "." { // 根下：直接文件名
			for _, e := range group {
				lines = append(lines, e.path+" "+opMark(e.act.ops))
			}
		} else if len(group) == 1 { // 单文件目录：整行路径，省一行
			lines = append(lines, group[0].path+" "+opMark(group[0].act.ops))
		} else {
			lines = append(lines, dir+"/")
			for _, e := range group {
				lines = append(lines, "  "+filepath.Base(e.path)+" "+opMark(e.act.ops))
			}
		}
		i = j
	}
	if elided > 0 {
		lines = append(lines, fmt.Sprintf("[…%d files elided…]", elided))
	}
	return "<files>\n" + strings.Join(lines, "\n") + "\n</files>"
}

// recentFilesText 压缩后恢复消息用的最近文件清单（最近活动优先，上限 limit）。无活动返回空串。
func recentFilesText(msgs []message.Message, limit int) string {
	acts := collectFileActivity(msgs)
	if len(acts) == 0 {
		return ""
	}
	files := sortedFilePaths(acts)
	if len(files) > limit {
		files = files[:limit]
	}
	return "<recent-files>\n" + strings.Join(files, "\n") + "\n</recent-files>"
}

// opMark 渲染读写标记。
func opMark(ops uint8) string {
	switch {
	case ops&opRead != 0 && ops&opWrite != 0:
		return "(RW)"
	case ops&opWrite != 0:
		return "(Write)"
	default:
		return "(Read)"
	}
}
