package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	projectpaths "einoclaw-build/internal/paths"
)

type searchRoot struct {
	path        string
	source      Source
	includeSelf bool
}

func discover(opts Options) (snapshot, []Warning, error) {
	if opts.MaxFileBytes <= 0 {
		opts.MaxFileBytes = DefaultMaxFileBytes
	}
	if opts.MaxSkills <= 0 {
		opts.MaxSkills = DefaultMaxSkills
	}
	cwd, err := canonicalExistingDir(opts.CWD)
	if err != nil {
		return snapshot{}, nil, fmt.Errorf("skills cwd: %w", err)
	}
	opts.CWD = cwd

	var warnings []Warning
	for _, pattern := range append(append([]string(nil), opts.Include...), opts.Ignore...) {
		if _, err := filepath.Match(pattern, "probe"); err != nil {
			warnings = append(warnings, Warning{Message: fmt.Sprintf("忽略非法 skill 匹配模式 %q: %v", pattern, err)})
		}
	}

	roots := discoveryRoots(opts)
	seenPath := map[string]bool{}
	byName := map[string]Skill{}
	var list []Skill
	limitReached := false
	for _, root := range roots {
		paths, rootWarnings := candidateFiles(root)
		warnings = append(warnings, rootWarnings...)
		for _, path := range paths {
			skill, enabled, err := skillFromFile(path, root.source, opts.MaxFileBytes)
			if err != nil {
				warnings = append(warnings, Warning{Path: path, Message: err.Error()})
				continue
			}
			if !enabled || !included(skill.Name, opts.Include, opts.Ignore) {
				continue
			}
			if seenPath[skill.FilePath] {
				continue
			}
			seenPath[skill.FilePath] = true
			key := normalizeName(skill.Name)
			if winner, exists := byName[key]; exists {
				warnings = append(warnings, Warning{Path: skill.FilePath,
					Message: fmt.Sprintf("skill %q 与 %s 冲突；保留优先级更高的 %s", skill.Name, winner.FilePath, winner.FilePath)})
				continue
			}
			if len(list) >= opts.MaxSkills {
				if !limitReached {
					warnings = append(warnings, Warning{Message: fmt.Sprintf("skills 数量超过上限 %d，其余条目已忽略", opts.MaxSkills)})
					limitReached = true
				}
				continue
			}
			byName[key] = skill
			list = append(list, skill)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		a, b := strings.ToLower(list[i].Name), strings.ToLower(list[j].Name)
		if a != b {
			return a < b
		}
		if list[i].Name != list[j].Name {
			return list[i].Name < list[j].Name
		}
		return list[i].FilePath < list[j].FilePath
	})
	return snapshot{list: list, byName: byName}, warnings, nil
}

func discoveryRoots(opts Options) []searchRoot {
	var roots []searchRoot
	for _, dir := range boundedProjectDirs(opts.CWD) { // nearest first = strongest project scope
		roots = append(roots, root(projectpaths.ProjectSkillsDir(dir), "project", "codeclaw-project", false))
		if opts.Compatibility.Claude {
			roots = append(roots, root(filepath.Join(dir, ".claude", "skills"), "compatibility", "claude-project", false))
		}
		if opts.Compatibility.Codex {
			roots = append(roots, root(filepath.Join(dir, ".codex", "skills"), "compatibility", "codex-project", false))
		}
		if opts.Compatibility.Agents {
			roots = append(roots, root(filepath.Join(dir, ".agents", "skills"), "compatibility", "agents-project", false))
		}
	}
	for i, dir := range opts.CustomDirectories {
		dir = expandHome(strings.TrimSpace(dir), opts.UserHome)
		if dir != "" {
			roots = append(roots, root(dir, "custom", fmt.Sprintf("custom-%d", i+1), true))
		}
	}
	if opts.UserSkillsDir != "" {
		roots = append(roots, root(opts.UserSkillsDir, "user", "codeclaw-user", false))
	}
	if opts.UserHome != "" {
		if opts.Compatibility.Claude {
			roots = append(roots, root(filepath.Join(opts.UserHome, ".claude", "skills"), "compatibility", "claude-user", false))
		}
		if opts.Compatibility.Codex {
			roots = append(roots, root(filepath.Join(opts.UserHome, ".codex", "skills"), "compatibility", "codex-user", false))
		}
		if opts.Compatibility.Agents {
			roots = append(roots, root(filepath.Join(opts.UserHome, ".agents", "skills"), "compatibility", "agents-user", false))
		}
	}
	return dedupeRoots(roots)
}

func root(path, kind, label string, includeSelf bool) searchRoot {
	path, _ = filepath.Abs(path)
	return searchRoot{path: filepath.Clean(path), source: Source{Kind: kind, Label: label, Root: filepath.Clean(path)}, includeSelf: includeSelf}
}

func dedupeRoots(in []searchRoot) []searchRoot {
	seen := map[string]bool{}
	out := make([]searchRoot, 0, len(in))
	for _, item := range in {
		key := item.path
		if real, err := filepath.EvalSymlinks(item.path); err == nil {
			key = real
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, item)
	}
	return out
}

func candidateFiles(root searchRoot) ([]string, []Warning) {
	entries, err := os.ReadDir(root.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []Warning{{Path: root.path, Message: fmt.Sprintf("读取 skills 目录失败: %v", err)}}
	}
	var paths []string
	if root.includeSelf {
		if st, err := os.Stat(filepath.Join(root.path, "SKILL.md")); err == nil && st.Mode().IsRegular() {
			paths = append(paths, filepath.Join(root.path, "SKILL.md"))
		}
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		candidate := filepath.Join(root.path, entry.Name(), "SKILL.md")
		if st, err := os.Stat(candidate); err == nil && st.Mode().IsRegular() {
			paths = append(paths, candidate)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func boundedProjectDirs(cwd string) []string {
	boundary := ""
	for dir := cwd; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			boundary = dir
			break
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	if boundary == "" {
		return []string{cwd}
	}
	var dirs []string
	for dir := cwd; ; dir = filepath.Dir(dir) {
		dirs = append(dirs, dir)
		if dir == boundary || filepath.Dir(dir) == dir {
			break
		}
	}
	return dirs
}

func canonicalExistingDir(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", fmt.Errorf("%s 不是目录", path)
	}
	return filepath.Clean(path), nil
}

func expandHome(path, home string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") && home != "" {
		return filepath.Join(home, path[2:])
	}
	return path
}

func included(name string, include, ignore []string) bool {
	if matchesAny(name, ignore) {
		return false
	}
	return len(include) == 0 || matchesAny(name, include)
}

func matchesAny(name string, patterns []string) bool {
	for _, pattern := range patterns {
		matched, err := filepath.Match(pattern, name)
		if err == nil && matched {
			return true
		}
	}
	return false
}
