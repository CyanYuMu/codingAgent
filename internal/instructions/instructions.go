// Package instructions 加载「项目指令层」（L1）：用户级与项目各级目录里的 AGENTS.md / CLAUDE.md，
// 展开其中的 @import，并把 RULES.md 当作粘性规则渲染在最后。
//
// 这一层回答的是「模型凭什么知道这个项目的约定」——构建命令、目录规范、不许碰的东西。
// 没有它，模型每次都得靠猜或者靠用户重复交代。
package instructions

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

const (
	maxImportHops    = 5     // @import 的递归跳数上限
	defaultLimit     = 32768 // 整块字符预算
	importTrimSuffix = `.,;:!?)]}"'。，；：！？）】`
)

// candidateNames 同一级目录里的优先级：原生名在前，兼容名在后，只取第一个存在的。
var candidateNames = []string{"AGENTS.md", "CLAUDE.md"}

// stickyName 粘性规则文件：渲染在最后，且每次压缩后会随前缀重新贴一遍。
const stickyName = "RULES.md"

// File 是一个被加载的指令文件。
type File struct {
	Path    string
	Content string // 已展开 @import
	Sticky  bool
}

// Block 是渲染好的指令块。
type Block struct {
	Files []File
	Text  string
}

// Load 收集 home 与 cwd 所在项目各级的指令文件，展开 @import 后渲染成一块。
// 顺序：用户级 → git 根 → … → cwd（祖先在前，近者在后 = 近者更强），粘性规则最后。
// limit ≤0 时用默认预算；超预算先丢最远的祖先，粘性规则与最近一级永不丢。
func Load(cwd, home string, limit int) (Block, error) {
	if limit <= 0 {
		limit = defaultLimit
	}
	dirs := searchDirs(cwd, home)
	var files []File
	for _, dir := range dirs {
		if f, ok := loadFirst(dir, candidateNames, false); ok {
			files = append(files, f)
		}
	}
	// 粘性规则与普通指令查同样的目录：RULES.md 通常放在仓库根，
	// 只看 cwd 的话，从子目录启动就把项目规则丢了。
	for _, dir := range dirs {
		if f, ok := loadFirst(dir, []string{stickyName}, true); ok {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		return Block{}, nil
	}
	files, dropped := fitBudget(files, limit)
	return Block{Files: files, Text: render(files, dropped)}, nil
}

// searchDirs 返回要查找的目录：用户目录，然后从 git 根一路到 cwd（祖先在前）。
// 不是 git 仓库时只看 cwd —— 无边界地往上翻会把别的项目的约定拽进来。
func searchDirs(cwd, home string) []string {
	var dirs []string
	if home != "" {
		dirs = append(dirs, home)
	}
	root := gitRoot(cwd)
	if root == "" {
		return dedupe(append(dirs, cwd))
	}
	var chain []string
	for d := cwd; ; d = filepath.Dir(d) {
		chain = append(chain, d)
		if d == root || filepath.Dir(d) == d {
			break
		}
	}
	for i := len(chain) - 1; i >= 0; i-- { // 反转：祖先在前
		dirs = append(dirs, chain[i])
	}
	return dedupe(dirs)
}

// gitRoot 向上找 .git（目录或文件）；找不到返回 ""。
func gitRoot(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

func loadFirst(dir string, names []string, sticky bool) (File, bool) {
	for _, n := range names {
		p := filepath.Join(dir, n)
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		content := strings.TrimSpace(expand(string(data), dir, 0, map[string]bool{p: true}))
		if content == "" {
			continue
		}
		return File{Path: p, Content: content, Sticky: sticky}, true
	}
	return File{}, false
}

// expand 展开一段内容里的 @import。
// 代码块与行内代码里的 @ 原样保留（用户可能只是在**讲**这个语法）；
// 目标读不到也原样保留（宁可让人看见没生效，也不要静默吞掉一行指令）。
func expand(content, dir string, depth int, seen map[string]bool) string {
	if depth >= maxImportHops {
		return content
	}
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	inFence := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		lines[i] = expandLine(line, dir, depth, seen)
	}
	return strings.Join(lines, "\n")
}

// expandLine 处理一行里的 @import。
// 只认「行首或空白之后」的 @：这样 git@github.com、user@example.com 天然不会被当成导入。
func expandLine(line, dir string, depth int, seen map[string]bool) string {
	var out strings.Builder
	rs := []rune(line)
	inCode := false
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if r == '`' {
			inCode = !inCode
			out.WriteRune(r)
			continue
		}
		if inCode || r != '@' || !boundaryBefore(rs, i) {
			out.WriteRune(r)
			continue
		}
		raw, next := readToken(rs, i+1)
		token := strings.TrimRight(raw, importTrimSuffix)
		if token == "" {
			out.WriteRune(r)
			continue
		}
		content, ok := readImport(token, dir, depth, seen)
		if !ok {
			out.WriteRune(r)
			out.WriteString(raw)
			i = next - 1
			continue
		}
		out.WriteString(content)
		out.WriteString(strings.TrimPrefix(raw, token)) // 把被 trim 掉的标点还回去
		i = next - 1
	}
	return out.String()
}

