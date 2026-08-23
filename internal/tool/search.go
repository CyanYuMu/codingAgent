package tool

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
)

// grepMatches 在 root 下搜索匹配 pattern 的行，返回 "path:line: content" 形式。
// 正则编译失败时降级为字面量搜索（regexp.QuoteMeta），永不因 pattern 非法而报错。
func grepMatches(pattern, root string, maxMatches int) ([]string, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		re = regexp.MustCompile(regexp.QuoteMeta(pattern)) // 降级字面量
	}

	var matches []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 跳过不可访问项
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "vendor" || strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			return nil
		}
		if len(matches) >= maxMatches {
			return fs.SkipAll
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				matches = append(matches, fmt.Sprintf("%s:%d: %s", path, i+1, line))
				if len(matches) >= maxMatches {
					return fs.SkipAll
				}
			}
		}
		return nil
	})
	return matches, err
}

// ---------- grep 工具 ----------

type grepTool struct{}

func (grepTool) Name() string        { return "grep" }
func (grepTool) Description() string { return "按正则搜索文件内容" }
func (grepTool) Parameters() map[string]any {
	return map[string]any{
		"pattern": map[string]any{"type": "string"},
		"path":    map[string]any{"type": "string"},
	}
}
func (grepTool) Tier() permission.Tier        { return permission.TierRead }
func (grepTool) Concurrency() Concurrency     { return ConcurrencyShared }

func (grepTool) Execute(ctx context.Context, args map[string]any, sink *runtime.Sink) error {
	pattern, _ := args["pattern"].(string)
	root, _ := args["path"].(string)
	if root == "" {
		root = "."
	}
	matches, err := grepMatches(pattern, root, 50)
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
