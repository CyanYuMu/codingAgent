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
	if cfg.Subagent.DefaultMaxTurns != 50 {
		t.Fatalf("max turns default = %d", cfg.Subagent.DefaultMaxTurns)
	}
}

func TestMissingFilesAreSkipped(t *testing.T) {
	_, err := loadConfigFrom([]string{filepath.Join(t.TempDir(), "nope.yaml")})
	if err == nil {
		t.Fatal("no models anywhere should error")
	}
}

func TestPermissionsMergeAppendsAndParseRules(t *testing.T) {
	dir := t.TempDir()
	user := writeYAML(t, dir, "user.yaml", "models:\n  - provider: qwen\n    api_key: k\n    model_id: m\npermissions:\n  allow: [\"bash(go test*)\"]\n  deny: [\"read(./.env*)\"]\nbash:\n  timeout: 60s\n")
	proj := writeYAML(t, dir, "proj.yaml", "permissions:\n  ask: [\"bash(git push*)\"]\n  deny: [\"bash(rm -rf *)\"]\n")
	cfg, err := loadConfigFrom([]string{user, proj})
	if err != nil {
		t.Fatal(err)
	}
	// 列表追加：两层 deny 都在（用户 deny 不会被项目顶掉）
	if len(cfg.Permissions.Deny) != 2 || len(cfg.Permissions.Allow) != 1 || len(cfg.Permissions.Ask) != 1 {
		t.Fatalf("permissions merge = %+v", cfg.Permissions)
	}
	if cfg.Bash.Timeout != 60*time.Second {
		t.Fatalf("bash timeout = %v", cfg.Bash.Timeout)
	}
	rules, errs := cfg.parseRules()
	if len(errs) != 0 || len(rules.Deny) != 2 {
		t.Fatalf("parseRules = %+v errs=%v", rules, errs)
	}
	// 坏规则必须启动失败；忽略一个写坏的 deny 会静默放宽安全边界。
	bad := writeYAML(t, dir, "bad.yaml", "permissions:\n  allow: [\"bash(git\"]\n")
	if _, err := loadConfigFrom([]string{bad}); err == nil {
		t.Fatal("坏规则应使配置加载失败")
	}
}

func TestConfigRejectsUnknownFieldsAndInvalidRanges(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"unknown.yaml": "models:\n  - provider: qwen\n    api_key: k\n    model_id: m\napprovel_mode: yolo\n",
		"mode.yaml":    "models:\n  - provider: qwen\n    api_key: k\n    model_id: m\napproval_mode: unsafe\n",
		"range.yaml":   "models:\n  - provider: qwen\n    api_key: k\n    model_id: m\nmemory:\n  recall_top_k: -1\n",
	} {
		p := writeYAML(t, dir, name, body)
		if _, err := loadConfigFrom([]string{p}); err == nil {
			t.Errorf("%s should fail validation", name)
		}
	}
}

func TestProjectConfigCannotGrantCapabilities(t *testing.T) {
	dir := t.TempDir()
	user := writeYAML(t, dir, "user.yaml", "approval_mode: write\nmodels:\n  - provider: qwen\n    api_key: k\n    model_id: m\n")
	cases := map[string]string{
		"yolo":  "approval_mode: yolo\n",
		"allow": "permissions:\n  allow: [\"bash(*)\"]\n",
		"mcp":   "mcp_servers:\n  - name: bad\n    command: ./bootstrap\n",
		"model": "models:\n  - provider: openai\n    api_key: stolen\n    model_id: proxy\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			project := writeYAML(t, dir, name+".yaml", body)
			if _, err := loadConfigFrom([]string{user, project}); err == nil {
				t.Fatal("repository capability grant should be rejected")
			}
		})
	}
	strict := writeYAML(t, dir, "strict.yaml", "approval_mode: always-ask\npermissions:\n  deny: [\"bash(rm *)\"]\n")
	if cfg, err := loadConfigFrom([]string{user, strict}); err != nil || cfg.ApprovalMode != "always-ask" {
		t.Fatalf("project should be allowed to tighten policy: mode=%s err=%v", cfg.ApprovalMode, err)
	}
}

func TestBashTimeoutDefaults(t *testing.T) {
	dir := t.TempDir()
	p := writeYAML(t, dir, "c.yaml", "models:\n  - provider: qwen\n    api_key: k\n    model_id: m\n")
	cfg, err := loadConfigFrom([]string{p})
	if err != nil || cfg.Bash.Timeout != 120*time.Second {
		t.Fatalf("默认超时应 120s，got %v err=%v", cfg.Bash.Timeout, err)
	}
	p2 := writeYAML(t, dir, "c2.yaml", "models:\n  - provider: qwen\n    api_key: k\n    model_id: m\nbash:\n  timeout: 900s\n")
	cfg2, err := loadConfigFrom([]string{p2})
	if err != nil || cfg2.Bash.Timeout != 600*time.Second {
		t.Fatalf("超时应被钳到 600s，got %v err=%v", cfg2.Bash.Timeout, err)
	}
}

func TestSkillsConfigDefaultsAndFalseOverride(t *testing.T) {
	dir := t.TempDir()
	user := writeYAML(t, dir, "user.yaml", "models:\n  - provider: qwen\n    api_key: k\n    model_id: m\nskills:\n  enabled: true\n  enable_commands: true\n  compatibility:\n    claude: true\n  custom_directories: [~/shared-skills]\n")
	project := writeYAML(t, dir, "project.yaml", "skills:\n  enable_commands: false\n  compatibility:\n    claude: false\n  ignore: [danger-*]\n")
	cfg, err := loadConfigFrom([]string{user, project})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Skills.EnabledValue() || cfg.Skills.CommandsEnabled() || cfg.Skills.ClaudeCompatible() {
		t.Fatalf("skills bool merge failed: %+v", cfg.Skills)
	}
	if cfg.Skills.MaxFileBytes != 256*1024 || cfg.Skills.MaxSkills != 500 {
		t.Fatalf("skills defaults failed: %+v", cfg.Skills)
	}
	if len(cfg.Skills.CustomDirectories) != 1 || len(cfg.Skills.Ignore) != 1 {
		t.Fatalf("skills lists failed: %+v", cfg.Skills)
	}
}
