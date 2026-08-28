package subagent

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed agents/*.md
var bundledFS embed.FS

// DiscoverResult 是一次发现的结果：可用定义 + 每个坏文件一条告警。
// 一个文件写坏不影响其它 agent —— 装配层把 Warns 打印一次就够。
type DiscoverResult struct {
	Defs  []AgentDef
	Warns []error
}

// Discover 合并三层定义：项目（<cwd>/.codeclaw/agents）→ 用户（<Home>/agents）→ 内置，
// 同名 first-wins（项目覆盖用户覆盖内置）。目录内按文件名字典序，目录不存在按空处理。
func Discover(projectAgentsDir, userAgentsDir string, bundled []AgentDef) DiscoverResult {
	var res DiscoverResult
	seen := map[string]bool{}
	add := func(d AgentDef) {
		if seen[d.Name] {
			return
		}
		seen[d.Name] = true
		res.Defs = append(res.Defs, d)
	}
	for _, dir := range []struct {
		path, source string
	}{{projectAgentsDir, "project"}, {userAgentsDir, "user"}} {
		if dir.path == "" {
			continue
		}
		defs, warns := loadAgentsFromDir(dir.path, dir.source)
		res.Warns = append(res.Warns, warns...)
		for _, d := range defs {
			add(d)
		}
	}
	for _, d := range bundled {
		add(d)
	}
	return res
}

// loadAgentsFromDir 读一个目录下的 *.md（字典序）；解析失败的文件记 warn 并跳过。
func loadAgentsFromDir(dir, source string) ([]AgentDef, []error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil // 目录不存在/不可读按空处理，不是错误
	}
	names := make([]string, 0, len(des))
	for _, de := range des {
		if !de.IsDir() && strings.HasSuffix(de.Name(), ".md") {
			names = append(names, de.Name())
		}
	}
	sort.Strings(names)
	var defs []AgentDef
	var warns []error
	for _, n := range names {
		p := filepath.Join(dir, n)
		data, err := os.ReadFile(p)
		if err != nil {
			warns = append(warns, fmt.Errorf("读取 agent 定义 %s 失败: %w", p, err))
			continue
		}
		d, err := ParseAgentFile(p, data, source)
		if err != nil {
			warns = append(warns, err)
			continue
		}
		defs = append(defs, d)
	}
	return defs, warns
}

var (
	bundledOnce sync.Once
	bundledDefs []AgentDef
)

// Bundled 返回内置 agent 定义（嵌入的 markdown，与用户定义走同一条解析路径）。
// 内置定义写错属于编译期错误，直接 panic —— 必须在启动时暴露，而不是静默少一个 agent。
func Bundled() []AgentDef {
	bundledOnce.Do(func() {
		des, err := bundledFS.ReadDir("agents")
		if err != nil {
			panic("内置 agent 目录不可读: " + err.Error())
		}
		names := make([]string, 0, len(des))
		for _, de := range des {
			names = append(names, de.Name())
		}
		sort.Strings(names)
		for _, n := range names {
			p := "agents/" + n
			data, err := bundledFS.ReadFile(p)
			if err != nil {
				panic("内置 agent " + p + " 不可读: " + err.Error())
			}
			d, err := ParseAgentFile(p, data, "bundled")
			if err != nil {
				panic("内置 agent " + p + " 解析失败: " + err.Error())
			}
			bundledDefs = append(bundledDefs, d)
		}
	})
	out := make([]AgentDef, len(bundledDefs))
	copy(out, bundledDefs)
	return out
}

// frontmatter 是定义文件头部的 YAML；未识别字段忽略（向前兼容其它 harness 的额外字段）。
type frontmatter struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description"`
	WhenToUse   string         `yaml:"when_to_use"`
	Tools       stringList     `yaml:"tools"`
	Spawns      stringList     `yaml:"spawns"`
	Model       string         `yaml:"model"`
	Output      map[string]any `yaml:"output"`
	SchemaMode  string         `yaml:"schema_mode"`
	MaxTurns    int            `yaml:"max_turns"`
	SoftBudget  int            `yaml:"soft_budget"`
	Timeout     string         `yaml:"timeout"`
	ReadOnly    bool           `yaml:"read_only"`
	Blocking    bool           `yaml:"blocking"`
}

// stringList 同时接受 CSV 标量（tools: a, b）与序列（tools: [a, b]）。
type stringList []string

func (s *stringList) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		var raw string
		if err := n.Decode(&raw); err != nil {
			return err
		}
		for _, p := range strings.Split(raw, ",") {
			if p = strings.TrimSpace(p); p != "" {
				*s = append(*s, p)
			}
		}
		return nil
	case yaml.SequenceNode:
		var items []string
		if err := n.Decode(&items); err != nil {
			return err
		}
		for _, it := range items {
			if it = strings.TrimSpace(it); it != "" {
				*s = append(*s, it)
			}
		}
		return nil
	}
	return fmt.Errorf("期望字符串或字符串数组")
}

// ParseAgentFile 解析一个 markdown 定义：`---` frontmatter + 正文（系统提示词）。
// name / description / 正文三者缺一即无效；单个字段格式错（如 timeout）只降级为默认值，不废掉整个文件。
func ParseAgentFile(path string, data []byte, source string) (AgentDef, error) {
	fmBytes, body, ok := splitFrontmatter(data)
	if !ok {
		return AgentDef{}, fmt.Errorf("agent 定义 %s 缺少 --- frontmatter 头", path)
	}
	var fm frontmatter
	if err := yaml.Unmarshal(fmBytes, &fm); err != nil {
		return AgentDef{}, fmt.Errorf("agent 定义 %s 的 frontmatter 解析失败: %w", path, err)
	}
	name := strings.TrimSpace(fm.Name)
	desc := strings.TrimSpace(fm.Description)
	body = strings.TrimSpace(body)
	if name == "" || desc == "" || body == "" {
		return AgentDef{}, fmt.Errorf("agent 定义 %s 必须有 name、description 与正文（系统提示词）", path)
	}
	d := AgentDef{
		Name: name, Description: desc, WhenToUse: strings.TrimSpace(fm.WhenToUse), SystemPrompt: body,
		Tools: fm.Tools, Spawns: fm.Spawns, Model: strings.TrimSpace(fm.Model),
		OutputSchema: fm.Output, SchemaMode: strings.TrimSpace(fm.SchemaMode),
		MaxTurns: fm.MaxTurns, SoftBudget: fm.SoftBudget,
		ReadOnly: fm.ReadOnly, Blocking: fm.Blocking,
		Source: source, FilePath: path,
	}
	if t := strings.TrimSpace(fm.Timeout); t != "" {
		if dur, err := time.ParseDuration(t); err == nil {
			d.Timeout = dur
		}
		// 非法 duration 只丢这个字段（用配置默认值），不废掉整个定义
	}
	return d, nil
}

// splitFrontmatter 切出首个 `---` 与下一个 `---` 之间的 YAML 与之后的正文。
func splitFrontmatter(data []byte) (fm []byte, body string, ok bool) {
	s := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") {
		return nil, "", false
	}
	rest := s[len("---\n"):]
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return nil, "", false
	}
	after := rest[idx+len("\n---"):]
	// 结束行必须只有 ---（允许行尾空白）
	nl := strings.Index(after, "\n")
	if nl < 0 {
		nl = len(after)
	}
	if strings.TrimSpace(after[:nl]) != "" {
		return nil, "", false
	}
	return []byte(rest[:idx]), after[min(nl+1, len(after)):], true
}
