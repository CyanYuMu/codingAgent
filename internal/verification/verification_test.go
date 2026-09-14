package verification

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"einoclaw-build/internal/runtime"
	"einoclaw-build/internal/workspace"
)

func setup(t *testing.T, commands ...string) (*Service, *workspace.Workspace) {
	t.Helper()
	w, err := workspace.Open(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	s, err := runtime.NewProcessSandbox(w.Path(), runtime.SandboxConfig{Mode: "off"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	v, err := New(w, s, Config{Commands: commands, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return v, w
}

func TestFailureRepairAndStaleEvidence(t *testing.T) {
	v, w := setup(t, "test \"$(cat result)\" = good")
	if err := os.WriteFile(filepath.Join(w.Path(), "result"), []byte("bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := v.CheckCompletion(context.Background()); err == nil {
		t.Fatal("unverified change accepted")
	}
	r, err := v.Run(context.Background())
	if err != nil || r.Passed {
		t.Fatalf("bad report: %+v %v", r, err)
	}
	if _, err = w.Apply([]workspace.Change{{Path: "result", BeforeHash: workspace.Hash([]byte("bad")), Content: "good"}}); err != nil {
		t.Fatal(err)
	}
	r, err = v.Run(context.Background())
	if err != nil || !r.Passed {
		t.Fatalf("good report: %+v %v", r, err)
	}
	if err = v.CheckCompletion(context.Background()); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(w.Path(), "result"), []byte("changed by user"), 0o644)
	if err = v.CheckCompletion(context.Background()); err == nil {
		t.Fatal("stale verification accepted")
	}
}

func TestMutatingAndCancelledChecksCannotPass(t *testing.T) {
	v, _ := setup(t, "printf changed > source")
	r, err := v.Run(context.Background())
	if err != nil || r.Passed || r.Before == r.After {
		t.Fatalf("mutating verification: %+v %v", r, err)
	}
	v, _ = setup(t, "sleep 30")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	r, err = v.Run(ctx)
	if err != nil || r.Passed || r.Error == "" {
		t.Fatalf("cancelled verification: %+v %v", r, err)
	}
}
