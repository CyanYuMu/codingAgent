// Package paths 决定 codeclaw 的数据落点：数据根目录、按 cwd 分桶的项目目录、项目身份与配置路径。
//
// 布局：
//
//	<Home>/                       $CODECLAW_HOME 或 ~/.codeclaw
//	├── config.yaml               用户级配置
//	└── projects/<EncodeCWD>/     项目桶：会话、产物、记忆
//
// 项目桶按规范化后的 cwd 编码命名，同一目录（含符号链接别名）落到同一桶，不同项目互不可见。
package paths

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Home 返回数据根目录：$CODECLAW_HOME 或 ~/.codeclaw；不存在则创建。
func Home() (string, error) {
	dir := os.Getenv("CODECLAW_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".codeclaw")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// Canonical 规范化路径：绝对化 + 解析符号链接 + Clean。
func Canonical(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	return filepath.Clean(abs), nil
}

// EncodeCWD 把规范化后的绝对路径编码成目录名。
// 家目录下 → "-" + 相对路径（分隔符换 "-"）；其它 → "--" + 绝对路径（分隔符换 "-"）+ "--"。
func EncodeCWD(cwd string) (string, error) {
	c, err := Canonical(cwd)
	if err != nil {
		return "", err
	}
	sep := string(filepath.Separator)
	if home, err := os.UserHomeDir(); err == nil {
		if h, err := Canonical(home); err == nil && (c == h || strings.HasPrefix(c, h+sep)) {
			rel := strings.TrimPrefix(c, h)
			return "-" + strings.Trim(strings.ReplaceAll(rel, sep, "-"), "-"), nil
		}
	}
	return "--" + strings.Trim(strings.ReplaceAll(c, sep, "-"), "-") + "--", nil
}

// ProjectDir 返回 <Home>/projects/<EncodeCWD(cwd)>/ 并确保存在，同时维护 project.json。
func ProjectDir(cwd string) (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	enc, err := EncodeCWD(cwd)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "projects", enc)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	writeProjectJSON(dir, cwd)
	return dir, nil
}

type projectMeta struct {
	CWD       string `json:"cwd"`
	GitRoot   string `json:"git_root,omitempty"`
	FirstSeen string `json:"first_seen"`
	LastSeen  string `json:"last_seen"`
}

func writeProjectJSON(dir, cwd string) {
	p := filepath.Join(dir, "project.json")
	now := time.Now().Format(time.RFC3339)
	meta := projectMeta{CWD: cwd, GitRoot: GitRoot(cwd), FirstSeen: now, LastSeen: now}
	if b, err := os.ReadFile(p); err == nil {
		var old projectMeta
		if json.Unmarshal(b, &old) == nil && old.FirstSeen != "" {
			meta.FirstSeen = old.FirstSeen
		}
	}
	if b, err := json.MarshalIndent(meta, "", "  "); err == nil {
		_ = os.WriteFile(p, b, 0o644)
	}
}

// GitRoot 返回 cwd 所在 git 主工作区根；worktree 解析 .git 文件里的 gitdir；非 git 返回 ""。
func GitRoot(cwd string) string {
	dir, err := Canonical(cwd)
	if err != nil {
		return ""
	}
	for {
		g := filepath.Join(dir, ".git")
		st, err := os.Stat(g)
		if err == nil {
			if st.IsDir() {
				return dir
			}
			if b, err := os.ReadFile(g); err == nil {
				line := strings.TrimSpace(string(b))
				if rest, ok := strings.CutPrefix(line, "gitdir:"); ok {
					gd := strings.TrimSpace(rest)
					if !filepath.IsAbs(gd) {
						gd = filepath.Join(dir, gd)
					}
					gd = filepath.Clean(gd)
					// …/.git/worktrees/<name> → 主根 = 上三级
					if filepath.Base(filepath.Dir(gd)) == "worktrees" {
						return filepath.Dir(filepath.Dir(filepath.Dir(gd)))
					}
					return filepath.Dir(gd)
				}
			}
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// ProjectID 返回记忆作用域用的项目身份：<主根 basename>-<sha256(主根)[:8]>；非 git 用 cwd。
func ProjectID(cwd string) (string, error) {
	base := GitRoot(cwd)
	if base == "" {
		c, err := Canonical(cwd)
		if err != nil {
			return "", err
		}
		base = c
	}
	sum := sha256.Sum256([]byte(base))
	return strings.ToLower(filepath.Base(base)) + "-" + hex.EncodeToString(sum[:])[:8], nil
}

// UserConfigPath = <Home>/config.yaml
func UserConfigPath() (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "config.yaml"), nil
}

// ProjectConfigPath = <cwd>/.codeclaw/config.yaml
func ProjectConfigPath(cwd string) string {
	return filepath.Join(cwd, ".codeclaw", "config.yaml")
}

// GlobalMemoryPath = <Home>/memory/global.db ：跨项目的用户偏好类记忆。
func GlobalMemoryPath() (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "memory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, "global.db"), nil
}

// UserAgentsDir = <Home>/agents ：用户级子 agent 定义（*.md，frontmatter）。目录可以不存在。
func UserAgentsDir() (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "agents"), nil
}

// ProjectAgentsDir = <cwd>/.codeclaw/agents ：项目级子 agent 定义（优先级高于用户级）。
func ProjectAgentsDir(cwd string) string {
	return filepath.Join(cwd, ".codeclaw", "agents")
}
