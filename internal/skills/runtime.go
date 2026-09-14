package skills

import (
	"fmt"
	"html"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// InvocationKind controls who is allowed to load a skill.
type InvocationKind int

const (
	InvocationModel InvocationKind = iota
	InvocationUser
)

// RenderIndex returns the stable, lightweight system-prompt inventory. Skills
// that opt out of model invocation remain available to explicit user commands.
func (m *Manager) RenderIndex() string {
	var visible []Skill
	for _, skill := range m.List() {
		if !skill.DisableModelInvocation {
			visible = append(visible, skill)
		}
	}
	if len(visible) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<available-skills>\n")
	for _, skill := range visible {
		fmt.Fprintf(&b, "- %s: %s\n", html.EscapeString(skill.Name), html.EscapeString(skill.Description))
	}
	b.WriteString("</available-skills>\n")
	b.WriteString("当某个 skill 与任务匹配时，必须先调用 skill 工具加载完整说明；不要根据描述猜测具体步骤。")
	return b.String()
}

// BuildPrompt reads and frames a skill body for a model or explicit user invocation.
func (m *Manager) BuildPrompt(name, args string, kind InvocationKind) (string, Skill, error) {
	skill, ok := m.Get(name)
	if !ok {
		return "", Skill{}, fmt.Errorf("未知 skill %q；可用：%s", name, m.availableNames(kind))
	}
	if kind == InvocationModel && skill.DisableModelInvocation {
		return "", Skill{}, fmt.Errorf("skill %q 禁止模型自动调用，只能由用户显式选择", skill.Name)
	}
	if kind == InvocationUser && !skill.UserInvocable {
		return "", Skill{}, fmt.Errorf("skill %q 禁止用户显式调用", skill.Name)
	}
	parsed, err := parseFile(skill.FilePath, m.opts.MaxFileBytes)
	if err != nil {
		return "", Skill{}, fmt.Errorf("重新读取 skill %q 失败: %w", skill.Name, err)
	}
	var b strings.Builder
	if kind == InvocationUser {
		fmt.Fprintf(&b, "[用户显式调用了 skill %q；请遵循以下完整说明。]\n\n", skill.Name)
	} else {
		fmt.Fprintf(&b, "[Skill: %s]\n", skill.Name)
	}
	if len(skill.AllowedTools) > 0 {
		fmt.Fprintf(&b, "[建议工具（不授予额外权限）: %s]\n", strings.Join(skill.AllowedTools, ", "))
	}
	b.WriteString(parsed.body)
	fmt.Fprintf(&b, "\n\n---\n[Skill directory: %s]\n", skill.BaseDir)
	b.WriteString("相对资源请通过 skill://" + skill.Name + "/<path> 读取；执行脚本仍须使用现有工具并遵守权限策略。")
	if args = strings.TrimSpace(args); args != "" {
		b.WriteString("\nUser: ")
		b.WriteString(args)
	}
	return b.String(), skill, nil
}

// Resolve converts the rest of a skill:// URL into a contained filesystem path.
func (m *Manager) Resolve(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("skill:// URL 缺少名称")
	}
	parts := strings.SplitN(ref, "/", 2)
	name, err := url.PathUnescape(parts[0])
	if err != nil || name == "" {
		return "", fmt.Errorf("非法 skill 名称 %q", parts[0])
	}
	skill, ok := m.Get(name)
	if !ok {
		return "", fmt.Errorf("未知 skill %q；可用：%s", name, m.availableNames(InvocationUser))
	}
	if len(parts) == 1 || parts[1] == "" {
		return skill.FilePath, nil
	}
	rel, err := url.PathUnescape(parts[1])
	if err != nil {
		return "", fmt.Errorf("非法 skill 相对路径: %w", err)
	}
	if filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" {
		return "", fmt.Errorf("skill:// 不允许绝对路径")
	}
	rel = filepath.FromSlash(rel)
	for _, component := range strings.FieldsFunc(rel, func(r rune) bool { return r == '/' || r == '\\' }) {
		if component == ".." {
			return "", fmt.Errorf("skill:// 不允许 .. 路径穿越")
		}
	}
	target := filepath.Join(skill.BaseDir, rel)
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("skill 资源不存在: %s", target)
		}
		return "", err
	}
	realBase, err := filepath.EvalSymlinks(skill.BaseDir)
	if err != nil {
		return "", err
	}
	contained, err := filepath.Rel(realBase, realTarget)
	if err != nil || contained == ".." || strings.HasPrefix(contained, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("skill:// 路径解析到 skill 目录之外")
	}
	st, err := os.Stat(realTarget)
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("skill 资源不是普通文件: %s", realTarget)
	}
	return realTarget, nil
}

func (m *Manager) availableNames(kind InvocationKind) string {
	list := m.List()
	names := make([]string, 0, len(list))
	for _, skill := range list {
		if kind == InvocationModel && skill.DisableModelInvocation {
			continue
		}
		if kind == InvocationUser && !skill.UserInvocable {
			continue
		}
		names = append(names, skill.Name)
	}
	if len(names) == 0 {
		return "无"
	}
	return strings.Join(names, ", ")
}

// ParseInvocation parses the leading `/skill:<name> [args]` form.
func ParseInvocation(text string) (name, args string, ok bool) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "/skill:") {
		return "", "", false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "/skill:"))
	if rest == "" {
		return "", "", false
	}
	if i := strings.IndexAny(rest, " \t\r\n"); i >= 0 {
		return rest[:i], strings.TrimSpace(rest[i:]), true
	}
	return rest, "", true
}
