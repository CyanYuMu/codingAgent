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
	// 同一 cwd 再次调用落到同一桶
	pd2, _ := ProjectDir(cwd)
	if pd2 != pd {
		t.Fatalf("bucket not stable: %q vs %q", pd, pd2)
	}
}

func TestGitRootFindsWorktreeMainRoot(t *testing.T) {
	root := t.TempDir()
	root, _ = Canonical(root)
	_ = os.MkdirAll(filepath.Join(root, ".git", "worktrees", "wt1"), 0o755)
	wt := t.TempDir()
	_ = os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+filepath.Join(root, ".git", "worktrees", "wt1")+"\n"), 0o644)
	sub := filepath.Join(wt, "a", "b")
	_ = os.MkdirAll(sub, 0o755)
	if got := GitRoot(sub); got != root {
		t.Fatalf("GitRoot = %q, want %q", got, root)
	}
	if got := GitRoot(filepath.Join(root, "x")); got != root {
		t.Fatalf("GitRoot under main = %q, want %q", got, root)
	}
	if got := GitRoot(t.TempDir()); got != "" {
		t.Fatalf("non-git should be empty, got %q", got)
	}
}

func TestProjectIDStableAndScoped(t *testing.T) {
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	sub := filepath.Join(root, "pkg")
	_ = os.MkdirAll(sub, 0o755)
	a, _ := ProjectID(root)
	b, _ := ProjectID(sub)
	if a != b || !strings.HasPrefix(a, strings.ToLower(filepath.Base(root))+"-") {
		t.Fatalf("ids %q %q", a, b)
	}
	other, _ := ProjectID(t.TempDir())
	if other == a {
		t.Fatal("different projects must differ")
	}
}

func TestAgentsDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODECLAW_HOME", home)
	got, err := UserAgentsDir()
	if err != nil || got != filepath.Join(home, "agents") {
		t.Fatalf("UserAgentsDir = %q err %v", got, err)
	}
	if got := ProjectAgentsDir("/x/y"); got != filepath.Join("/x/y", ".codeclaw", "agents") {
		t.Fatalf("ProjectAgentsDir = %q", got)
	}
}