// boundaryBefore 判断 @ 前面是不是行首或空白。
func boundaryBefore(rs []rune, i int) bool {
	if i == 0 {
		return true
	}
	return unicode.IsSpace(rs[i-1])
}

// readToken 读到下一个空白为止。
func readToken(rs []rune, from int) (string, int) {
	j := from
	for j < len(rs) && !unicode.IsSpace(rs[j]) {
		j++
	}
	return string(rs[from:j]), j
}

// readImport 解析并读取导入目标；返回展开后的内容。
func readImport(token, dir string, depth int, seen map[string]bool) (string, bool) {
	p := token
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		p = filepath.Join(home, p[2:])
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p) // 相对导入者所在目录，不是会话 cwd
	}
	p = filepath.Clean(p)
	if seen[p] {
		return "", false // 环：留着原文，读的人能看出这里有个循环
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", false
	}
	seen[p] = true
	return expand(string(data), filepath.Dir(p), depth+1, seen), true
}

// fitBudget 裁到预算内：从最远的祖先开始丢，粘性规则与最后一个普通文件永不丢。
func fitBudget(files []File, limit int) ([]File, int) {
	total := 0
	for _, f := range files {
		total += len(f.Content)
	}
	dropped := 0
	for total > limit {
		idx := -1
		for i, f := range files {
			if !f.Sticky {
				idx = i
				break
			}
		}
		if idx < 0 || countNonSticky(files) <= 1 {
			break // 只剩粘性规则或最后一个普通文件：宁可超一点预算也要留下最近的约定
		}
		total -= len(files[idx].Content)
		files = append(files[:idx], files[idx+1:]...)
		dropped++
	}
	return files, dropped
}

func countNonSticky(files []File) int {
	n := 0
	for _, f := range files {
		if !f.Sticky {
			n++
		}
	}
	return n
}

// render 渲染成注入文本：每个文件带绝对路径，粘性规则单独一段放最后。
func render(files []File, dropped int) string {
	sb := &strings.Builder{}
	for _, f := range files {
		if f.Sticky {
			continue
		}
		writeTag(sb, "project-instructions", f)
	}
	if dropped > 0 {
		fmt.Fprintf(sb, "（还有 %d 个上层指令文件因预算被省略）\n", dropped)
	}
	for _, f := range files {
		if f.Sticky {
			writeTag(sb, "sticky-rules", f)
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

func writeTag(sb *strings.Builder, tag string, f File) {
	sb.WriteString("<" + tag + ` path="` + f.Path + `">` + "\n")
	sb.WriteString(f.Content)
	sb.WriteString("\n</" + tag + ">\n")
}

func dedupe(xs []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if x == "" || seen[x] {
			continue
		}
		seen[x] = true
		out = append(out, x)
	}
	return out
}
