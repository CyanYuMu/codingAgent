package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

type stringList []string

func (s *stringList) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		var raw string
		if err := n.Decode(&raw); err != nil {
			return err
		}
		for _, item := range strings.Split(raw, ",") {
			if item = strings.TrimSpace(item); item != "" {
				*s = append(*s, item)
			}
		}
		return nil
	case yaml.SequenceNode:
		var items []string
		if err := n.Decode(&items); err != nil {
			return err
		}
		for _, item := range items {
			if item = strings.TrimSpace(item); item != "" {
				*s = append(*s, item)
			}
		}
		return nil
	default:
		return fmt.Errorf("期望字符串或字符串数组")
	}
}

type frontmatter struct {
	Name                   string     `yaml:"name"`
	Description            string     `yaml:"description"`
	Enabled                *bool      `yaml:"enabled"`
	DisableModelInvocation bool       `yaml:"disable-model-invocation"`
	UserInvocable          *bool      `yaml:"user-invocable"`
	AllowedTools           stringList `yaml:"allowed-tools"`
}

type parsedFile struct {
	frontmatter frontmatter
	body        string
}

func parseFile(path string, maxBytes int64) (parsedFile, error) {
	st, err := os.Stat(path)
	if err != nil {
		return parsedFile{}, err
	}
	if !st.Mode().IsRegular() {
		return parsedFile{}, fmt.Errorf("不是普通文件")
	}
	if maxBytes > 0 && st.Size() > maxBytes {
		return parsedFile{}, fmt.Errorf("文件大小 %d bytes，超过限制 %d bytes", st.Size(), maxBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return parsedFile{}, err
	}
	fmData, body, ok := splitFrontmatter(data)
	if !ok {
		return parsedFile{}, fmt.Errorf("缺少有效的 --- YAML frontmatter")
	}
	var fm frontmatter
	if err := yaml.Unmarshal(fmData, &fm); err != nil {
		return parsedFile{}, fmt.Errorf("frontmatter 解析失败: %w", err)
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return parsedFile{}, fmt.Errorf("正文不能为空")
	}
	return parsedFile{frontmatter: fm, body: body}, nil
}

func splitFrontmatter(data []byte) ([]byte, string, bool) {
	s := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") {
		return nil, "", false
	}
	rest := s[len("---\n"):]
	lines := strings.Split(rest, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != "---" {
			continue
		}
		return []byte(strings.Join(lines[:i], "\n")), strings.Join(lines[i+1:], "\n"), true
	}
	return nil, "", false
}

func skillFromFile(path string, source Source, maxBytes int64) (Skill, bool, error) {
	parsed, err := parseFile(path, maxBytes)
	if err != nil {
		return Skill{}, false, err
	}
	if parsed.frontmatter.Enabled != nil && !*parsed.frontmatter.Enabled {
		return Skill{}, false, nil
	}
	name := strings.TrimSpace(parsed.frontmatter.Name)
	if name == "" {
		name = filepath.Base(filepath.Dir(path))
	}
	if !validName.MatchString(name) {
		return Skill{}, false, fmt.Errorf("name %q 非法：只允许字母、数字、-、_，长度不超过 128", name)
	}
	description := normalizeDescription(parsed.frontmatter.Description)
	if description == "" {
		return Skill{}, false, fmt.Errorf("description 不能为空")
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return Skill{}, false, fmt.Errorf("解析真实路径失败: %w", err)
	}
	realPath, err = filepath.Abs(realPath)
	if err != nil {
		return Skill{}, false, err
	}
	userInvocable := true
	if parsed.frontmatter.UserInvocable != nil {
		userInvocable = *parsed.frontmatter.UserInvocable
	}
	return Skill{
		Name: name, Description: description, FilePath: filepath.Clean(realPath), BaseDir: filepath.Dir(realPath),
		Source: source, DisableModelInvocation: parsed.frontmatter.DisableModelInvocation,
		UserInvocable: userInvocable, AllowedTools: append([]string(nil), parsed.frontmatter.AllowedTools...),
	}, true, nil
}

func normalizeDescription(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const maxRunes = 512
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxRunes]) + "…"
}

func normalizeName(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
