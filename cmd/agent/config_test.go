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
	user := writeYAML(t, dir, "user.yaml", "models:\n  - provider: qwen\n    api_key: k\n    model_id: m\npermissions:\n  deny: [\"read(./.env*)\"]\nbash:\n  timeout: 60s\n")
	proj := writeYAML(t, dir, "proj.yaml", "permissions:\n  allow: [\"bash(go test*)\"]\n  deny: [\"bash(rm -rf *)\"]\n")
	cfg, err := loadConfigFrom([]string{user, proj})
	if err != nil {
		t.Fatal(err)
	}
	// 列表追加：两层 deny 都在（用户 deny 不会被项目顶掉）
	if len(cfg.Permissions.Deny) != 2 || len(cfg.Permissions.Allow) != 1 {
		t.Fatalf("permissions merge = %+v", cfg.Permissions)
	}
	if cfg.Bash.Timeout != 60*time.Second {
		t.Fatalf("bash timeout = %v", cfg.Bash.Timeout)
	}
	rules, errs := cfg.parseRules()
	if len(errs) != 0 || len(rules.Deny) != 2 {
		t.Fatalf("parseRules = %+v errs=%v", rules, errs)
	}
	// 坏条目只告警不致命
	bad := writeYAML(t, dir, "bad.yaml", "permissions:\n  allow: [\"bash(git\"]\n")
	cfg2, err := loadConfigFrom([]string{user, bad})
	if err != nil {
		t.Fatal(err)
	}
	if _, errs := cfg2.parseRules(); len(errs) == 0 {
		t.Fatal("坏规则应报错")
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
