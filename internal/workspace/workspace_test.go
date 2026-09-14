package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func testWorkspace(t *testing.T) *Workspace {
	t.Helper()
	w, err := Open(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	return w
}
func put(t *testing.T, w *Workspace, p, s string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(w.Path(), p), []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBoundary(t *testing.T) {
	w := testWorkspace(t)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(w.Path(), "link")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"../secret", secret, "link/secret"} {
		if _, err := w.ReadFile(p); err == nil {
			t.Errorf("read escaped: %s", p)
		}
		if _, err := w.Apply([]Change{{Path: p, BeforeHash: "missing", Content: "bad"}}); err == nil {
			t.Errorf("write escaped: %s", p)
		}
	}
	for _, p := range []string{".git/config", ".codeclaw/config.yaml", "sub/.agents/a"} {
		if _, err := w.Apply([]Change{{Path: p, BeforeHash: "missing", Content: "bad"}}); err == nil {
			t.Errorf("metadata writable: %s", p)
		}
	}
	got, _ := os.ReadFile(secret)
	if string(got) != "private" {
		t.Fatal("outside file changed")
	}
}

func TestJournalUndoAfterReopenAndUserConflict(t *testing.T) {
	root, journal := t.TempDir(), t.TempDir()
	w, err := Open(root, journal)
	if err != nil {
		t.Fatal(err)
	}
	put(t, w, "a.go", "old")
	tx, err := w.Apply([]Change{{Path: "a.go", BeforeHash: Hash([]byte("old")), Content: "new"}, {Path: "new.go", BeforeHash: "missing", Content: "created"}})
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	w, err = Open(root, journal)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	put(t, w, "a.go", "user edit")
	if _, err = w.Undo(tx.ID); err == nil {
		t.Fatal("undo overwrote user's edit")
	}
	if _, err = w.ReadFile("new.go"); err != nil {
		t.Fatal("undo partially applied before conflict check")
	}
	put(t, w, "a.go", "new")
	if _, err = w.Undo(tx.ID); err != nil {
		t.Fatal(err)
	}
	b, _ := w.ReadFile("a.go")
	if string(b) != "old" {
		t.Fatalf("got %s", b)
	}
	if _, err = w.ReadFile("new.go"); !os.IsNotExist(err) {
		t.Fatalf("new file not removed: %v", err)
	}
}

func TestBatchPreflightAndContentFingerprint(t *testing.T) {
	w := testWorkspace(t)
	put(t, w, "a", "old")
	put(t, w, "b", "old")
	_, err := w.Apply([]Change{{Path: "a", BeforeHash: Hash([]byte("old")), Content: "new"}, {Path: "b", BeforeHash: Hash([]byte("other")), Content: "new"}})
	if err == nil {
		t.Fatal("stale write accepted")
	}
	b, _ := w.ReadFile("a")
	if string(b) != "old" {
		t.Fatal("batch partially written")
	}
	fp, err := w.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	w.MarkVerified(fp)
	if !w.Verified() {
		t.Fatal("unchanged snapshot not verified")
	}
	put(t, w, "a", "new")
	if w.Verified() {
		t.Fatal("external write did not invalidate verification")
	}
}

func TestRecoverPreparedBatch(t *testing.T) {
	w := testWorkspace(t)
	put(t, w, "a", "old")
	tx := Transaction{ID: newID(), State: "prepared", Files: []FileChange{{Path: "a", Before: []byte("old"), After: []byte("new"), Existed: true, Mode: 0o644}, {Path: "b", After: []byte("new"), Mode: 0o644}}}
	if err := w.save(tx); err != nil {
		t.Fatal(err)
	}
	// Crash after only the first file was installed.
	put(t, w, "a", "new")
	if _, err := w.Undo(tx.ID); err != nil {
		t.Fatal(err)
	}
	b, _ := w.ReadFile("a")
	if string(b) != "old" {
		t.Fatal(string(b))
	}
}

func TestCWDIndependentAndBoundedRead(t *testing.T) {
	a, b := testWorkspace(t), testWorkspace(t)
	put(t, a, "file", "a")
	put(t, b, "file", "b")
	aa, _ := a.ReadFile("file")
	bb, _ := b.ReadFile("file")
	if string(aa) != "a" || string(bb) != "b" {
		t.Fatal("cwd contamination")
	}
	put(t, a, "big", strings.Repeat("a", MaxFileBytes+1))
	if _, err := a.ReadFile("big"); err == nil {
		t.Fatal("oversized file read")
	}
}

func TestConcurrentWritersUseCompareAndSwap(t *testing.T) {
	w := testWorkspace(t)
	put(t, w, "file", "base")
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, content := range []string{"one", "two"} {
		wg.Add(1)
		go func(content string) {
			defer wg.Done()
			_, err := w.Apply([]Change{{Path: "file", BeforeHash: Hash([]byte("base")), Content: content}})
			results <- err
		}(content)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("successful writes=%d, want exactly one", success)
	}
}

func TestJournalOutsideRootAndHardlinkReplacement(t *testing.T) {
	root := t.TempDir()
	if w, err := Open(root, filepath.Join(root, "journal")); err == nil {
		w.Close()
		t.Fatal("writable journal accepted")
	}
	w := testWorkspace(t)
	outside := filepath.Join(t.TempDir(), "file")
	os.WriteFile(outside, []byte("original"), 0o600)
	if err := os.Link(outside, filepath.Join(w.Path(), "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Apply([]Change{{Path: "link", BeforeHash: Hash([]byte("original")), Content: "changed"}}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(outside)
	if string(b) != "original" {
		t.Fatal("atomic replacement modified external hardlink")
	}
	fp, err := w.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	w.MarkVerified(fp)
	if err := os.Symlink("link", filepath.Join(w.Path(), "alias")); err != nil {
		t.Fatal(err)
	}
	if w.Verified() {
		t.Fatal("symlink addition did not invalidate evidence")
	}
}
