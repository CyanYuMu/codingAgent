# Phase 8 地基修正 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 codeclaw 的会话/记忆按项目隔离、压缩永不拆 tool_call/tool_result 配对、子 agent 真正可控（yield 终止、状态准确、继承父权限、独立 bash、sidecar 转录），并提供 headless `-p` 入口用于自测。

**Architecture:** 循环以 `agent.Context`（生产 = `context.Manager` 包着 session）为真相源：每步从 session 重建输入、在循环内记录 assistant/tool 消息，压缩与溢出恢复都在循环内生效。数据目录迁到 `~/.codeclaw/projects/<encoded-cwd>/`。子 agent 由 `subagent.Manager` 以 Options 装配：每 Run 独立 Bash/产物存储/sidecar 会话，权限继承父。

**Tech Stack:** Go 1.26，eino v0.10.0-alpha.9（仅 `internal/model`），`github.com/eino-contrib/jsonschema`（已是间接依赖，用于工具 schema 透传），modernc.org/sqlite，BubbleTea v2。

**Spec:** `docs/specs/phase-8-foundation-fixes.md`

## Global Constraints

- 只有 `internal/model` 可以 import eino / eino-contrib。
- 每个任务结束：`env -u GOROOT go build ./... && env -u GOROOT go vet ./... && env -u GOROOT go test ./...` 通过。
- 默认 `approval_mode` = `write`；headless 子 agent 的 Prompt 决策默认拒绝，`subagent.approval_escalation: true` 才升级。
- 数据根目录 `$CODECLAW_HOME` 或 `~/.codeclaw`；项目桶 `projects/<EncodeCWD(cwd)>`。
- 压缩切点只能落在 `user` 消息或无 tool_call 的 `assistant` 消息上。
- 提交信息用中文前缀 `feat/fix/refactor/test:`，每个任务至少一次提交。

---

## File Structure

| 文件 | 职责 |
|---|---|
| `internal/paths/paths.go`（新） | Home / EncodeCWD / ProjectDir / ProjectID / GitRoot / 配置路径 |
| `internal/paths/paths_test.go`（新） | 编码与 git root 测试 |
| `cmd/agent/config.go` | 三层合并、默认值、`subagent` 段 |
| `cmd/agent/config_test.go`（新） | 合并测试 |
| `cmd/agent/main.go` | 装配；`--yolo` / `-p` / `--cwd`；headless 分支 |
| `cmd/agent/headless.go`（新） | `-p` 模式事件打印 |
| `internal/session/entry.go` | Entry v2 字段与类型常量 |
| `internal/session/session.go` | leaf、Append 生成 id、Replay（路径回溯 + compaction 展开 + 悬空修复）、Compact(firstKept)、SetTitle、Info |
| `internal/session/replay.go`（新） | 纯函数 `buildContext(entries, leaf)` 与 `repairDangling` |
| `internal/session/manager.go` | 项目目录、Info 列表、ArtifactDir |
| `internal/session/*_test.go` | 更新 + 新增 |
| `internal/model/model.go` | `ToolSpec.Required`、`ModelStream` 接口、`IsContextOverflow`、`IsRetryable` |
| `internal/model/eino.go` | JSON Schema 透传、返回 `ModelStream` |
| `internal/model/errors_test.go`（新） | 错误分类测试 |
| `internal/context/manager.go` | Manager v2（Build/Record/ShouldCompact/Compact/RecoverOverflow） |
| `internal/context/compaction.go` | 安全切点、完整序列化 |
| `internal/context/tokenizer.go` | 全块估算 |
| `internal/context/context_test.go` | 更新 + 新增 |
| `internal/agent/agent.go` | `Context` 接口、`New` 新签名、`MemoryContext` |
| `internal/agent/loop.go` | 循环 v2 |
| `internal/agent/event.go` | 新事件类型 |
| `internal/agent/loop_test.go` | fakeModel 驱动的循环测试 |
| `internal/runtime/artifact.go`（新） | ArtifactStore |
| `internal/runtime/sink.go` | 接 ArtifactStore |
| `internal/runtime/bash.go` | 默认 cwd、相对 cd |
| `internal/tool/tool.go` | `Terminal` 接口 |
| `internal/tool/executor.go` | 产物存储注入、审批拒绝文案 |
| `internal/tool/tools.go` | `Builtins(bash, store)`、`artifact://` 读取 |
| `internal/tool/mcp.go` | `TierWrite` |
| `internal/subagent/spec.go` | 状态常量、Task.Name、Result 扩展 |
| `internal/subagent/manager.go` | Options、Run v2、RunMany |
| `internal/subagent/yield.go` | Terminal |
| `internal/subagent/approver.go`（新） | denyApprover / labeledApprover |
| `internal/subagent/task.go` | 参数 schema（含 name）、输出格式 |
| `internal/subagent/manager_test.go` | fakeModel 驱动测试 |
| `internal/tui/tui.go`、`approval.go` | 接新循环、会话列表、审批标签 |
| `internal/eval/evaluator.go` | 适配新 API |
| `docs/DEVELOPMENT_LOG.md`、`example.yaml` | 记录与示例 |

---

### Task 1: `internal/paths` 数据目录与项目分桶

**Files:**
- Create: `internal/paths/paths.go`
- Test: `internal/paths/paths_test.go`

**Interfaces:**
- Produces: `paths.Home() (string, error)`、`paths.EncodeCWD(cwd string) (string, error)`、`paths.ProjectDir(cwd string) (string, error)`、`paths.ProjectID(cwd string) (string, error)`、`paths.GitRoot(cwd string) string`、`paths.UserConfigPath() (string, error)`、`paths.ProjectConfigPath(cwd string) string`

- [ ] **Step 1: 写失败测试**

