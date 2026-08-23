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
