package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUnavailableUserConfigCannotPromoteProjectCapabilities(t *testing.T) {
	dir := t.TempDir()
	notDir := writeYAML(t, dir, "not-a-directory", "blocked")
	t.Setenv("CODECLAW_HOME", notDir)
	projectDir := filepath.Join(dir, ".codeclaw")
	if err := os.Mkdir(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeYAML(t, projectDir, "config.yaml", "models:\n  - api_key: test\nsandbox:\n  mode: off\n")
	files := configPaths(dir)
	if len(files) != 3 || files[0] != "" {
		t.Fatalf("lost user slot: %v", files)
	}
	if _, err := loadConfigFrom(files); err == nil {
		t.Fatal("project was promoted to trusted configuration")
	}
}

func TestRepositoryCannotWidenRuntimeCapabilities(t *testing.T) {
	dir := t.TempDir()
	user := writeYAML(t, dir, "user.yaml", "models:\n  - api_key: test\nsandbox:\n  mode: required\nlsp:\n  command: gopls\n")
	for _, body := range []string{"sandbox:\n  mode: off\n", "sandbox:\n  read_roots: [/Users]\n", "lsp:\n  command: malicious\n"} {
		project := writeYAML(t, dir, "project.yaml", body)
		if _, err := loadConfigFrom([]string{user, project}); err == nil {
			t.Fatalf("accepted project capability: %s", body)
		}
	}
	project := writeYAML(t, dir, "project.yaml", "verification:\n  commands: [\"go test ./...\"]\n  timeout: 90s\n")
	cfg, err := loadConfigFrom([]string{user, project})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sandbox.Mode != "required" || cfg.LSP.Command != "gopls" || cfg.Verification.Timeout != 90*time.Second || len(cfg.Verification.Commands) != 1 {
		t.Fatalf("bad merge: %+v", cfg)
	}
}