```go
package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEncodeCWDUnderHome(t *testing.T) {
	home, _ := os.UserHomeDir()
	got, err := EncodeCWD(filepath.Join(home, "Projects", "foo"))
	if err != nil || got != "-Projects-foo" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestEncodeCWDOutsideHome(t *testing.T) {
	got, err := EncodeCWD("/opt/work/bar")
	if err != nil || got != "--opt-work-bar--" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestHomeOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODECLAW_HOME", dir)
	h, err := Home()
	if err != nil || h != dir {
		t.Fatalf("home = %q err %v", h, err)
	}
}

func TestProjectDirCreatesBucket(t *testing.T) {
	t.Setenv("CODECLAW_HOME", t.TempDir())
	cwd := t.TempDir()
	pd, err := ProjectDir(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(pd); err != nil || !st.IsDir() {
		t.Fatalf("project dir missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pd, "project.json")); err != nil {
		t.Fatalf("project.json missing: %v", err)
	}
	if !strings.HasPrefix(filepath.Base(pd), "-") {
		t.Fatalf("bucket name = %q", filepath.Base(pd))
	}
}

func TestGitRootFindsWorktreeMainRoot(t *testing.T) {
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, ".git", "worktrees", "wt1"), 0o755)
	wt := t.TempDir()
	_ = os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+filepath.Join(root, ".git", "worktrees", "wt1")+"\n"), 0o644)
	sub := filepath.Join(wt, "a", "b")
	_ = os.MkdirAll(sub, 0o755)
	if got := GitRoot(sub); got != root {
		t.Fatalf("GitRoot = %q, want %q", got, root)
	}
	if got := GitRoot(t.TempDir()); got != "" {
		t.Fatalf("non-git should be empty, got %q", got)
	}
}

func TestProjectIDStableAndScoped(t *testing.T) {
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	a, _ := ProjectID(root)
	b, _ := ProjectID(filepath.Join(root))
	if a != b || !strings.HasPrefix(a, filepath.Base(root)+"-") {
		t.Fatalf("ids %q %q", a, b)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `env -u GOROOT go test ./internal/paths/ -v`
Expected: FAIL（包不存在）

- [ ] **Step 3: 实现**

```go
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
				if strings.HasPrefix(line, "gitdir:") {
					gd := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
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
```

- [ ] **Step 4: 运行测试通过**

Run: `env -u GOROOT go test ./internal/paths/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/paths && git commit -m "feat(paths): 数据根目录与项目分桶（EncodeCWD/ProjectDir/GitRoot/ProjectID）"
```

---

### Task 2: 配置三层合并与新默认值

**Files:**
- Modify: `cmd/agent/config.go`
- Test: `cmd/agent/config_test.go`

**Interfaces:**
- Produces: `type config struct{ Models; ApprovalMode; MCPServers; DelegationMode; Subagent subagentConfig }`、`type subagentConfig struct{ MaxConcurrency int; ApprovalEscalation bool; DefaultTimeout time.Duration; DefaultMaxTurns int }`、`func loadConfigFrom(paths []string) (config, error)`、`func applyDefaults(cfg *config)`、`func mergeConfig(dst *config, src config)`

- [ ] **Step 1: 写失败测试**

```go
package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeYAML(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMergeLaterLayerOverridesNonZero(t *testing.T) {
	dir := t.TempDir()
	user := writeYAML(t, dir, "user.yaml", "approval_mode: yolo\ndelegation_mode: always\nmodels:\n  - provider: deepseek\n    api_key: k\n    model_id: m\n")
	proj := writeYAML(t, dir, "proj.yaml", "approval_mode: always-ask\nsubagent:\n  approval_escalation: true\n")
	cfg, err := loadConfigFrom([]string{user, proj})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ApprovalMode != "always-ask" || cfg.DelegationMode != "always" {
		t.Fatalf("merge wrong: %+v", cfg)
	}
	if !cfg.Subagent.ApprovalEscalation || cfg.Subagent.MaxConcurrency != 4 || cfg.Subagent.DefaultTimeout != 10*time.Minute {
		t.Fatalf("subagent defaults wrong: %+v", cfg.Subagent)
	}
	if len(cfg.Models) != 1 || cfg.Models[0].APIKey != "k" {
		t.Fatalf("models lost: %+v", cfg.Models)
	}
}

func TestDefaultsAreWriteAndPreferred(t *testing.T) {
	dir := t.TempDir()
	p := writeYAML(t, dir, "c.yaml", "models:\n  - provider: qwen\n    api_key: k\n    model_id: m\n")
	cfg, err := loadConfigFrom([]string{p})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ApprovalMode != "write" || cfg.DelegationMode != "preferred" || cfg.Models[0].ContextWindow != 128000 {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
}

func TestMissingFilesAreSkipped(t *testing.T) {
	_, err := loadConfigFrom([]string{filepath.Join(t.TempDir(), "nope.yaml")})
	if err == nil {
		t.Fatal("no models anywhere should error")
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `env -u GOROOT go test ./cmd/agent/ -run 'TestMerge|TestDefaults|TestMissing' -v`
Expected: FAIL（`loadConfigFrom` 未定义）

- [ ] **Step 3: 实现**

把 `cmd/agent/config.go` 改为：

```go
package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"einoclaw-build/internal/paths"
	"einoclaw-build/internal/tool"
)

type ModelProvider string

const (
	ModelProviderQwen     ModelProvider = "qwen"
	ModelProviderOpenAI   ModelProvider = "openai"
	ModelProviderArk      ModelProvider = "ark"
	ModelProviderDeepseek ModelProvider = "deepseek"
)

type modelConfig struct {
	APIKey         string        `yaml:"api_key"`
	BaseURL        string        `yaml:"base_url"`
	Provider       ModelProvider `yaml:"provider"`
	ModelName      string        `yaml:"model_name"`
	ModelID        string        `yaml:"model_id"`
	EnableThinking bool          `yaml:"enable_thinking"`
	ContextWindow  int           `yaml:"context_window"`
}

// subagentConfig 子 agent 运行时配置。
type subagentConfig struct {
	MaxConcurrency     int           `yaml:"max_concurrency"`
	ApprovalEscalation bool          `yaml:"approval_escalation"` // headless 子 agent 的 Prompt 决策升级到父弹窗
	DefaultTimeout     time.Duration `yaml:"default_timeout"`
	DefaultMaxTurns    int           `yaml:"default_max_turns"`
}

type config struct {
	Models         []modelConfig    `yaml:"models"`
	ApprovalMode   string           `yaml:"approval_mode"`   // always-ask/write/yolo，默认 write
	MCPServers     []tool.MCPConfig `yaml:"mcp_servers"`
	DelegationMode string           `yaml:"delegation_mode"` // conservative/preferred/always
	Subagent       subagentConfig   `yaml:"subagent"`
}

// configPaths 返回三层配置路径（用户 → 项目 → 仓库内 legacy）。
func configPaths(cwd string) []string {
	var out []string
	if p, err := paths.UserConfigPath(); err == nil {
		out = append(out, p)
	}
	out = append(out, paths.ProjectConfigPath(cwd), "config.yaml")
	return out
}

// loadConfigFrom 按顺序读取存在的文件并合并（后者覆盖前者的非零值），最后补默认值。
func loadConfigFrom(files []string) (config, error) {
	var cfg config
	found := false
	for _, p := range files {
		data, err := os.ReadFile(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return cfg, err
		}
		var layer config
		if err := yaml.Unmarshal(data, &layer); err != nil {
			return cfg, fmt.Errorf("%s: %w", p, err)
		}
		mergeConfig(&cfg, layer)
		found = true
	}
	if !found || len(cfg.Models) == 0 || cfg.Models[0].APIKey == "" {
		return cfg, errors.New("未找到模型配置：请在 ~/.codeclaw/config.yaml 或 <项目>/.codeclaw/config.yaml 填入 models（参考 example.yaml）")
	}
	applyDefaults(&cfg)
	return cfg, nil
}

func mergeConfig(dst *config, src config) {
	if len(src.Models) > 0 {
		dst.Models = src.Models
	}
	if src.ApprovalMode != "" {
		dst.ApprovalMode = src.ApprovalMode
	}
	if len(src.MCPServers) > 0 {
		dst.MCPServers = append(dst.MCPServers, src.MCPServers...)
	}
	if src.DelegationMode != "" {
		dst.DelegationMode = src.DelegationMode
	}
	if src.Subagent.MaxConcurrency != 0 {
		dst.Subagent.MaxConcurrency = src.Subagent.MaxConcurrency
	}
	if src.Subagent.ApprovalEscalation {
		dst.Subagent.ApprovalEscalation = true
	}
	if src.Subagent.DefaultTimeout != 0 {
		dst.Subagent.DefaultTimeout = src.Subagent.DefaultTimeout
	}
	if src.Subagent.DefaultMaxTurns != 0 {
		dst.Subagent.DefaultMaxTurns = src.Subagent.DefaultMaxTurns
	}
}

func applyDefaults(cfg *config) {
	if cfg.Models[0].ContextWindow == 0 {
		cfg.Models[0].ContextWindow = 128000
	}
	if cfg.ApprovalMode == "" {
		cfg.ApprovalMode = "write"
	}
	if cfg.DelegationMode == "" {
		cfg.DelegationMode = "preferred"
	}
	if cfg.Subagent.MaxConcurrency == 0 {
		cfg.Subagent.MaxConcurrency = 4
	}
	if cfg.Subagent.DefaultTimeout == 0 {
		cfg.Subagent.DefaultTimeout = 10 * time.Minute
	}
	if cfg.Subagent.DefaultMaxTurns == 0 {
		cfg.Subagent.DefaultMaxTurns = 50
	}
}

// loadConfig 读取三层配置；失败则打印原因退出。
func loadConfig(cwd string) config {
	cfg, err := loadConfigFrom(configPaths(cwd))
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	return cfg
}
```

- [ ] **Step 4: 运行测试通过**

Run: `env -u GOROOT go test ./cmd/agent/ -v`
Expected: PASS（`main.go` 暂时仍调用 `loadConfig()` 无参会编译失败——先把 `main.go` 里 `loadConfig()` 改成 `loadConfig(".")`，Task 12 再完整改写）

- [ ] **Step 5: 提交**

```bash
git add cmd/agent/config.go cmd/agent/config_test.go cmd/agent/main.go && git commit -m "feat(config): 三层配置合并，默认 approval_mode=write，新增 subagent 段"
```

---

### Task 3: Session v2 —— Entry、leaf、Replay（含 compaction 展开与悬空修复）

**Files:**
- Modify: `internal/session/entry.go`、`internal/session/session.go`
- Create: `internal/session/replay.go`
- Test: `internal/session/session_test.go`（更新）、`internal/session/replay_test.go`（新）

**Interfaces:**
- Produces: `Entry{ID, ParentID, Timestamp, Version, SessionID, CWD, Title, ParentSession, Model, Message, Usage, Compaction, Init, CustomType, Data}`；常量 `EntryInit = "session_init"`、`EntryCustom = "custom"`、`EntryTitle = "title_change"`；`Compaction{Summary, FirstKeptEntryID, TokensBefore}`；`SessionInit{Agent, SystemPrompt, Task, Tools, OutputSchema, Depth, ParentToolCallID}`；`type Header struct{ ID, CWD, Title, ParentSession, Model string }`；`New(id string, st Storage) (*Session, error)`、`NewWithHeader(h Header, st Storage) (*Session, error)`、`Open(st Storage) (*Session, error)`（读已有文件，重建 leaf）、`(*Session).AppendInit(SessionInit) error`、`AppendCustom(customType string, data any) error`、`Compact(summary, firstKeptID string, tokensBefore int) error`、`LastEntryID() string`、`SetTitle(string) error`、`Header() Header`；纯函数 `buildContext(entries []Entry, leafID string) []message.Message`

- [ ] **Step 1: 写失败测试**（替换 `session_test.go` 的 Fork 测试并新增 `replay_test.go`）

```go
package session

import (
	"testing"

	"einoclaw-build/internal/message"
)

func toolCallMsg(id, name string) message.Message {
	return message.Message{Role: message.RoleAssistant, Blocks: []message.ContentBlock{
		{Kind: message.BlockToolCall, ToolCall: &message.ToolCall{ID: id, Name: name, Args: "{}"}},
	}}
}

func TestAppendBuildsParentChain(t *testing.T) {
	s, _ := New("s1", &MemoryStorage{})
	_ = s.Append(msg(message.RoleUser, "u1"))
	_ = s.Append(msg(message.RoleAssistant, "a1"))
	es, _ := s.Entries()
	if len(es) != 3 || es[1].ID == "" || es[2].ParentID != es[1].ID || es[1].ParentID != "" {
		t.Fatalf("chain wrong: %+v", es)
	}
	if s.LastEntryID() != es[2].ID {
		t.Fatalf("leaf = %q, want %q", s.LastEntryID(), es[2].ID)
	}
}

func TestCompactionKeepsFromFirstKept(t *testing.T) {
	s, _ := New("s1", &MemoryStorage{})
	_ = s.Append(msg(message.RoleUser, "u1"))
	_ = s.Append(msg(message.RoleAssistant, "a1"))
	_ = s.Append(msg(message.RoleUser, "u2")) // kept from here
	keptID := s.LastEntryID()
	_ = s.Append(msg(message.RoleAssistant, "a2"))
	if err := s.Compact("SUMMARY", keptID, 1234); err != nil {
		t.Fatal(err)
	}
	_ = s.Append(msg(message.RoleUser, "u3"))
	ms, _ := s.Replay()
	want := []string{"SUMMARY", "u2", "a2", "u3"}
	if len(ms) != len(want) {
		t.Fatalf("replay = %d msgs, want %d: %+v", len(ms), len(want), ms)
	}
	for i, w := range want {
		if ms[i].Blocks[0].Text != w {
			t.Fatalf("replay[%d] = %q, want %q", i, ms[i].Blocks[0].Text, w)
		}
	}
}

func TestReplayRepairsDanglingToolCall(t *testing.T) {
	s, _ := New("s1", &MemoryStorage{})
	_ = s.Append(msg(message.RoleUser, "u1"))
	_ = s.Append(toolCallMsg("c1", "bash")) // 中断：没有 tool 结果
	_ = s.Append(msg(message.RoleUser, "u2"))
	ms, _ := s.Replay()
	if len(ms) != 4 {
		t.Fatalf("replay = %d, want 4 (repair inserted): %+v", len(ms), ms)
	}
	tr := ms[2]
	if tr.Role != message.RoleTool || tr.Blocks[0].ToolResult == nil || tr.Blocks[0].ToolResult.ToolCallID != "c1" || !tr.Blocks[0].ToolResult.IsError {
		t.Fatalf("repair msg = %+v", tr)
	}
}

func TestOpenV1FileIsLinear(t *testing.T) {
	st := &MemoryStorage{}
	_ = st.Append(Entry{Type: EntrySession, Version: 1, ID: "old"})
	u := msg(message.RoleUser, "u1")
	a := msg(message.RoleAssistant, "a1")
	_ = st.Append(Entry{Type: EntryMessage, Message: &u})
	_ = st.Append(Entry{Type: EntryMessage, Message: &a})
	s, err := Open(st)
	if err != nil {
		t.Fatal(err)
	}
	ms, _ := s.Replay()
	if len(ms) != 2 || ms[1].Blocks[0].Text != "a1" {
		t.Fatalf("v1 replay = %+v", ms)
	}
	_ = s.Append(msg(message.RoleUser, "u2"))
	ms, _ = s.Replay()
	if len(ms) != 3 {
		t.Fatalf("after append = %d", len(ms))
	}
}

func TestInitAndCustomDoNotProduceMessages(t *testing.T) {
	s, _ := NewWithHeader(Header{ID: "c", CWD: "/x", ParentSession: "p"}, &MemoryStorage{})
	_ = s.AppendInit(SessionInit{Agent: "explorer", Task: "t"})
	_ = s.AppendCustom("tool_execution_start", map[string]any{"tool": "bash"})
	_ = s.Append(msg(message.RoleUser, "u"))
	ms, _ := s.Replay()
	if len(ms) != 1 {
		t.Fatalf("replay = %+v", ms)
	}
	if s.Header().ParentSession != "p" || s.Header().CWD != "/x" {
		t.Fatalf("header = %+v", s.Header())
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `env -u GOROOT go test ./internal/session/ -v`
Expected: FAIL（字段/方法未定义）

- [ ] **Step 3: 实现 entry.go**

```go
package session

import (
	"encoding/json"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
)

type EntryType string

const (
	EntrySession    EntryType = "session"
	EntryMessage    EntryType = "message"
	EntryReset      EntryType = "reset_boundary"
	EntryCompaction EntryType = "compaction"
	EntryInit       EntryType = "session_init" // 子 agent 首条：记录任务与约束
	EntryCustom     EntryType = "custom"       // 非 LLM 状态（tool_execution_start / session_exit …）
	EntryTitle      EntryType = "title_change"
)

const CurrentVersion = 2

type Entry struct {
	Type      EntryType `json:"type"`
	ID        string    `json:"id,omitempty"`
	ParentID  string    `json:"parentId,omitempty"`
	Timestamp string    `json:"ts,omitempty"`

	// EntrySession（header）
	Version       int    `json:"version,omitempty"`
	SessionID     string `json:"sessionId,omitempty"`
	CWD           string `json:"cwd,omitempty"`
	Title         string `json:"title,omitempty"` // header 初始标题；EntryTitle 的新标题
	ParentSession string `json:"parentSession,omitempty"`
	Model         string `json:"model,omitempty"`

	// EntryMessage
	Message *message.Message `json:"message,omitempty"`
	Usage   model.Usage      `json:"usage,omitzero"`

	// EntryCompaction
	Compaction *Compaction `json:"compaction,omitempty"`

	// EntryInit
	Init *SessionInit `json:"init,omitempty"`

	// EntryCustom
	CustomType string          `json:"customType,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
}

type Compaction struct {
	Summary          string `json:"summary"`
	FirstKeptEntryID string `json:"firstKeptEntryId,omitempty"`
	TokensBefore     int    `json:"tokensBefore,omitempty"`
}

type SessionInit struct {
	Agent            string         `json:"agent"`
	SystemPrompt     string         `json:"systemPrompt,omitempty"`
	Task             string         `json:"task"`
	Tools            []string       `json:"tools,omitempty"`
	OutputSchema     map[string]any `json:"outputSchema,omitempty"`
	Depth            int            `json:"depth"`
	ParentToolCallID string         `json:"parentToolCallId,omitempty"`
}

// Header 是会话头的稳定视图。
type Header struct {
	ID, CWD, Title, ParentSession, Model string
}
```

- [ ] **Step 4: 实现 replay.go（纯函数）**

```go
package session

import "einoclaw-build/internal/message"

// pathToLeaf 从 leaf 沿 ParentID 回溯，返回 root→leaf 顺序的条目。
func pathToLeaf(entries []Entry, leafID string) []Entry {
	byID := make(map[string]Entry, len(entries))
	for _, e := range entries {
		if e.ID != "" {
			byID[e.ID] = e
		}
	}
	var rev []Entry
	seen := map[string]bool{}
	for id := leafID; id != "" && !seen[id]; {
		seen[id] = true
		e, ok := byID[id]
		if !ok {
			break
		}
		rev = append(rev, e)
		id = e.ParentID
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}

// buildContext 把 root→leaf 路径展开成模型上下文：
// 最新 reset 之后；最新 compaction 展开为 [摘要] + 从 FirstKeptEntryID 起的消息；修复悬空 tool_call。
func buildContext(path []Entry) []message.Message {
	start := 0
	for i, e := range path {
		if e.Type == EntryReset {
			start = i + 1
		}
	}
	path = path[start:]

	// 最新 compaction
	cmpIdx := -1
	for i, e := range path {
		if e.Type == EntryCompaction && e.Compaction != nil {
			cmpIdx = i
		}
	}
	var msgs []message.Message
	if cmpIdx >= 0 {
		c := path[cmpIdx].Compaction
		msgs = append(msgs, message.NewUserMessage(c.Summary))
		keptFrom := cmpIdx + 1 // v1：无 firstKept，保留段被重追加在 compaction 之后
		if c.FirstKeptEntryID != "" {
			for i := 0; i < cmpIdx; i++ {
				if path[i].ID == c.FirstKeptEntryID {
					keptFrom = i
					break
				}
			}
		}
		for i := keptFrom; i < len(path); i++ {
			if i == cmpIdx {
				continue
			}
			if path[i].Type == EntryMessage && path[i].Message != nil {
				msgs = append(msgs, *path[i].Message)
			}
		}
	} else {
		for _, e := range path {
			if e.Type == EntryMessage && e.Message != nil {
				msgs = append(msgs, *e.Message)
			}
		}
	}
	return repairDangling(msgs)
}

// repairDangling 为没有配对 tool 结果的 tool_call 合成一条 error 结果（仅回放，不落盘）。
func repairDangling(msgs []message.Message) []message.Message {
	out := make([]message.Message, 0, len(msgs))
	for i, m := range msgs {
		out = append(out, m)
		if m.Role != message.RoleAssistant {
			continue
		}
		for _, b := range m.Blocks {
			if b.Kind != message.BlockToolCall || b.ToolCall == nil {
				continue
			}
			if !hasResultAfter(msgs, i, b.ToolCall.ID) {
				out = append(out, message.NewToolMessage(b.ToolCall.ID, b.ToolCall.Name, "[interrupted: tool did not run]", true))
			}
		}
	}
	return out
}

func hasResultAfter(msgs []message.Message, from int, callID string) bool {
	for j := from + 1; j < len(msgs); j++ {
		m := msgs[j]
		if m.Role == message.RoleUser || m.Role == message.RoleAssistant {
			// 下一个非 tool 消息之前必须出现结果
			return false
		}
		for _, b := range m.Blocks {
			if b.Kind == message.BlockToolResult && b.ToolResult != nil && b.ToolResult.ToolCallID == callID {
				return true
			}
		}
	}
	return false
}
```

- [ ] **Step 5: 实现 session.go**

```go
package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
)

type Session struct {
	mu      sync.Mutex
	header  Header
	storage Storage
	leafID  string
	entries []Entry // 内存镜像（Open 时从存储加载，之后随 append 增长）
}

func newID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// New 创建会话（只有 id 的 header）。
func New(id string, st Storage) (*Session, error) {
	return NewWithHeader(Header{ID: id}, st)
}

// NewWithHeader 创建会话并写入完整 header。
func NewWithHeader(h Header, st Storage) (*Session, error) {
	e := Entry{Type: EntrySession, Version: CurrentVersion, ID: h.ID, SessionID: h.ID, CWD: h.CWD, Title: h.Title,
		ParentSession: h.ParentSession, Model: h.Model, Timestamp: time.Now().Format(time.RFC3339Nano)}
	if err := st.Append(e); err != nil {
		return nil, err
	}
	return &Session{header: h, storage: st, entries: []Entry{e}}, nil
}

// Open 打开已有存储：读全部条目、重建 header 与 leaf；v1 条目（无 id）按顺序串成线性链。
func Open(st Storage) (*Session, error) {
	entries, err := st.Entries()
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return New("default", st)
	}
	s := &Session{storage: st}
	prev := ""
	for i := range entries {
		e := &entries[i]
		if e.Type == EntrySession {
			s.header = Header{ID: firstNonEmpty(e.SessionID, e.ID), CWD: e.CWD, Title: e.Title, ParentSession: e.ParentSession, Model: e.Model}
			continue
		}
		if e.ID == "" { // v1 兼容：内存中赋 id 串链
			e.ID = "v1-" + newID()
			e.ParentID = prev
		}
		if e.Type == EntryTitle {
			s.header.Title = e.Title
		}
		prev = e.ID
	}
	s.entries = entries
	s.leafID = prev
	return s, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (s *Session) Header() Header { return s.header }

// LastEntryID 返回 leaf id。
func (s *Session) LastEntryID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.leafID
}

func (s *Session) appendLocked(e Entry) error {
	e.ID = newID()
	e.ParentID = s.leafID
	e.Timestamp = time.Now().Format(time.RFC3339Nano)
	if err := s.storage.Append(e); err != nil {
		return err
	}
	s.entries = append(s.entries, e)
	s.leafID = e.ID
	return nil
}

func (s *Session) Append(m message.Message) error { return s.AppendWithUsage(m, model.Usage{}) }

func (s *Session) AppendWithUsage(m message.Message, u model.Usage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(Entry{Type: EntryMessage, Message: &m, Usage: u})
}

func (s *Session) AppendInit(init SessionInit) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(Entry{Type: EntryInit, Init: &init})
}

func (s *Session) AppendCustom(customType string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(Entry{Type: EntryCustom, CustomType: customType, Data: b})
}

func (s *Session) SetTitle(title string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.header.Title = title
	return s.appendLocked(Entry{Type: EntryTitle, Title: title})
}

func (s *Session) Entries() ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, len(s.entries))
	copy(out, s.entries)
	return out, nil
}

func (s *Session) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(Entry{Type: EntryReset})
}

// Replay 返回 leaf 路径上的模型上下文（见 buildContext）。
func (s *Session) Replay() ([]message.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return buildContext(pathToLeaf(s.entries, s.leafID)), nil
}

// Compact 追加一条 compaction：摘要 + 保留起点；不再重追加保留消息。
func (s *Session) Compact(summary, firstKeptID string, tokensBefore int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(Entry{Type: EntryCompaction, Compaction: &Compaction{Summary: summary, FirstKeptEntryID: firstKeptID, TokensBefore: tokensBefore}})
}

// Fork 复制条目（保留 id 链）到新存储，header 带 ParentSession。
func (s *Session) Fork(newID string, st Storage) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	child, err := NewWithHeader(Header{ID: newID, CWD: s.header.CWD, ParentSession: s.header.ID, Model: s.header.Model}, st)
	if err != nil {
		return nil, err
	}
	for _, e := range s.entries {
		if e.Type == EntrySession {
			continue
		}
		if err := child.storage.Append(e); err != nil {
			return nil, err
		}
		child.entries = append(child.entries, e)
		child.leafID = e.ID
	}
	return child, nil
}

func (s *Session) Close() error { return s.storage.Close() }
```

注意：`Replay` 需要条目 id 来定位 `FirstKeptEntryID`，`context.Manager` 在压缩时用 `Entries()` 找到"保留段第一条 message 条目"的 id（Task 6）。

- [ ] **Step 6: 更新旧测试**：`TestForkCopiesAndIsolates` 保持不变（语义仍成立）；`compact_test.go` 里调用 `Compact(summary, kept []message.Message)` 的测试改为 `Compact(summary, keptID, 0)` 形式（读该文件后按新签名改写断言）。

- [ ] **Step 7: 运行测试通过**

Run: `env -u GOROOT go test ./internal/session/ -v`
Expected: PASS

- [ ] **Step 8: 提交**

```bash
git add internal/session && git commit -m "feat(session): v2 条目（id/parentId/ts）、leaf 回放、FirstKeptEntryID 压缩、悬空 tool_call 修复"
```

---

### Task 4: Session Manager —— 项目目录、带标题的清单、产物目录

**Files:**
- Modify: `internal/session/manager.go`
- Test: `internal/session/manager_test.go`（更新）

**Interfaces:**
- Produces: `type Info struct{ ID, Title, FirstUser, Path string; ModTime time.Time }`、`NewManager(projectDir string) (*Manager, error)`、`(*Manager).Current(cwd string) (*Session, error)`、`New(cwd string) (*Session, error)`、`Switch(idPrefix string) (*Session, error)`、`List() ([]Info, error)`、`ArtifactDir(s *Session) (string, error)`

- [ ] **Step 1: 写失败测试**

```go
package session

import (
	"path/filepath"
	"strings"
	"testing"

	"einoclaw-build/internal/message"
)

func TestManagerNewListSwitch(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s1, _ := m.New("/proj")
	_ = s1.Append(msg(message.RoleUser, "first question"))
	_ = s1.SetTitle("修复登录")
	s1.Close()
	s2, _ := m.New("/proj")
	s2.Close()

	infos, err := m.List()
	if err != nil || len(infos) != 2 {
		t.Fatalf("list = %+v err %v", infos, err)
	}
	var found bool
	for _, in := range infos {
		if in.ID == s1.Header().ID {
			found = true
			if in.Title != "修复登录" || in.FirstUser != "first question" {
				t.Fatalf("info = %+v", in)
			}
		}
	}
	if !found {
		t.Fatal("s1 not listed")
	}
	// 前缀切换
	sw, err := m.Switch(s1.Header().ID[:6])
	if err != nil || sw.Header().ID != s1.Header().ID {
		t.Fatalf("switch = %v err %v", sw, err)
	}
	sw.Close()
	cur, _ := m.Current("/proj")
	if cur.Header().ID != s1.Header().ID {
		t.Fatalf("current = %q", cur.Header().ID)
	}
	ad, _ := m.ArtifactDir(cur)
	if !strings.HasSuffix(ad, cur.Header().ID) || filepath.Dir(ad) != m.dir {
		t.Fatalf("artifact dir = %q", ad)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `env -u GOROOT go test ./internal/session/ -run TestManager -v`
Expected: FAIL

- [ ] **Step 3: 实现**

```go
package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"einoclaw-build/internal/message"
)

// Manager 管理一个项目桶里的会话：<dir>/<id>.jsonl、<dir>/<id>/（产物）、<dir>/current。
type Manager struct{ dir string }

type Info struct {
	ID, Title, FirstUser, Path string
	ModTime                    time.Time
}

func NewManager(projectDir string) (*Manager, error) {
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		return nil, err
	}
	return &Manager{dir: projectDir}, nil
}

func (m *Manager) Dir() string { return m.dir }

// Current 打开 current 指向的会话；无则新建。
func (m *Manager) Current(cwd string) (*Session, error) {
	id := m.currentID()
	if id == "" {
		return m.New(cwd)
	}
	if _, err := os.Stat(m.path(id)); err != nil {
		return m.New(cwd)
	}
	return m.open(id)
}

func (m *Manager) New(cwd string) (*Session, error) {
	id := time.Now().Format("20060102-150405") + "_" + newID()[:6]
	st, err := NewFileStorage(m.path(id))
	if err != nil {
		return nil, err
	}
	s, err := NewWithHeader(Header{ID: id, CWD: cwd}, st)
	if err != nil {
		return nil, err
	}
	return s, m.setCurrent(id)
}

// Switch 按 id 或唯一前缀切换。
func (m *Manager) Switch(idPrefix string) (*Session, error) {
	infos, err := m.List()
	if err != nil {
		return nil, err
	}
	var match []string
	for _, in := range infos {
		if in.ID == idPrefix {
			match = []string{in.ID}
			break
		}
		if strings.HasPrefix(in.ID, idPrefix) {
			match = append(match, in.ID)
		}
	}
	if len(match) != 1 {
		return nil, fmt.Errorf("会话 %q 匹配到 %d 个", idPrefix, len(match))
	}
	s, err := m.open(match[0])
	if err != nil {
		return nil, err
	}
	return s, m.setCurrent(match[0])
}

// List 列出会话（最近在前），只读每个文件前 8 行取标题/首条用户消息。
func (m *Manager) List() ([]Info, error) {
	des, err := os.ReadDir(m.dir)
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, de := range des {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".jsonl") || strings.HasPrefix(de.Name(), "agent-") {
			continue
		}
		st, err := de.Info()
		if err != nil {
			continue
		}
		id := strings.TrimSuffix(de.Name(), ".jsonl")
		in := Info{ID: id, Path: m.path(id), ModTime: st.ModTime()}
		in.Title, in.FirstUser = peek(in.Path)
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	return out, nil
}

// peek 读前 8 行：header 标题 / title_change / 首条 user 文本。
func peek(path string) (title, firstUser string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for i := 0; i < 8 && sc.Scan(); i++ {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		switch e.Type {
		case EntrySession, EntryTitle:
			if e.Title != "" {
				title = e.Title
			}
		case EntryMessage:
			if firstUser == "" && e.Message != nil && e.Message.Role == message.RoleUser {
				for _, b := range e.Message.Blocks {
					if b.Kind == message.BlockText {
						firstUser = strings.SplitN(strings.TrimSpace(b.Text), "\n", 2)[0]
						break
					}
				}
			}
		}
	}
	return title, firstUser
}

// ArtifactDir 返回会话产物目录并确保存在。
func (m *Manager) ArtifactDir(s *Session) (string, error) {
	d := filepath.Join(m.dir, s.Header().ID)
	return d, os.MkdirAll(d, 0o755)
}

func (m *Manager) path(id string) string { return filepath.Join(m.dir, id+".jsonl") }

func (m *Manager) currentID() string {
	b, err := os.ReadFile(filepath.Join(m.dir, "current"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (m *Manager) setCurrent(id string) error {
	return os.WriteFile(filepath.Join(m.dir, "current"), []byte(id), 0o644)
}

func (m *Manager) open(id string) (*Session, error) {
	st, err := NewFileStorage(m.path(id))
	if err != nil {
		return nil, err
	}
	s, err := Open(st)
	if err != nil {
		st.Close()
		return nil, errors.Join(err)
	}
	return s, nil
}
```

`FileStorage.Entries()` 读文件用 `bufio.Scanner`（1MB 行缓冲）替代 `strings.Split`，其余不变。

- [ ] **Step 4: 运行测试通过**

Run: `env -u GOROOT go test ./internal/session/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/session && git commit -m "feat(session): 项目目录 Manager，带标题/首句的会话清单，前缀 resume，产物目录"
```

---

### Task 5: 模型层 —— ModelStream 接口、完整 JSON Schema、错误分类

**Files:**
- Modify: `internal/model/model.go`、`internal/model/eino.go`
- Test: `internal/model/errors_test.go`（新）、`internal/model/eino_test.go`（更新）

**Interfaces:**
- Produces: `ToolSpec.Required []string`；`type ModelStream interface{ Recv() (ModelEvent, error); Usage() Usage; Close() }`；`Model.Stream(...) (ModelStream, error)`；`func IsContextOverflow(err error) bool`；`func IsRetryable(err error) bool`

- [ ] **Step 1: 写失败测试**

```go
package model

import (
	"errors"
	"testing"
)

func TestIsContextOverflow(t *testing.T) {
	for _, s := range []string{
		"This model's maximum context length is 128000 tokens",
		"context_length_exceeded",
		"prompt is too long: 210000 tokens",
		"input length and max_tokens exceed context limit",
	} {
		if !IsContextOverflow(errors.New(s)) {
			t.Errorf("should be overflow: %q", s)
		}
	}
	if IsContextOverflow(errors.New("rate limit exceeded")) || IsContextOverflow(nil) {
		t.Error("false positive")
	}
}

func TestIsRetryable(t *testing.T) {
	for _, s := range []string{"429 Too Many Requests", "status 503", "connection reset by peer", "server overloaded", "i/o timeout"} {
		if !IsRetryable(errors.New(s)) {
			t.Errorf("should retry: %q", s)
		}
	}
	if IsRetryable(errors.New("invalid api key")) || IsRetryable(errors.New("context_length_exceeded")) {
		t.Error("false positive")
	}
}

func TestToSchemaToolsKeepsNestedSchema(t *testing.T) {
	specs := []ToolSpec{{Name: "task", Parameters: map[string]any{
		"tasks": map[string]any{"type": "array", "items": map[string]any{
			"type": "object", "properties": map[string]any{"subagent": map[string]any{"type": "string"}}, "required": []string{"subagent"},
		}},
	}, Required: []string{"tasks"}}}
	infos := toSchemaTools(specs)
	js, err := infos[0].ParamsOneOf.ToJSONSchema()
	if err != nil || js == nil {
		t.Fatalf("schema err %v", err)
	}
	tasks, ok := js.Properties.Get("tasks")
	if !ok || tasks.Items == nil || tasks.Items.Properties == nil {
		t.Fatalf("nested items lost: %+v", tasks)
	}
	if len(js.Required) != 1 || js.Required[0] != "tasks" {
		t.Fatalf("required = %v", js.Required)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `env -u GOROOT go test ./internal/model/ -v`
Expected: FAIL

- [ ] **Step 3: 实现 model.go 改动**

```go
// ToolSpec 增加 Required
type ToolSpec struct {
	Name        string
	Description string
	Parameters  map[string]any // JSON Schema properties（可含嵌套 items/properties/enum/description）
	Required    []string
}

// ModelStream 一次流式调用的事件流（*Stream 实现；测试可注入 fake）。
type ModelStream interface {
	Recv() (ModelEvent, error)
	Usage() Usage
	Close()
}

type Model interface {
	Stream(ctx context.Context, msgs []message.Message, tools []ToolSpec) (ModelStream, error)
}
```

新文件 `internal/model/errors.go`：

```go
package model

import "strings"

var overflowMarkers = []string{
	"context_length_exceeded", "maximum context length", "context length", "prompt is too long",
	"too many tokens", "exceed context limit", "exceeds the context", "input length",
	"context window", "request too large",
}

var retryMarkers = []string{
	"429", "too many requests", "rate limit", "ratelimit", "500", "502", "503", "504",
	"overloaded", "server error", "internal error", "service unavailable", "timeout", "timed out",
	"connection reset", "connection refused", "broken pipe", "eof", "temporarily unavailable", "try again",
}

// IsContextOverflow 判断模型错误是否为上下文溢出（走压缩恢复通道，绝不重试）。
func IsContextOverflow(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, m := range overflowMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// IsRetryable 判断是否为可重试的瞬时错误（限流/5xx/网络）。溢出不可重试。
func IsRetryable(err error) bool {
	if err == nil || IsContextOverflow(err) {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, m := range retryMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: 实现 eino.go 改动**

```go
import "github.com/eino-contrib/jsonschema"

func (m *einoModel) Stream(ctx context.Context, msgs []message.Message, tools []ToolSpec) (ModelStream, error) { /* 同原逻辑，返回 *Stream */ }

// toSchemaTools 把 ToolSpec 转成 eino ToolInfo，用完整 JSON Schema 透传嵌套结构。
func toSchemaTools(tools []ToolSpec) []*schema.ToolInfo {
	out := make([]*schema.ToolInfo, 0, len(tools))
	for _, t := range tools {
		props := t.Parameters
		if props == nil {
			props = map[string]any{}
		}
		required := t.Required
		if required == nil {
			required = []string{}
		}
		raw := map[string]any{"type": "object", "properties": props, "required": required}
		b, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		var js jsonschema.Schema
		if err := json.Unmarshal(b, &js); err != nil {
			continue
		}
		out = append(out, &schema.ToolInfo{Name: t.Name, Desc: t.Description, ParamsOneOf: schema.NewParamsOneOfByJSONSchema(&js)})
	}
	return out
}
```

删除 `dataTypeOf`。`eino_test.go` 若引用 `dataTypeOf`，改为断言 `ToJSONSchema()` 的 `Type`。

- [ ] **Step 5: 运行测试通过**

Run: `env -u GOROOT go test ./internal/model/ -v`
Expected: PASS

- [ ] **Step 6: 提交**

```bash
git add internal/model && git commit -m "feat(model): ModelStream 接口、工具 JSON Schema 完整透传、溢出/可重试错误分类"
```

---

### Task 6: 上下文管理 v2 —— 全块估算、安全切点、完整序列化、Build/Record/Compact/RecoverOverflow

**Files:**
- Modify: `internal/context/tokenizer.go`、`internal/context/compaction.go`、`internal/context/manager.go`
- Test: `internal/context/context_test.go`（更新 + 新增）

**Interfaces:**
- Produces: `New(s *session.Session, sum summarizer, window, keepRecent int, system func(context.Context) []message.Message) *Manager`；`(*Manager).Build(ctx) ([]message.Message, error)`、`Record(m message.Message, u model.Usage) error`、`ShouldCompact(u model.Usage) bool`、`Compact(ctx) (bool, error)`、`RecoverOverflow(ctx) (bool, error)`、`Session() *session.Session`；导出 `EstimateTokens(m message.Message) int`

- [ ] **Step 1: 写失败测试**（替换 `context_test.go`）

```go
package context

import (
	"context"
	"strings"
	"testing"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
	"einoclaw-build/internal/session"
)

func ctxMsg(role message.Role, text string) message.Message {
	return message.Message{Role: role, Blocks: []message.ContentBlock{{Kind: message.BlockText, Text: text}}}
}
func callMsg(id, name, args string) message.Message {
	return message.Message{Role: message.RoleAssistant, Blocks: []message.ContentBlock{{Kind: message.BlockToolCall, ToolCall: &message.ToolCall{ID: id, Name: name, Args: args}}}}
}

type fakeSummarizer struct {
	got []message.Message
	out string
}

func (f *fakeSummarizer) Summarize(_ context.Context, msgs []message.Message) (string, error) {
	f.got = msgs
	return f.out, nil
}

func TestEstimateTokensCountsToolBlocks(t *testing.T) {
	tr := message.NewToolMessage("c1", "read", strings.Repeat("x", 200), false)
	if got := EstimateTokens(tr); got < 100 {
		t.Fatalf("tool result tokens = %d, want >= 100", got)
	}
	if got := EstimateTokens(callMsg("c1", "bash", strings.Repeat("y", 100))); got < 50 {
		t.Fatalf("tool call tokens = %d", got)
	}
}

func TestFindCutPointNeverSplitsToolPair(t *testing.T) {
	msgs := []message.Message{
		ctxMsg(message.RoleUser, "u1"),
		callMsg("c1", "read", strings.Repeat("a", 40)),
		message.NewToolMessage("c1", "read", strings.Repeat("b", 400), false),
		ctxMsg(message.RoleAssistant, "a1"),
		ctxMsg(message.RoleUser, "u2"),
	}
	// keep 很小 → 候选落在 u2；安全
	if got := findCutPoint(msgs, 1); got != 4 {
		t.Fatalf("cut = %d, want 4", got)
	}
	// keep 覆盖到 tool 结果 → 候选 2（tool 消息）不安全 → 回退到 0（u1）
	if got := findCutPoint(msgs, 300); got != 0 {
		t.Fatalf("cut = %d, want 0 (u1)", got)
	}
}

func TestSerializeIncludesToolCallsAndResults(t *testing.T) {
	s := serializeConversation([]message.Message{
		callMsg("c1", "read_file", `{"file_path":"a.go"}`),
		message.NewToolMessage("c1", "read_file", "package main", false),
	})
	if !strings.Contains(s, "tool_call read_file") || !strings.Contains(s, "package main") {
		t.Fatalf("serialized = %q", s)
	}
}

func TestCompactWritesFirstKeptAndRebuilds(t *testing.T) {
	s, _ := session.New("s1", &session.MemoryStorage{})
	fs := &fakeSummarizer{out: "SUMMARY"}
	cm := New(s, fs, 1000, 6, func(context.Context) []message.Message { return []message.Message{message.NewSystemMessage("SYS")} })
	_ = cm.Record(ctxMsg(message.RoleUser, "m0"), model.Usage{})
	_ = cm.Record(ctxMsg(message.RoleAssistant, "m1"), model.Usage{})
	_ = cm.Record(ctxMsg(message.RoleUser, "m2"), model.Usage{})
	_ = cm.Record(ctxMsg(message.RoleAssistant, "m3"), model.Usage{})

	if !cm.ShouldCompact(model.Usage{PromptTokens: 600}) {
		t.Fatal("600 > 500 should compact")
	}
	did, err := cm.Compact(context.Background())
	if err != nil || !did {
		t.Fatalf("compact did=%v err=%v", did, err)
	}
	if len(fs.got) != 2 || fs.got[0].Blocks[0].Text != "m0" {
		t.Fatalf("summarize input = %+v", fs.got)
	}
	msgs, _ := cm.Build(context.Background())
	want := []string{"SYS", "SUMMARY", "m2", "m3"}
	if len(msgs) != len(want) {
		t.Fatalf("build = %+v", msgs)
	}
	for i, w := range want {
		if msgs[i].Blocks[0].Text != w {
			t.Fatalf("build[%d] = %q want %q", i, msgs[i].Blocks[0].Text, w)
		}
	}
}

func TestRecoverOverflowCutsDeeper(t *testing.T) {
	s, _ := session.New("s1", &session.MemoryStorage{})
	fs := &fakeSummarizer{out: "S"}
	cm := New(s, fs, 1000, 1000, func(context.Context) []message.Message { return nil })
	for i := 0; i < 6; i++ {
		_ = cm.Record(ctxMsg(message.RoleUser, strings.Repeat("u", 50)), model.Usage{})
		_ = cm.Record(ctxMsg(message.RoleAssistant, strings.Repeat("a", 50)), model.Usage{})
	}
	// keep=1000 覆盖全部 → 正常 Compact 无可压内容
	if did, _ := cm.Compact(context.Background()); did {
		t.Fatal("nothing to compact at keep=1000")
	}
	did, err := cm.RecoverOverflow(context.Background())
	if err != nil || !did {
		t.Fatalf("recover did=%v err=%v", did, err)
	}
	msgs, _ := cm.Build(context.Background())
	if len(msgs) >= 12 {
		t.Fatalf("overflow recovery should shrink context, got %d", len(msgs))
	}
}

func TestSummarizePromptRequiresSixFields(t *testing.T) {
	p := summarizePrompt([]message.Message{ctxMsg(message.RoleUser, "hi")})
	sys := p[0].Blocks[0].Text
	for _, field := range []string{"目标", "当前状态", "决策", "文件", "失败", "下一步"} {
		if !strings.Contains(sys, field) {
			t.Errorf("prompt 缺少字段 %q", field)
		}
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `env -u GOROOT go test ./internal/context/ -v`
Expected: FAIL

- [ ] **Step 3: 实现 tokenizer.go**

```go
package context

import "einoclaw-build/internal/message"

// EstimateTokens 粗估一条消息的 token：所有块按 2 rune ≈ 1 token，每消息 +4 framing。
func EstimateTokens(m message.Message) int {
	n := 0
	for _, b := range m.Blocks {
		switch b.Kind {
		case message.BlockText:
			n += len([]rune(b.Text)) / 2
		case message.BlockThinking:
			n += len([]rune(b.Thinking)) / 2
		case message.BlockToolCall:
			if b.ToolCall != nil {
				n += (len(b.ToolCall.Name) + len([]rune(b.ToolCall.Args))) / 2
			}
		case message.BlockToolResult:
			if b.ToolResult != nil {
				n += len([]rune(b.ToolResult.Content)) / 2
			}
		}
	}
	return n + 4
}

func estimateTokens(m message.Message) int { return EstimateTokens(m) }
```

- [ ] **Step 4: 实现 compaction.go**

```go
// findCutPoint 从新到旧累计到 keepTokens 得到候选，再向旧回退到安全切点。
// 安全切点：user 消息，或没有 tool_call 的 assistant 消息。返回 0 表示无可压内容。
func findCutPoint(msgs []message.Message, keepTokens int) int {
	acc := 0
	i := 0
	for j := len(msgs) - 1; j >= 0; j-- {
		acc += estimateTokens(msgs[j])
		if acc >= keepTokens {
			i = j
			break
		}
	}
	for i > 0 && !safeCut(msgs, i) {
		i--
	}
	return i
}

func safeCut(msgs []message.Message, i int) bool {
	m := msgs[i]
	switch m.Role {
	case message.RoleUser:
		return true
	case message.RoleAssistant:
		for _, b := range m.Blocks {
			if b.Kind == message.BlockToolCall {
				return false
			}
		}
		return true
	}
	return false
}

const (
	resultHead = 1000
	resultTail = 500
)

// serializeConversation 把消息序列化成摘要器输入：含工具调用与（截断的）工具结果，不含 thinking。
func serializeConversation(msgs []message.Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		for _, b := range m.Blocks {
			switch b.Kind {
			case message.BlockText:
				if b.Text != "" {
					fmt.Fprintf(&sb, "%s: %s\n", m.Role, b.Text)
				}
			case message.BlockToolCall:
				if b.ToolCall != nil {
					fmt.Fprintf(&sb, "assistant→tool_call %s(%s)\n", b.ToolCall.Name, clip(b.ToolCall.Args, 300, 0))
				}
			case message.BlockToolResult:
				if b.ToolResult != nil {
					fmt.Fprintf(&sb, "tool(%s): %s\n", b.ToolResult.Name, clip(b.ToolResult.Content, resultHead, resultTail))
				}
			}
		}
	}
	return sb.String()
}

func clip(s string, head, tail int) string {
	r := []rune(s)
	if len(r) <= head+tail {
		return s
	}
	if tail == 0 {
		return string(r[:head]) + "…"
	}
	return string(r[:head]) + "\n…(elided)…\n" + string(r[len(r)-tail:])
}
```

保留 `sixFieldInstruction`、`summarizePrompt`、`messageText`。

- [ ] **Step 5: 实现 manager.go**

```go
type Manager struct {
	session    *session.Session
	summarizer summarizer
	window     int
	keepRecent int
	system     func(ctx context.Context) []message.Message
}

func New(s *session.Session, sum summarizer, window, keepRecent int, system func(context.Context) []message.Message) *Manager {
	if system == nil {
		system = func(context.Context) []message.Message { return nil }
	}
	return &Manager{session: s, summarizer: sum, window: window, keepRecent: keepRecent, system: system}
}

func (m *Manager) Session() *session.Session { return m.session }
func (m *Manager) SetSession(s *session.Session) { m.session = s }

func (m *Manager) threshold() int { /* 同原实现 */ }

func (m *Manager) Build(ctx context.Context) ([]message.Message, error) {
	hist, err := m.session.Replay()
	if err != nil {
		return nil, err
	}
	return append(m.system(ctx), hist...), nil
}

func (m *Manager) Record(msg message.Message, u model.Usage) error { return m.session.AppendWithUsage(msg, u) }

func (m *Manager) ShouldCompact(u model.Usage) bool { return u.PromptTokens > m.threshold() }

func (m *Manager) Compact(ctx context.Context) (bool, error) { return m.compact(ctx, m.keepRecent) }

// RecoverOverflow 溢出恢复：把保留量减半再压缩；若仍无可压内容返回 false。
func (m *Manager) RecoverOverflow(ctx context.Context) (bool, error) {
	keep := m.keepRecent / 2
	if keep < 512 {
		keep = 512
	}
	did, err := m.compact(ctx, keep)
	if err != nil || did {
		return did, err
	}
	return m.compact(ctx, 1)
}

func (m *Manager) compact(ctx context.Context, keep int) (bool, error) {
	msgs, err := m.session.Replay()
	if err != nil {
		return false, err
	}
	cut := findCutPoint(msgs, keep)
	if cut <= 0 {
		return false, nil
	}
	summary, err := m.summarizer.Summarize(ctx, msgs[:cut])
	if err != nil {
		return false, err
	}
	firstKept, err := m.entryIDOfMessageIndex(cut)
	if err != nil {
		return false, err
	}
	before := 0
	for _, mm := range msgs {
		before += estimateTokens(mm)
	}
	return true, m.session.Compact(summary, firstKept, before)
}

// entryIDOfMessageIndex 把 Replay 结果里的消息下标映射回 session 条目 id：
// 回放 = [可选摘要] + 当前上下文里的 message 条目；修复合成的 tool 消息不在条目里，映射时跳过。
func (m *Manager) entryIDOfMessageIndex(idx int) (string, error) {
	ids, err := m.session.ContextEntryIDs()
	if err != nil {
		return "", err
	}
	if idx < len(ids) {
		return ids[idx], nil
	}
	return "", fmt.Errorf("cut index %d out of range %d", idx, len(ids))
}
```

这要求 `session` 增加 `ContextEntryIDs() ([]string, error)`：返回当前上下文每条消息对应的条目 id 列表，顺序与 `Replay()` 一致（摘要占位为 compaction 条目 id；合成修复消息占位为其所属 assistant 条目 id）。在 Task 3 的 `replay.go` 里 `buildContext` 改为同时返回 `[]string` ids（`buildContextWithIDs`），`Replay` 取消息部分，`ContextEntryIDs` 取 id 部分。`repairDangling` 插入合成消息时复制所属 assistant 的 id。

- [ ] **Step 6: 运行测试通过**

Run: `env -u GOROOT go test ./internal/context/ ./internal/session/ -v`
Expected: PASS

- [ ] **Step 7: 提交**

```bash
git add internal/context internal/session && git commit -m "feat(context): Manager v2（Build/Record/Compact/RecoverOverflow），全块估算，配对安全切点，完整序列化"
```

---

### Task 7: Agent 循环 v2 —— Context 真相源、终止型工具、溢出/重试双通道

**Files:**
- Modify: `internal/agent/agent.go`、`internal/agent/loop.go`、`internal/agent/event.go`、`internal/tool/tool.go`
- Test: `internal/agent/loop_test.go`（重写）

**Interfaces:**
- Consumes: `model.ModelStream`、`tool.Executor.ExecuteAll`、`tool.Registry.Get`
- Produces: `type Context interface{...}`（见 spec §7）；`agent.New(name string, m model.Model, tools *tool.Registry, exec *tool.Executor, cc Context) *Agent`；`(*Agent).Run(ctx, steer <-chan message.Message) <-chan AgentEvent`；`(*Agent).SetMaxIterations(int)`；`NewMemoryContext(system []message.Message) *MemoryContext`；事件 `EventCompaction{Compaction: &CompactionInfo{Reason}}`、`EventRetry{Retry: &RetryInfo{Attempt, Delay, Err}}`、`EventTerminated{Terminated: &TerminatedInfo{ToolName}}`；`tool.Terminal` 接口 `IsTerminal() bool`

- [ ] **Step 1: 写失败测试**（重写 `loop_test.go`，保留 `fakeStream` 并加 `fakeModel`）

```go
package agent

import (
	"context"
	"errors"
	"io"
	"testing"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
	"einoclaw-build/internal/tool"
)

type fakeStream struct {
	events []model.ModelEvent
	err    error
	i      int
	usage  model.Usage
}

func (f *fakeStream) Recv() (model.ModelEvent, error) {
	if f.i < len(f.events) {
		ev := f.events[f.i]
		f.i++
		return ev, nil
	}
	if f.err != nil {
		return model.ModelEvent{}, f.err
	}
	return model.ModelEvent{}, io.EOF
}
func (f *fakeStream) Usage() model.Usage { return f.usage }
func (f *fakeStream) Close()             {}

// fakeModel 按步返回脚本：每步要么一个 stream，要么一个 Stream() 错误。
type fakeModel struct {
	steps []func() (model.ModelStream, error)
	calls [][]message.Message
}

func (f *fakeModel) Stream(_ context.Context, msgs []message.Message, _ []model.ToolSpec) (model.ModelStream, error) {
	f.calls = append(f.calls, msgs)
	if len(f.steps) == 0 {
		return &fakeStream{events: []model.ModelEvent{{Text: "done"}}}, nil
	}
	s := f.steps[0]
	f.steps = f.steps[1:]
	return s()
}

func textStep(t string) func() (model.ModelStream, error) {
	return func() (model.ModelStream, error) { return &fakeStream{events: []model.ModelEvent{{Text: t}}}, nil }
}
func callStep(id, name, args string) func() (model.ModelStream, error) {
	return func() (model.ModelStream, error) {
		return &fakeStream{events: []model.ModelEvent{{ToolCalls: []model.ToolCallDelta{{Index: 0, CallID: id, Name: name, Args: args}}}}}, nil
	}
}
func errStep(err error) func() (model.ModelStream, error) {
	return func() (model.ModelStream, error) { return nil, err }
}

type echoTool struct{ terminal bool }

func (e echoTool) Name() string                   { if e.terminal { return "finish" }; return "echo" }
func (echoTool) Description() string               { return "" }
func (echoTool) Parameters() map[string]any        { return map[string]any{"v": map[string]any{"type": "string"}} }
func (echoTool) Tier() permission.Tier             { return permission.TierRead }
func (echoTool) Concurrency() tool.Concurrency     { return tool.ConcurrencyShared }
func (e echoTool) IsTerminal() bool                { return e.terminal }
func (echoTool) Execute(_ context.Context, args map[string]any, sink *runtime.Sink) error {
	v, _ := args["v"].(string)
	sink.Write([]byte("echo:" + v))
	return nil
}

func newTestAgent(fm *fakeModel, cc Context) *Agent {
	reg := tool.NewRegistry()
	reg.Register(echoTool{})
	reg.Register(echoTool{terminal: true})
	return New("t", fm, reg, tool.NewExecutor(reg, permission.ModeYolo, nil), cc)
}

func drain(ch <-chan AgentEvent) []AgentEvent {
	var out []AgentEvent
	for e := range ch {
		out = append(out, e)
	}
	return out
}

func TestLoopRecordsToolRoundTrip(t *testing.T) {
	fm := &fakeModel{steps: []func() (model.ModelStream, error){callStep("c1", "echo", `{"v":"hi"}`), textStep("ok")}}
	cc := NewMemoryContext([]message.Message{message.NewSystemMessage("SYS")})
	_ = cc.Record(message.NewUserMessage("go"), model.Usage{})
	evs := drain(newTestAgent(fm, cc).Run(context.Background(), nil))
	msgs, _ := cc.Build(context.Background())
	// SYS, user, assistant(call), tool(result), assistant(ok)
	if len(msgs) != 5 || msgs[3].Role != message.RoleTool || msgs[3].Blocks[0].ToolResult.Content != "echo:hi" {
		t.Fatalf("context = %+v", msgs)
	}
	if len(fm.calls) != 2 || len(fm.calls[1]) != 4 {
		t.Fatalf("second model call should see 4 msgs, got %d", len(fm.calls[1]))
	}
	var sawEnd bool
	for _, e := range evs {
		if e.Type == EventAgentEnd {
			sawEnd = true
		}
	}
	if !sawEnd {
		t.Fatal("no agent_end")
	}
}

func TestLoopStopsOnTerminalTool(t *testing.T) {
	fm := &fakeModel{steps: []func() (model.ModelStream, error){callStep("c1", "finish", `{"v":"x"}`), textStep("SHOULD NOT RUN")}}
	cc := NewMemoryContext(nil)
	_ = cc.Record(message.NewUserMessage("go"), model.Usage{})
	evs := drain(newTestAgent(fm, cc).Run(context.Background(), nil))
	if len(fm.calls) != 1 {
		t.Fatalf("model called %d times, want 1", len(fm.calls))
	}
	var term bool
	for _, e := range evs {
		if e.Type == EventTerminated && e.Terminated.ToolName == "finish" {
			term = true
		}
	}
	if !term {
		t.Fatal("no EventTerminated")
	}
}

func TestLoopRecoversFromOverflow(t *testing.T) {
	fm := &fakeModel{steps: []func() (model.ModelStream, error){errStep(errors.New("context_length_exceeded")), textStep("after")}}
	cc := NewMemoryContext(nil)
	cc.recoverOK = true
	_ = cc.Record(message.NewUserMessage("go"), model.Usage{})
	evs := drain(newTestAgent(fm, cc).Run(context.Background(), nil))
	if cc.recovers != 1 || len(fm.calls) != 2 {
		t.Fatalf("recovers=%d calls=%d", cc.recovers, len(fm.calls))
	}
	var cmp bool
	for _, e := range evs {
		if e.Type == EventCompaction && e.Compaction.Reason == "overflow" {
			cmp = true
		}
	}
	if !cmp {
		t.Fatal("no overflow compaction event")
	}
}

func TestLoopRetriesTransientError(t *testing.T) {
	fm := &fakeModel{steps: []func() (model.ModelStream, error){errStep(errors.New("429 Too Many Requests")), textStep("after")}}
	cc := NewMemoryContext(nil)
	_ = cc.Record(message.NewUserMessage("go"), model.Usage{})
	a := newTestAgent(fm, cc)
	a.retryBase = 0 // 测试不等待
	evs := drain(a.Run(context.Background(), nil))
	var retried bool
	for _, e := range evs {
		if e.Type == EventRetry {
			retried = true
		}
	}
	if !retried || len(fm.calls) != 2 {
		t.Fatalf("retried=%v calls=%d", retried, len(fm.calls))
	}
}

func TestLoopMidTurnCompaction(t *testing.T) {
	fm := &fakeModel{steps: []func() (model.ModelStream, error){
		func() (model.ModelStream, error) {
			return &fakeStream{events: []model.ModelEvent{{ToolCalls: []model.ToolCallDelta{{Index: 0, CallID: "c1", Name: "echo", Args: `{"v":"1"}`}}}}, usage: model.Usage{PromptTokens: 999}}, nil
		},
		textStep("end"),
	}}
	cc := NewMemoryContext(nil)
	cc.compactAt = 500
	_ = cc.Record(message.NewUserMessage("go"), model.Usage{})
	evs := drain(newTestAgent(fm, cc).Run(context.Background(), nil))
	var mid bool
	for _, e := range evs {
		if e.Type == EventCompaction && e.Compaction.Reason == "mid-turn" {
			mid = true
		}
	}
	if !mid || cc.compacts != 1 {
		t.Fatalf("mid=%v compacts=%d", mid, cc.compacts)
	}
}

func TestConsumeStreamErrorEmitsError(t *testing.T) {
	fs := &fakeStream{err: errors.New("boom")}
	var got []AgentEvent
	_, _, streamErr := consumeStream(context.Background(), fs, func(e AgentEvent) { got = append(got, e) })
	if streamErr == nil || len(got) != 3 {
		t.Fatalf("err=%v events=%d", streamErr, len(got))
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `env -u GOROOT go test ./internal/agent/ -v`
Expected: FAIL

- [ ] **Step 3: 实现 `tool.Terminal` 与事件**

`internal/tool/tool.go` 追加：

```go
// Terminal 可选接口：工具执行成功后终止本次 run（如子 agent 的 yield）。
type Terminal interface{ IsTerminal() bool }
```

`internal/agent/event.go` 追加：

```go
const (
	// …原有…
	EventCompaction // 上下文已压缩
	EventRetry      // 模型错误重试中
	EventTerminated // 终止型工具结束了 run
)

type CompactionInfo struct{ Reason string } // threshold | mid-turn | overflow
type RetryInfo struct {
	Attempt int
	Delay   time.Duration
	Err     error
}
type TerminatedInfo struct{ ToolName string }

// AgentEvent 增加字段
//   Compaction *CompactionInfo
//   Retry      *RetryInfo
//   Terminated *TerminatedInfo
```

- [ ] **Step 4: 实现 agent.go**

```go
package agent

import (
	"context"
	"sync"
	"time"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
	"einoclaw-build/internal/tool"
)

// Context 是循环的真相源：输入重建、消息记录、压缩与溢出恢复。
type Context interface {
	Build(ctx context.Context) ([]message.Message, error)
	Record(m message.Message, u model.Usage) error
	ShouldCompact(u model.Usage) bool
	Compact(ctx context.Context) (bool, error)
	RecoverOverflow(ctx context.Context) (bool, error)
}

type Agent struct {
	name          string
	model         model.Model
	tools         *tool.Registry
	executor      *tool.Executor
	cc            Context
	maxIterations int
	maxRetries    int
	retryBase     time.Duration
}

func New(name string, m model.Model, tools *tool.Registry, exec *tool.Executor, cc Context) *Agent {
	return &Agent{name: name, model: m, tools: tools, executor: exec, cc: cc, maxIterations: 50, maxRetries: 3, retryBase: 500 * time.Millisecond}
}

func (a *Agent) SetMaxIterations(n int) {
	if n > 0 {
		a.maxIterations = n
	}
}

// MemoryContext 纯内存 Context（测试 / 无会话场景）。
type MemoryContext struct {
	mu        sync.Mutex
	system    []message.Message
	msgs      []message.Message
	compactAt int  // >0 时 PromptTokens 超过即 ShouldCompact
	recoverOK bool // RecoverOverflow 是否成功
	compacts  int
	recovers  int
}

func NewMemoryContext(system []message.Message) *MemoryContext { return &MemoryContext{system: system} }

func (c *MemoryContext) Build(context.Context) ([]message.Message, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := append([]message.Message{}, c.system...)
	return append(out, c.msgs...), nil
}
func (c *MemoryContext) Record(m message.Message, _ model.Usage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, m)
	return nil
}
func (c *MemoryContext) ShouldCompact(u model.Usage) bool { return c.compactAt > 0 && u.PromptTokens > c.compactAt }
func (c *MemoryContext) Compact(context.Context) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.compacts++
	if len(c.msgs) > 2 {
		c.msgs = append([]message.Message{message.NewUserMessage("[summary]")}, c.msgs[len(c.msgs)-1:]...)
	}
	return true, nil
}
func (c *MemoryContext) RecoverOverflow(context.Context) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recovers++
	if c.recoverOK && len(c.msgs) > 0 {
		c.msgs = c.msgs[len(c.msgs)-1:]
	}
	return c.recoverOK, nil
}
```

（`MemoryContext.Compact` 直接丢弃会拆 tool 配对，但它只用于测试；生产用 `context.Manager`。）

- [ ] **Step 5: 实现 loop.go**

```go
func (a *Agent) Run(ctx context.Context, steer <-chan message.Message) <-chan AgentEvent {
	ch := make(chan AgentEvent, 16)
	go func() {
		defer close(ch)
		emit := func(e AgentEvent) {
			select {
			case ch <- e:
			case <-time.After(time.Second):
			}
		}
		emit(AgentEvent{Type: EventAgentStart})
		emit(AgentEvent{Type: EventTurnStart})
		a.loop(ctx, steer, emit)
		emit(AgentEvent{Type: EventTurnEnd})
		emit(AgentEvent{Type: EventAgentEnd})
	}()
	return ch
}

func (a *Agent) loop(ctx context.Context, steer <-chan message.Message, emit func(AgentEvent)) {
	var lastUsage model.Usage
	retries := 0
	for step := 0; step < a.maxIterations; step++ {
		if steer != nil {
			select {
			case sm := <-steer:
				_ = a.cc.Record(sm, model.Usage{})
			default:
			}
		}
		if lastUsage.PromptTokens > 0 && a.cc.ShouldCompact(lastUsage) {
			if did, err := a.cc.Compact(ctx); err == nil && did {
				emit(AgentEvent{Type: EventCompaction, Compaction: &CompactionInfo{Reason: "mid-turn"}})
				lastUsage = model.Usage{}
			}
		}
		msgs, err := a.cc.Build(ctx)
		if err != nil {
			emit(AgentEvent{Type: EventError, Err: err})
			return
		}
		stream, err := a.model.Stream(ctx, msgs, a.tools.Specs())
		if err != nil {
			if a.handleModelError(ctx, err, &retries, emit) {
				step-- // 恢复/重试不计步
				continue
			}
			return
		}
		assistant, usage, streamErr := consumeStream(ctx, stream, emit)
		stream.Close()
		if streamErr != nil {
			if a.handleModelError(ctx, streamErr, &retries, emit) {
				step--
				continue
			}
			return
		}
		retries = 0
		lastUsage = usage
		if err := a.cc.Record(assistant, usage); err != nil {
			emit(AgentEvent{Type: EventError, Err: err})
			return
		}
		calls := toolCallsOf(assistant)
		if len(calls) == 0 || ctx.Err() != nil {
			return
		}
		for _, tc := range calls {
			emit(AgentEvent{Type: EventToolStart, ToolStart: &ToolStart{ID: tc.ID, Name: tc.Name, Args: tc.Args}})
		}
		results := a.executor.ExecuteAll(ctx, calls)
		terminated := ""
		for i, tc := range calls {
			emit(AgentEvent{Type: EventToolEnd, ToolEnd: &ToolEnd{ID: tc.ID, Name: tc.Name, Content: results[i].Content, IsError: results[i].IsError}})
			_ = a.cc.Record(message.NewToolMessage(tc.ID, tc.Name, results[i].Content, results[i].IsError), model.Usage{})
			if t, ok := a.tools.Get(tc.Name); ok && !results[i].IsError {
				if term, ok := t.(tool.Terminal); ok && term.IsTerminal() {
					terminated = tc.Name
				}
			}
		}
		if terminated != "" {
			emit(AgentEvent{Type: EventTerminated, Terminated: &TerminatedInfo{ToolName: terminated}})
			return
		}
	}
}

// handleModelError 分流：溢出 → 压缩恢复；瞬时 → 退避重试；其它 → EventError。返回 true 表示应继续循环。
func (a *Agent) handleModelError(ctx context.Context, err error, retries *int, emit func(AgentEvent)) bool {
	if ctx.Err() != nil {
		return false
	}
	if model.IsContextOverflow(err) {
		did, cerr := a.cc.RecoverOverflow(ctx)
		if cerr == nil && did {
			emit(AgentEvent{Type: EventCompaction, Compaction: &CompactionInfo{Reason: "overflow"}})
			return true
		}
		emit(AgentEvent{Type: EventError, Err: err})
		return false
	}
	if model.IsRetryable(err) && *retries < a.maxRetries {
		*retries++
		delay := a.retryBase * time.Duration(1<<(*retries-1))
		emit(AgentEvent{Type: EventRetry, Retry: &RetryInfo{Attempt: *retries, Delay: delay, Err: err}})
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return false
		}
		return true
	}
	emit(AgentEvent{Type: EventError, Err: err})
	return false
}
```

`consumeStream` 的 `eventStream` 参数类型改为 `model.ModelStream`；流错误路径不再 emit `EventError`（由 `handleModelError` 统一发），但仍 emit `EventMessageEnd`。`ExecuteAll` 返回 `[]tool.Result{Content string; IsError bool}`（Task 9 一并改 Executor）；`ToolEnd` 增加 `IsError bool`。`lastUserText`/`renderMemories` 从 loop.go 移到 `cmd/agent`（记忆注入改由 system func 负责，见 Task 12）。

- [ ] **Step 6: 运行测试通过**

Run: `env -u GOROOT go test ./internal/agent/ -v`
Expected: PASS（Executor 签名在 Task 9 改，先在本任务内把 `Executor.Execute` 返回 `Result` 一起改掉以保证编译）

- [ ] **Step 7: 提交**

```bash
git add internal/agent internal/tool && git commit -m "feat(agent): 循环 v2——Context 真相源、终止型工具、溢出恢复与重试双通道、mid-turn 压缩"
```

---

### Task 8: 产物存储 —— ArtifactStore、Sink 接线、`artifact://` 读回

**Files:**
- Create: `internal/runtime/artifact.go`、`internal/runtime/artifact_test.go`
- Modify: `internal/runtime/sink.go`、`internal/runtime/sink_test.go`

**Interfaces:**
- Produces: `runtime.NewArtifactStore(dir string) *ArtifactStore`、`(*ArtifactStore).Create(tool string) (id string, f *os.File, err error)`、`(*ArtifactStore).Resolve(ref string) (string, error)`、`(*ArtifactStore).Dir() string`；`(*Sink).SetArtifactStore(store *ArtifactStore, tool string)`；常量 `ArtifactScheme = "artifact://"`

- [ ] **Step 1: 写失败测试**

```go
package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArtifactStoreAllocatesAfterExisting(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "7.bash.log"), []byte("x"), 0o644)
	s := NewArtifactStore(dir)
	id, f, err := s.Create("grep")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if id != "8" || filepath.Base(f.Name()) != "8.grep.log" {
		t.Fatalf("id=%s name=%s", id, f.Name())
	}
	p, err := s.Resolve("artifact://8")
	if err != nil || !strings.HasSuffix(p, "8.grep.log") {
		t.Fatalf("resolve = %q err %v", p, err)
	}
	if _, err := s.Resolve("artifact://99"); err == nil {
		t.Fatal("missing artifact should error")
	}
}

func TestSinkSpillsToArtifactStore(t *testing.T) {
	s := NewArtifactStore(t.TempDir())
	sink := NewSink(10, 10)
	sink.SetArtifactStore(s, "bash")
	sink.Write([]byte(strings.Repeat("a", 50)))
	sink.Write([]byte(strings.Repeat("b", 50)))
	out := sink.Result()
	sink.Close()
	if !strings.Contains(out, "artifact://0") {
		t.Fatalf("result = %q", out)
	}
	p, _ := s.Resolve("0")
	b, _ := os.ReadFile(p)
	if len(b) != 100 {
		t.Fatalf("artifact bytes = %d, want 100", len(b))
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `env -u GOROOT go test ./internal/runtime/ -run 'Artifact|Spills' -v`
Expected: FAIL

- [ ] **Step 3: 实现 artifact.go**

```go
package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

const ArtifactScheme = "artifact://"

var artifactName = regexp.MustCompile(`^(\d+)\.[A-Za-z0-9_-]+\.log$`)

// ArtifactStore 管理一个会话的产物目录：<dir>/<id>.<tool>.log，id 单调递增。
type ArtifactStore struct {
	dir  string
	mu   sync.Mutex
	next int64
	init bool
}

func NewArtifactStore(dir string) *ArtifactStore { return &ArtifactStore{dir: dir} }

func (s *ArtifactStore) Dir() string { return s.dir }

func (s *ArtifactStore) scanLocked() error {
	if s.init {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	des, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, de := range des {
		if m := artifactName.FindStringSubmatch(de.Name()); m != nil {
			if n, err := strconv.ParseInt(m[1], 10, 64); err == nil && n >= s.next {
				s.next = n + 1
			}
		}
	}
	s.init = true
	return nil
}

// Create 分配一个新产物文件。
func (s *ArtifactStore) Create(tool string) (string, *os.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.scanLocked(); err != nil {
		return "", nil, err
	}
	id := strconv.FormatInt(s.next, 10)
	s.next++
	name := sanitizeTool(tool)
	f, err := os.Create(filepath.Join(s.dir, id+"."+name+".log"))
	if err != nil {
		return "", nil, err
	}
	return id, f, nil
}

// Resolve 把 "artifact://N" 或 "N" 解析为文件路径。
func (s *ArtifactStore) Resolve(ref string) (string, error) {
	id := strings.TrimPrefix(strings.TrimSpace(ref), ArtifactScheme)
	if _, err := strconv.Atoi(id); err != nil {
		return "", fmt.Errorf("artifact id 必须是数字，got %q", ref)
	}
	matches, _ := filepath.Glob(filepath.Join(s.dir, id+".*.log"))
	if len(matches) == 0 {
		return "", fmt.Errorf("artifact %s 不存在", id)
	}
	return matches[0], nil
}

func sanitizeTool(t string) string {
	var b strings.Builder
	for _, r := range t {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "tool"
	}
	return b.String()
}
```

- [ ] **Step 4: 改 sink.go**

把 `artifactDir string` 字段换成 `store *ArtifactStore; tool string`；`SetArtifactStore(store, tool)`；`openArtifactLocked` 改为 `id, f, err := s.store.Create(s.tool)`；`Result()` 尾部改为：

```go
if s.artifact != nil {
	fmt.Fprintf(&sb, "\n[完整输出已保存: %s%s ；用 read_file 的 file_path=\"%s%s\" 读取]", ArtifactScheme, s.artifactID, ArtifactScheme, s.artifactID)
}
```

删除 `SetArtifactDir` 与全局 `sinkCounter`。更新 `sink_test.go` 里使用 `SetArtifactDir` 的测试为 `SetArtifactStore(NewArtifactStore(dir), "t")`。

- [ ] **Step 5: 运行测试通过**

Run: `env -u GOROOT go test ./internal/runtime/ -v`
Expected: PASS

- [ ] **Step 6: 提交**

```bash
git add internal/runtime && git commit -m "feat(runtime): ArtifactStore（会话产物目录、id 扫描分配）与 Sink 接线"
```

---

### Task 9: 工具层 —— Executor 产物注入与 Result、`artifact://` 读取、Bash cwd、MCP tier

**Files:**
- Modify: `internal/tool/executor.go`、`internal/tool/tools.go`、`internal/tool/mcp.go`、`internal/runtime/bash.go`
- Test: `internal/tool/executor_test.go`（更新）、`internal/runtime/bash_test.go`（更新）

**Interfaces:**
- Produces: `type Result struct{ Content string; IsError bool }`；`NewExecutor(r *Registry, mode permission.Mode, approver Approver) *Executor`；`(*Executor).SetArtifactStore(*runtime.ArtifactStore)`；`Execute(ctx, call) Result`；`ExecuteAll(ctx, calls) []Result`；`Builtins(bash *runtime.Bash, store *runtime.ArtifactStore) []Tool`；`runtime.NewBash(cwd string) *Bash`（空 cwd → `os.Getwd()`）；`(*Bash).CWD() string`

- [ ] **Step 1: 写失败测试**

```go
// internal/tool/executor_test.go 追加
func TestExecuteReadsArtifactBack(t *testing.T) {
	store := runtime.NewArtifactStore(t.TempDir())
	reg := NewRegistry()
	for _, tl := range Builtins(runtime.NewBash(t.TempDir()), store) {
		reg.Register(tl)
	}
	ex := NewExecutor(reg, permission.ModeYolo, nil)
	ex.SetArtifactStore(store)
	big := strings.Repeat("line\n", 3000) // > head+tail
	r := ex.Execute(context.Background(), message.ToolCall{ID: "1", Name: "bash", Args: `{"command":"printf '` + big + `'"}`})
	if r.IsError || !strings.Contains(r.Content, "artifact://0") {
		t.Fatalf("result = %+v", r)
	}
	rr := ex.Execute(context.Background(), message.ToolCall{ID: "2", Name: "read_file", Args: `{"file_path":"artifact://0","limit":2}`})
	if rr.IsError || !strings.HasPrefix(rr.Content, "line\nline") {
		t.Fatalf("read back = %+v", rr)
	}
}

func TestDeniedApprovalIsError(t *testing.T) {
	reg := NewRegistry()
	reg.Register(bashTool{bash: runtime.NewBash("")})
	ex := NewExecutor(reg, permission.ModeAlwaysAsk, nil)
	r := ex.Execute(context.Background(), message.ToolCall{ID: "1", Name: "bash", Args: `{"command":"true"}`})
	if !r.IsError || !strings.Contains(r.Content, "approval") {
		t.Fatalf("result = %+v", r)
	}
}
```

```go
// internal/runtime/bash_test.go 追加
func TestBashDefaultCwdAndRelativeCd(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	b := NewBash(dir)
	sink := NewSink(4000, 4000)
	if err := b.Execute(context.Background(), "cd sub && pwd", sink); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(strings.TrimSpace(sink.Result()), "sub") || b.CWD() != filepath.Join(dir, "sub") {
		t.Fatalf("cwd = %q out = %q", b.CWD(), sink.Result())
	}
	if NewBash("").CWD() == "" {
		t.Fatal("empty cwd should default to os.Getwd")
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `env -u GOROOT go test ./internal/tool/ ./internal/runtime/ -v`
Expected: FAIL

- [ ] **Step 3: 实现**

`executor.go`：

```go
type Result struct {
	Content string
	IsError bool
}

type Executor struct {
	registry *Registry
	mode     permission.Mode
	approver Approver
	sem      chan struct{}
	store    *runtime.ArtifactStore
}

func (e *Executor) SetArtifactStore(s *runtime.ArtifactStore) { e.store = s }

func (e *Executor) Execute(ctx context.Context, call message.ToolCall) Result {
	t, ok := e.registry.Get(call.Name)
	if !ok {
		return Result{Content: "tool not found: " + call.Name, IsError: true}
	}
	if permission.Resolve(t.Tier(), e.mode) == permission.DecisionPrompt {
		if e.approver == nil {
			return Result{Content: "tool denied: requires approval (tier=" + string(t.Tier()) + ", no approver)", IsError: true}
		}
		approved, err := e.approver.Approve(ctx, call)
		if err != nil {
			return Result{Content: "tool approval interrupted: " + err.Error(), IsError: true}
		}
		if !approved {
			reason := "tool denied by user"
			if r, ok := e.approver.(interface{ DenyReason() string }); ok && r.DenyReason() != "" {
				reason = r.DenyReason()
			}
			return Result{Content: reason, IsError: true}
		}
	}
	var args map[string]any
	if call.Args != "" {
		_ = json.Unmarshal([]byte(call.Args), &args)
	}
	sink := runtime.NewSink(sinkHeadLimit, sinkTailLimit)
	if e.store != nil {
		sink.SetArtifactStore(e.store, call.Name)
	}
	defer sink.Close()
	err := t.Execute(ctx, args, sink)
	res := sink.Result()
	if err != nil {
		return Result{Content: res + "\n[tool error: " + err.Error() + "]", IsError: true}
	}
	return Result{Content: res}
}
```

`ExecuteAll` 返回 `[]Result`，其余逻辑不变（acquire 失败返回 `Result{IsError: true}`）。

`tools.go`：`Builtins(bash *runtime.Bash, store *runtime.ArtifactStore)`；`readFileTool{store *runtime.ArtifactStore}`，`Execute` 开头：

```go
if strings.HasPrefix(path, runtime.ArtifactScheme) {
	if t.store == nil {
		return fmt.Errorf("本会话没有产物目录")
	}
	p, err := t.store.Resolve(path)
	if err != nil {
		return err
	}
	path = p
}
```

并把 `offset/limit` 改为按**行**（1-based offset，limit 行数），UTF-8 安全：`lines := strings.Split(text, "\n")`。

`mcp.go`：`Tier()` 返回 `permission.TierWrite`；`inputSchemaToMap` 返回 `(props map[string]any, required []string)`；`mcpTool` 新增 `required []string` 与 `Required()` — 由于 `Tool` 接口无 `Required`，在 `Registry.Specs()` 里用可选接口 `interface{ Required() []string }` 取值填入 `ToolSpec.Required`（同样给 `task` 工具用）。

`bash.go`：

```go
func NewBash(cwd string) *Bash {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}
	return &Bash{cwd: cwd}
}
func (b *Bash) CWD() string { b.mu.Lock(); defer b.mu.Unlock(); return b.cwd }
// Execute 里 parseCd 后：if !filepath.IsAbs(newCwd) { newCwd = filepath.Join(b.cwd, newCwd) }
```

- [ ] **Step 4: 运行测试通过**

Run: `env -u GOROOT go test ./internal/tool/ ./internal/runtime/ ./internal/agent/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/tool internal/runtime && git commit -m "feat(tool): Executor 产物注入与 Result，read_file 支持 artifact:// 与按行读取，Bash 默认 cwd，MCP 默认 write"
```

---

### Task 10: 子 agent 运行时修正

**Files:**
- Modify: `internal/subagent/spec.go`、`manager.go`、`yield.go`、`task.go`
- Create: `internal/subagent/approver.go`
- Test: `internal/subagent/manager_test.go`（重写）

**Interfaces:**
- Consumes: `agent.New(name, model, tools, exec, cc)`、`context.New(...)`、`session.NewWithHeader/AppendInit`、`runtime.NewBash/NewArtifactStore`、`tool.NewExecutor/SetArtifactStore`
- Produces: `type Options struct{...}`（spec §9）；`NewManager(o Options) *Manager`；`Status` 增加 `StatusTimeout`、`StatusAborted`；`Task{Name, Subagent, Prompt}`；`Result{ID, Name, Status, Yielded bool, Data, Text, Err, Usage model.Usage, Requests int, DurationMs int64, SessionFile string}`；`StatusString(Status) string`；`denyApprover{}`（`Approve → false, nil`；`DenyReason()`）；`labeledApprover{inner tool.Approver; label string}`

- [ ] **Step 1: 写失败测试**

```go
package subagent

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
	"einoclaw-build/internal/tool"
)

type fakeStream struct {
	events []model.ModelEvent
	i      int
	delay  time.Duration
}

func (f *fakeStream) Recv() (model.ModelEvent, error) {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.i < len(f.events) {
		ev := f.events[f.i]
		f.i++
		return ev, nil
	}
	return model.ModelEvent{}, io.EOF
}
func (f *fakeStream) Usage() model.Usage { return model.Usage{PromptTokens: 10, CompletionTokens: 5} }
func (f *fakeStream) Close()             {}

type scriptModel struct {
	steps []model.ModelEvent
	delay time.Duration
}

func (m *scriptModel) Stream(ctx context.Context, _ []message.Message, _ []model.ToolSpec) (model.ModelStream, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if len(m.steps) == 0 {
		return &fakeStream{events: []model.ModelEvent{{Text: "idle"}}, delay: m.delay}, nil
	}
	s := m.steps[0]
	m.steps = m.steps[1:]
	return &fakeStream{events: []model.ModelEvent{s}, delay: m.delay}, nil
}

func call(id, name, args string) model.ModelEvent {
	return model.ModelEvent{ToolCalls: []model.ToolCallDelta{{Index: 0, CallID: id, Name: name, Args: args}}}
}

func workerTools(cwd string, store *runtime.ArtifactStore) *tool.Registry {
	reg := tool.NewRegistry()
	for _, t := range tool.Builtins(runtime.NewBash(cwd), store) {
		reg.Register(t)
	}
	return reg
}

func baseOpts(m model.Model, dir string) Options {
	return Options{Model: m, WorkerTools: workerTools, Mode: permission.ModeYolo, SessionDir: dir, CWD: dir, MaxConcurrency: 2,
		Defs: []SubagentSpec{{Name: "explorer", SystemPrompt: "x", MaxTurns: 10}}, ContextWindow: 100000}
}

func TestYieldTerminatesAndExtractsData(t *testing.T) {
	m := &scriptModel{steps: []model.ModelEvent{
		call("c1", "yield", `{"data":{"files":["a.go"]}}`),
		{Text: "SHOULD NOT RUN"},
	}}
	dir := t.TempDir()
	mgr := NewManager(baseOpts(m, dir))
	r := mgr.Run(context.Background(), Task{Subagent: "explorer", Prompt: "look"})
	if r.Status != StatusCompleted || !r.Yielded || r.Data["files"] == nil || r.Requests != 1 {
		t.Fatalf("result = %+v", r)
	}
	if r.SessionFile == "" || !strings.HasPrefix(filepath.Base(r.SessionFile), "agent-explorer") {
		t.Fatalf("session file = %q", r.SessionFile)
	}
	b, _ := os.ReadFile(r.SessionFile)
	if !strings.Contains(string(b), `"type":"session_init"`) {
		t.Fatalf("sidecar lacks session_init: %s", b)
	}
}

func TestTimeoutIsReported(t *testing.T) {
	m := &scriptModel{delay: 300 * time.Millisecond, steps: []model.ModelEvent{call("c1", "bash", `{"command":"sleep 2"}`)}}
	o := baseOpts(m, t.TempDir())
	o.Defs[0].Timeout = 150 * time.Millisecond
	r := NewManager(o).Run(context.Background(), Task{Subagent: "explorer", Prompt: "slow"})
	if r.Status != StatusTimeout {
		t.Fatalf("status = %s, want timeout", StatusString(r.Status))
	}
}

func TestParentCancelIsAborted(t *testing.T) {
	m := &scriptModel{delay: 200 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	r := NewManager(baseOpts(m, t.TempDir())).Run(ctx, Task{Subagent: "explorer", Prompt: "x"})
	if r.Status != StatusAborted {
		t.Fatalf("status = %s, want aborted", StatusString(r.Status))
	}
}

func TestHeadlessDeniesPromptByDefault(t *testing.T) {
	m := &scriptModel{steps: []model.ModelEvent{call("c1", "bash", `{"command":"echo hi"}`), {Text: "end"}}}
	o := baseOpts(m, t.TempDir())
	o.Mode = permission.ModeAlwaysAsk
	r := NewManager(o).Run(context.Background(), Task{Subagent: "explorer", Prompt: "x"})
	b, _ := os.ReadFile(r.SessionFile)
	if !strings.Contains(string(b), "headless subagent cannot prompt") {
		t.Fatalf("bash should be denied, transcript: %s", b)
	}
}

type recordingApprover struct{ got []message.ToolCall }

func (r *recordingApprover) Approve(_ context.Context, c message.ToolCall) (bool, error) {
	r.got = append(r.got, c)
	return true, nil
}

func TestEscalationLabelsCall(t *testing.T) {
	m := &scriptModel{steps: []model.ModelEvent{call("c1", "bash", `{"command":"echo hi"}`), {Text: "end"}}}
	o := baseOpts(m, t.TempDir())
	o.Mode = permission.ModeAlwaysAsk
	ap := &recordingApprover{}
	o.Approver, o.Escalate = ap, true
	NewManager(o).Run(context.Background(), Task{Name: "Scout", Subagent: "explorer", Prompt: "x"})
	if len(ap.got) != 1 || !strings.Contains(ap.got[0].Name, "[子 agent Scout]") {
		t.Fatalf("approver got %+v", ap.got)
	}
}

func TestSchemaRequiredButNoData(t *testing.T) {
	m := &scriptModel{steps: []model.ModelEvent{{Text: "just text"}}}
	o := baseOpts(m, t.TempDir())
	o.Defs[0].OutputSchema = map[string]any{"type": "object"}
	r := NewManager(o).Run(context.Background(), Task{Subagent: "explorer", Prompt: "x"})
	if r.Status != StatusFailed || r.Err == nil {
		t.Fatalf("result = %+v", r)
	}
}

func TestRunManyOrderAndUnknown(t *testing.T) {
	mgr := NewManager(baseOpts(&scriptModel{}, t.TempDir()))
	rs := mgr.RunMany(context.Background(), []Task{{Subagent: "nope", Prompt: "x"}, {Subagent: "explorer", Prompt: "y"}})
	if len(rs) != 2 || rs[0].Status != StatusFailed || rs[1].Status != StatusCompleted || rs[1].Yielded {
		t.Fatalf("results = %+v", rs)
	}
	if rs[1].Name != "explorer-2" {
		t.Fatalf("default name = %q", rs[1].Name)
	}
}

var _ = errors.New
```

- [ ] **Step 2: 运行确认失败**

Run: `env -u GOROOT go test ./internal/subagent/ -v`
Expected: FAIL

- [ ] **Step 3: 实现 spec.go 增补**

```go
const (
	StatusPending Status = iota
	StatusRunning
	StatusCompleted
	StatusFailed
	StatusKilled
	StatusTimeout
	StatusAborted
)

func StatusString(s Status) string { /* pending running completed failed killed timeout aborted */ }

type Task struct {
	Name     string // 稳定名；缺省 <subagent>-<序号>
	Subagent string
	Prompt   string
}

type Result struct {
	ID, Name    string
	Status      Status
	Yielded     bool
	Data        map[string]any
	Text        string
	Err         error
	Usage       model.Usage
	Requests    int
	DurationMs  int64
	SessionFile string
}
```

- [ ] **Step 4: 实现 approver.go**

```go
package subagent

import (
	"context"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/tool"
)

// denyApprover：headless 子 agent 的默认策略——需要审批的调用一律拒绝并说明。
type denyApprover struct{}

func (denyApprover) Approve(context.Context, message.ToolCall) (bool, error) { return false, nil }
func (denyApprover) DenyReason() string {
	return "tool denied: headless subagent cannot prompt for approval (enable subagent.approval_escalation to ask the user)"
}

// labeledApprover：升级到父审批器，调用名带子 agent 标签供弹窗显示。
type labeledApprover struct {
	inner tool.Approver
	label string
}

func (l labeledApprover) Approve(ctx context.Context, c message.ToolCall) (bool, error) {
	c.Name = l.label + " " + c.Name
	return l.inner.Approve(ctx, c)
}
```

- [ ] **Step 5: 实现 manager.go**

```go
type Options struct {
	Model          model.Model
	WorkerTools    func(cwd string, store *runtime.ArtifactStore) *tool.Registry
	Memory         memory.Recaller
	Mode           permission.Mode
	Approver       tool.Approver
	Escalate       bool
	SessionDir     string
	CWD            string
	MaxConcurrency int
	Defs           []SubagentSpec
	Summarizer     summarizer
	ContextWindow  int
}

type summarizer interface {
	Summarize(ctx context.Context, msgs []message.Message) (string, error)
}

type Manager struct {
	o   Options
	sem chan struct{}
	seq atomic.Int64
}

func NewManager(o Options) *Manager {
	if o.MaxConcurrency <= 0 {
		o.MaxConcurrency = 4
	}
	if o.ContextWindow <= 0 {
		o.ContextWindow = 128000
	}
	return &Manager{o: o, sem: make(chan struct{}, o.MaxConcurrency)}
}

func (m *Manager) List() []SubagentSpec { return m.o.Defs }

func (m *Manager) Run(ctx context.Context, t Task) Result {
	n := m.seq.Add(1)
	name := t.Name
	if name == "" {
		name = fmt.Sprintf("%s-%d", t.Subagent, n)
	}
	start := time.Now()
	def, ok := m.find(t.Subagent)
	if !ok {
		return Result{ID: t.Subagent, Name: name, Status: StatusFailed, Err: fmt.Errorf("unknown subagent %q", t.Subagent)}
	}

	// 运行时：独立 bash / 产物 / 工具集 / 审批
	store := runtime.NewArtifactStore(m.o.SessionDir)
	var tools *tool.Registry
	if m.o.WorkerTools != nil {
		tools = m.o.WorkerTools(m.o.CWD, store)
	} else {
		tools = tool.NewRegistry()
	}
	tools = tools.Without("task")
	tools.Register(NewYieldTool())
	var approver tool.Approver = denyApprover{}
	if m.o.Escalate && m.o.Approver != nil {
		approver = labeledApprover{inner: m.o.Approver, label: "[子 agent " + name + "]"}
	}
	exec := tool.NewExecutor(tools, m.o.Mode, approver)
	if m.o.SessionDir != "" {
		exec.SetArtifactStore(store)
	}

	// sidecar 会话
	sess, file, err := m.openSidecar(name, def, t)
	if err != nil {
		return Result{ID: t.Subagent, Name: name, Status: StatusFailed, Err: err}
	}
	defer sess.Close()

	system := func(context.Context) []message.Message {
		return []message.Message{message.NewSystemMessage(def.SystemPrompt)}
	}
	cc := agentctx.New(sess, m.o.Summarizer, m.o.ContextWindow, 16384, system)
	_ = cc.Record(message.NewUserMessage(t.Prompt), model.Usage{})

	sub := agent.New(def.Name, m.o.Model, tools, exec, cc)
	sub.SetMaxIterations(def.MaxTurns)

	runCtx, cancel := ctx, context.CancelFunc(func() {})
	if def.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, def.Timeout)
	}
	defer cancel()

	res := Result{ID: t.Subagent, Name: name, SessionFile: file}
	var runErr error
	for ev := range sub.Run(runCtx, nil) {
		switch ev.Type {
		case agent.EventMessageEnd:
			res.Requests++
			res.Usage = addUsage(res.Usage, ev.Ended.Usage)
			if txt := textOf(ev.Ended.Message); txt != "" {
				res.Text = txt
			}
		case agent.EventToolEnd:
			if ev.ToolEnd.Name == "yield" && !ev.ToolEnd.IsError {
				res.Yielded = true
			}
		case agent.EventToolStart:
			if ev.ToolStart.Name == "yield" {
				var args map[string]any
				if json.Unmarshal([]byte(ev.ToolStart.Args), &args) == nil {
					if d, ok := args["data"].(map[string]any); ok {
						res.Data = d
					}
				}
			}
		case agent.EventError:
			runErr = ev.Err
		}
	}
	res.DurationMs = time.Since(start).Milliseconds()

	switch {
	case ctx.Err() != nil:
		res.Status, res.Err = StatusAborted, ctx.Err()
	case runCtx.Err() != nil:
		res.Status, res.Err = StatusTimeout, fmt.Errorf("子 agent 超时（%s）", def.Timeout)
	case runErr != nil:
		res.Status, res.Err = StatusFailed, runErr
	case def.OutputSchema != nil && res.Data == nil:
		res.Status, res.Err = StatusFailed, errors.New("子 agent 未通过 yield 产出符合 schema 的结构化数据")
	default:
		res.Status = StatusCompleted
	}
	_ = sess.AppendCustom("session_exit", map[string]any{"status": StatusString(res.Status), "requests": res.Requests})
	return res
}

func (m *Manager) openSidecar(name string, def SubagentSpec, t Task) (*session.Session, string, error) {
	var st session.Storage
	file := ""
	if m.o.SessionDir != "" {
		if err := os.MkdirAll(m.o.SessionDir, 0o755); err != nil {
			return nil, "", err
		}
		file = filepath.Join(m.o.SessionDir, "agent-"+name+".jsonl")
		fs, err := session.NewFileStorage(file)
		if err != nil {
			return nil, "", err
		}
		st = fs
	} else {
		st = &session.MemoryStorage{}
	}
	sess, err := session.NewWithHeader(session.Header{ID: "agent-" + name, CWD: m.o.CWD, ParentSession: filepath.Base(m.o.SessionDir)}, st)
	if err != nil {
		return nil, "", err
	}
	_ = sess.AppendInit(session.SessionInit{Agent: def.Name, SystemPrompt: def.SystemPrompt, Task: t.Prompt, OutputSchema: def.OutputSchema, Depth: 1})
	return sess, file, nil
}

func addUsage(a, b model.Usage) model.Usage {
	a.PromptTokens += b.PromptTokens
	a.CompletionTokens += b.CompletionTokens
	a.TotalTokens += b.TotalTokens
	a.ReasoningTokens += b.ReasoningTokens
	a.CachedTokens += b.CachedTokens
	return a
}

func (m *Manager) RunMany(ctx context.Context, tasks []Task) []Result { /* 同原逻辑，Task 传整个 t */ }
```

`yield.go`：加 `func (yieldTool) IsTerminal() bool { return true }`，`Execute` 写 "result submitted"。

`task.go`：参数 schema 增加 `name`（string）并标 `required: ["subagent","prompt"]`（通过 `Required()` 可选接口返回 `[]string{"tasks"}`）；输出格式：

```go
fmt.Fprintf(&sb, "## %s (%s) [%s] requests=%d tokens=%d %dms\n", r.Name, r.ID, StatusString(r.Status), r.Requests, r.Usage.TotalTokens, r.DurationMs)
switch {
case r.Status == StatusCompleted && r.Data != nil: json
case r.Status == StatusCompleted && !r.Yielded: "[未显式 yield，以下为最后输出]\n" + r.Text
case r.Status == StatusCompleted: r.Text
default: "error: " + r.Err.Error() + "\n" + partial(r.Text)
}
if r.SessionFile != "" { fmt.Fprintf(&sb, "\n(transcript: %s)\n", r.SessionFile) }
```

- [ ] **Step 6: 运行测试通过**

Run: `env -u GOROOT go test ./internal/subagent/ -v`
Expected: PASS

- [ ] **Step 7: 提交**

```bash
git add internal/subagent && git commit -m "feat(subagent): yield 终止、timeout/aborted 状态、独立 bash 与 sidecar 会话、父权限继承（默认拒绝/可升级）"
```

---

### Task 11: TUI 接新循环、会话清单、审批标签

**Files:**
- Modify: `internal/tui/tui.go`、`internal/tui/approval.go`

**Interfaces:**
- Consumes: `agent.Run(ctx, steer)`、`context.Manager.Record/SetSession/Session`、`session.Manager.List/Switch/New/Current`
- Produces: `tui.NewModel(ag *agent.Agent, mgr *session.Manager, cmgr *agentctx.Manager, mem *memory.Store, cwd string) teaModel`

- [ ] **Step 1: 改 runAgent**

```go
func (m teaModel) runAgent(ctx context.Context, text string, steer chan message.Message) {
	runMu.Lock()
	defer runMu.Unlock()
	defer func() { currentSteer = nil }()
	_ = m.cmgr.Record(message.NewUserMessage(text), model.Usage{})
	for ev := range m.agent.Run(ctx, steer) {
		if program != nil {
			program.Send(ev)
		}
	}
}
```

- [ ] **Step 2: 渲染新事件**：`handleAgentEvent` 增加

```go
case agent.EventCompaction:
	m = m.finalizeStreaming()
	m.chatLines = append(m.chatLines, lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Render("── 上下文已压缩（"+ev.Compaction.Reason+"）──"))
case agent.EventRetry:
	m.chatLines = append(m.chatLines, renderError(fmt.Errorf("模型错误，%v 后重试（%d/3）：%v", ev.Retry.Delay, ev.Retry.Attempt, ev.Retry.Err)))
case agent.EventTerminated:
	m.chatLines = append(m.chatLines, "  ✓ "+ev.Terminated.ToolName)
```

- [ ] **Step 3: 会话命令**：`/sessions` 显示 `in.ID + "  " + title-or-firstUser + "  " + ModTime.Format("01-02 15:04")`；`/resume <prefix>` 调 `m.mgr.Switch`；`/new` 调 `m.mgr.New(m.cwd)`；切换后 `m.cmgr.SetSession(ns)`。

- [ ] **Step 4: 审批标签**：`approvalRequestMsg` 不变（标签已在 `call.Name` 里），`renderApprovalDialog` 不动。

- [ ] **Step 5: 编译**

Run: `env -u GOROOT go build ./...`
Expected: 仅 `cmd/agent`、`internal/eval` 因签名变化报错（Task 12/13 修）

- [ ] **Step 6: 提交**

```bash
git add internal/tui && git commit -m "feat(tui): 接循环 v2，渲染压缩/重试/终止事件，会话清单带标题"
```

---

### Task 12: 装配与 headless `-p` 模式

**Files:**
- Modify: `cmd/agent/main.go`
- Create: `cmd/agent/headless.go`

**Interfaces:**
- Consumes: 以上全部
- Produces: 命令行 `codeclaw [--cwd DIR] [--yolo] [-p PROMPT]`

- [ ] **Step 1: 重写 main.go**

```go
func main() {
	cwdFlag := flag.String("cwd", "", "工作目录（默认当前目录）")
	yolo := flag.Bool("yolo", false, "强制 approval_mode=yolo")
	prompt := flag.String("p", "", "headless：执行一个提示词后退出")
	flag.Parse()

	cwd := *cwdFlag
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	cwd, _ = paths.Canonical(cwd)
	cfg := loadConfig(cwd)
	if *yolo {
		cfg.ApprovalMode = "yolo"
	}

	m, err := model.New(context.Background(), model.Config{...})
	projectDir, err := paths.ProjectDir(cwd)
	warnLegacyData(cwd, projectDir)

	mem, _ := memory.Open(filepath.Join(projectDir, "memory.db"))

	sessMgr, _ := session.NewManager(projectDir)
	s, _ := sessMgr.Current(cwd)
	artifactDir, _ := sessMgr.ArtifactDir(s)
	store := runtime.NewArtifactStore(artifactDir)

	workerTools := func(cwd string, store *runtime.ArtifactStore) *tool.Registry {
		reg := tool.NewRegistry()
		for _, t := range tool.Builtins(runtime.NewBash(cwd), store) { reg.Register(t) }
		if mem != nil { reg.Register(tool.NewRememberTool(mem)) }
		for _, srv := range cfg.MCPServers { _ = tool.ConnectMCP(context.Background(), reg, srv) }
		return reg
	}

	mode := parseMode(cfg.ApprovalMode)
	var approver tool.Approver
	if *prompt == "" { approver = tui.NewApprover() } else { approver = headlessApprover{} } // headless 主 agent：Prompt → 拒绝并说明

	summ := agentctx.NewModelSummarizer(m)
	mgr := subagent.NewManager(subagent.Options{
		Model: m, WorkerTools: workerTools, Memory: mem, Mode: mode, Approver: approver,
		Escalate: cfg.Subagent.ApprovalEscalation, SessionDir: artifactDir, CWD: cwd,
		MaxConcurrency: cfg.Subagent.MaxConcurrency, Defs: builtinDefs(cfg), Summarizer: summ, ContextWindow: cfg.Models[0].ContextWindow,
	})

	mainRegistry := buildMainRegistry(cfg.DelegationMode, workerTools(cwd, store), mgr, mem)
	exec := tool.NewExecutor(mainRegistry, mode, approver)
	exec.SetArtifactStore(store)

	system := func(ctx context.Context) []message.Message {
		msgs := []message.Message{message.NewSystemMessage(buildInstruction(cfg.DelegationMode) + envBlock(cwd))}
		if mem != nil { if mems, err := mem.Recall(recallQuery(s), 5); err == nil && len(mems) > 0 { msgs = append(msgs, message.NewSystemMessage(renderMemories(mems))) } }
		return msgs
	}
	cmgr := agentctx.New(s, summ, cfg.Models[0].ContextWindow, 16384, system)
	ag := agent.New("codeclaw", m, mainRegistry, exec, cmgr)

	if *prompt != "" { os.Exit(runHeadless(context.Background(), ag, cmgr, *prompt)) }
	program := tea.NewProgram(tui.NewModel(ag, sessMgr, cmgr, mem, cwd))
	...
}
```

`builtinDefs(cfg)` 把原三个定义加上 `Timeout: cfg.Subagent.DefaultTimeout, MaxTurns: cfg.Subagent.DefaultMaxTurns`；`envBlock(cwd)` 输出 `\n\n<env>\ncwd: …\ngit_root: …\ndate: …\nplatform: …\n</env>`；`recallQuery(s)` 取会话最后一条 user 文本（`s.Replay()` 尾部）；`renderMemories`/`lastUserText` 从 agent 包迁入此处；`warnLegacyData` 检测 `./sessions` 或 `./memory.db` 存在时打印一行迁移提示。

- [ ] **Step 2: headless.go**

```go
type headlessApprover struct{}
func (headlessApprover) Approve(context.Context, message.ToolCall) (bool, error) { return false, nil }
func (headlessApprover) DenyReason() string { return "tool denied: headless mode cannot prompt for approval (use --yolo)" }

func runHeadless(ctx context.Context, ag *agent.Agent, cmgr *agentctx.Manager, prompt string) int {
	_ = cmgr.Record(message.NewUserMessage(prompt), model.Usage{})
	var final strings.Builder
	code := 0
	for ev := range ag.Run(ctx, nil) {
		switch ev.Type {
		case agent.EventMessageUpdate:
			if ev.Update.Text != "" { final.WriteString(ev.Update.Text) }
		case agent.EventMessageEnd:
			if final.Len() > 0 { fmt.Println(final.String()); final.Reset() }
		case agent.EventToolStart:
			fmt.Printf("▶ %s %s\n", ev.ToolStart.Name, clip(ev.ToolStart.Args, 200))
		case agent.EventToolEnd:
			fmt.Printf("◀ %s %s\n", ev.ToolEnd.Name, clip(firstLines(ev.ToolEnd.Content, 3), 300))
		case agent.EventCompaction:
			fmt.Printf("[compaction: %s]\n", ev.Compaction.Reason)
		case agent.EventRetry:
			fmt.Printf("[retry %d after %v: %v]\n", ev.Retry.Attempt, ev.Retry.Delay, ev.Retry.Err)
		case agent.EventError:
			fmt.Fprintf(os.Stderr, "error: %v\n", ev.Err)
			code = 1
		}
	}
	return code
}
```

- [ ] **Step 3: 编译 + 手动验证**

Run: `env -u GOROOT go build ./... && env -u GOROOT go vet ./...`
Expected: 通过（`internal/eval` 在 Task 13 修）

- [ ] **Step 4: 提交**

```bash
git add cmd/agent && git commit -m "feat(cmd): 项目作用域装配、headless -p 模式、--yolo/--cwd"
```

---

### Task 13: eval 适配 + 文档

**Files:**
- Modify: `internal/eval/evaluator.go`、`docs/DEVELOPMENT_LOG.md`、`example.yaml`

- [ ] **Step 1: eval**：用 `runtime.NewBash(workdir)` + `runtime.NewArtifactStore(filepath.Join(workdir, ".artifacts"))` + `tool.Builtins(bash, store)`；`session.New(fx.Name, &session.MemoryStorage{})`；`agentctx.New(sess, nil, 128000, 16384, system)`（`summarizer` 为 nil 时 `compact` 直接返回 false）；`cc.Record(user)`；`agent.New(fx.Name, m, reg, exec, cc)`；去掉 `os.Chdir`，工具路径仍相对进程 cwd 的问题留 M4（本次只保证编译与原行为：保留 `os.Chdir`）。
- [ ] **Step 2: DEVELOPMENT_LOG.md** 追加 "P8 地基修正" 小节（本 plan 的产出与验收）；`example.yaml` 增加 `approval_mode: write`、`subagent:` 段。
- [ ] **Step 3: 全量验证**

Run: `env -u GOROOT go build ./... && env -u GOROOT go vet ./... && env -u GOROOT go test ./...`
Expected: 全绿

- [ ] **Step 4: 提交**

```bash
git add internal/eval docs example.yaml && git commit -m "chore: eval 适配循环 v2；记录 P8"
```

---

### Task 14: 启动项目验证（spec §1 验收）

- [ ] **Step 1**：`codeclaw -p "列出当前目录并告诉我有几个 go 文件" --cwd /tmp/some-project` 观察 `▶ bash` / `◀` 输出与最终回复；确认 `~/.codeclaw/projects/` 出现桶。
- [ ] **Step 2**：在另一个目录跑一次，确认两个桶互不包含对方会话；记忆库各自独立。
- [ ] **Step 3**：配置 `context_window: 6000` 后跑一个多文件阅读任务，观察 `[compaction: mid-turn]` 且后续工具继续执行。
- [ ] **Step 4**：`approval_mode: always-ask` + `delegation_mode: always`，让主 agent 派 explorer 跑 bash：transcript 里出现 "headless subagent cannot prompt"；打开 `subagent.approval_escalation: true` 后在 TUI 看到带标签的弹窗。
- [ ] **Step 5**：让工具输出超 8KB（如 `find / -maxdepth 3`），确认 `artifact://N` 与读回。

---

## Self-Review

- **Spec coverage**：§2 → Task 1；§3 → Task 2/12；§4 → Task 3/4；§5 → Task 5；§6 → Task 6；§7 → Task 7；§8 → Task 8/9；§9 → Task 10；§10 → Task 11/12；§11 → Task 12 `warnLegacyData`；§12 测试策略 → 各任务 Step 1。
- **Placeholder scan**：无 TBD；Task 11/12 的代码为节选但函数名、签名与文件位置明确。
- **Type consistency**：`tool.Result{Content, IsError}` 在 Task 7/9/10 一致；`agent.New(name, model, tools, exec, cc)` 在 Task 7/10/12/13 一致；`context.New(s, sum, window, keep, system)` 在 Task 6/10/12/13 一致；`session.NewWithHeader(Header, Storage)` 在 Task 3/4/10 一致；`ToolEnd.IsError` 在 Task 7/10 一致。
