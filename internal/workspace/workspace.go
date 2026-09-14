// Package workspace owns the file boundary shared by tools and verification.
package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

const MaxFileBytes = 8 << 20

type Workspace struct {
	mu       sync.Mutex
	root     *os.Root
	journal  *os.Root
	lock     *os.File
	path     string
	verified string
}

func Open(path, journal string) (*Workspace, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(journal, 0o700); err != nil {
		r.Close()
		return nil, err
	}
	j, err := os.OpenRoot(journal)
	journalAbs, e := filepath.Abs(journal)
	if e == nil {
		journalAbs, e = filepath.EvalSymlinks(journalAbs)
	}
	if e == nil {
		if rel, relErr := filepath.Rel(abs, journalAbs); relErr == nil && filepath.IsLocal(rel) {
			e = fmt.Errorf("change journal must be outside the workspace")
		}
	}
	if e != nil {
		r.Close()
		if j != nil {
			j.Close()
		}
		return nil, e
	}
	if err != nil {
		r.Close()
		return nil, err
	}
	l, err := j.OpenFile(".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		r.Close()
		j.Close()
		return nil, err
	}
	return &Workspace{root: r, journal: j, lock: l, path: abs}, nil
}

func (w *Workspace) Path() string { return w.path }
func (w *Workspace) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return errorsJoin(w.lock.Close(), w.journal.Close(), w.root.Close())
}

// Rel accepts absolute paths only inside this workspace. Resolution is always
// relative to the pinned os.Root, never the host's changing current directory.
func (w *Workspace) Rel(path string) (string, error) {
	if path == "" || strings.Contains(path, "://") {
		return "", fmt.Errorf("invalid workspace path %q", path)
	}
	if filepath.IsAbs(path) {
		var err error
		path, err = filepath.Rel(w.path, path)
		if err != nil {
			return "", err
		}
	}
	path = filepath.Clean(path)
	if !filepath.IsLocal(path) {
		return "", fmt.Errorf("path outside workspace: %q", path)
	}
	return path, nil
}

// Strict paths reject symlinks (including aliases into protected metadata).
// os.Root additionally enforces the root boundary during the actual operation.
func (w *Workspace) check(path string, write bool) (string, error) {
	rel, err := w.Rel(path)
	if err != nil {
		return "", err
	}
	prefix := ""
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if write && protected(part) {
			return "", fmt.Errorf("protected workspace metadata: %s", rel)
		}
		prefix = filepath.Join(prefix, part)
		st, err := w.root.Lstat(prefix)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("symlink paths are not supported: %s", rel)
		}
	}
	return rel, nil
}

func protected(part string) bool {
	switch strings.ToLower(part) {
	case ".git", ".codeclaw", ".agents", ".codex", ".claude":
		return true
	}
	return false
}

// SensitivePath marks workspace files whose contents commonly contain live
// credentials. They remain readable after explicit approval, but bulk search
// omits them and direct read/bash classification can require HITL.
func SensitivePath(path string) bool {
	clean := filepath.ToSlash(filepath.Clean(path))
	parts := strings.Split(strings.ToLower(clean), "/")
	for i, part := range parts {
		if part == ".codeclaw" && i+1 < len(parts) && (parts[i+1] == "config.yaml" || parts[i+1] == "config.yml") {
			return true
		}
		switch part {
		case ".npmrc", ".pypirc", ".netrc", "id_rsa", "id_ed25519", "credentials.json", "service-account.json":
			return true
		}
		if part == ".env" || (strings.HasPrefix(part, ".env.") && part != ".env.example" && part != ".env.sample" && part != ".env.template") {
			return true
		}
	}
	return false
}

func Hash(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }

func (w *Workspace) ReadFile(path string) ([]byte, error) {
	rel, err := w.check(path, false)
	if err != nil {
		return nil, err
	}
	f, err := w.root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > MaxFileBytes {
		return nil, fmt.Errorf("not a regular file or exceeds %d bytes: %s", MaxFileBytes, rel)
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if len(b) > MaxFileBytes {
		return nil, fmt.Errorf("file grew beyond read limit: %s", rel)
	}
	return b, err
}

func (w *Workspace) Stat(path string) (fs.FileInfo, error) {
	rel, err := w.check(path, false)
	if err != nil {
		return nil, err
	}
	return w.root.Stat(rel)
}

// Files returns a deterministic, bounded list without following symlinks.
func (w *Workspace) Files(path string) ([]string, error) {
	return w.files(path, false)
}

func (w *Workspace) files(path string, includeLinks bool) ([]string, error) {
	rel, err := w.check(path, false)
	if err != nil {
		return nil, err
	}
	var out []string
	err = fs.WalkDir(w.root.FS(), filepath.ToSlash(rel), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && protected(d.Name()) {
			return fs.SkipDir
		}
		if !includeLinks && d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if !d.IsDir() {
			if !includeLinks && SensitivePath(path) {
				return nil // safe search never sweeps credentials into model context
			}
			out = append(out, filepath.FromSlash(path))
			if len(out) > 20000 {
				return fmt.Errorf("workspace exceeds 20000 files; narrow the search")
			}
		}
		return nil
	})
	return out, err
}

// Fingerprint binds verification to file names, modes and contents. Protected
// agent/git metadata are excluded; generated files should live outside the root.
func (w *Workspace) Fingerprint() (string, error) {
	files, err := w.files(".", true)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	total := 0
	for _, p := range files {
		st, err := w.root.Lstat(p)
		if err != nil {
			return "", err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			target, err := w.root.Readlink(p)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(h, "symlink\x00%s\x00%s\n", p, target)
			continue
		}
		b, err := w.ReadFile(p)
		if err != nil {
			return "", err
		}
		total += len(b)
		if total > 128<<20 {
			return "", fmt.Errorf("verification snapshot exceeds 128 MiB")
		}
		fmt.Fprintf(h, "%s\x00%d\x00%s\n", p, st.Mode(), Hash(b))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (w *Workspace) MarkVerified(fingerprint string) {
	w.mu.Lock()
	w.verified = fingerprint
	w.mu.Unlock()
}
func (w *Workspace) Verified() bool {
	w.mu.Lock()
	prev := w.verified
	w.mu.Unlock()
	if prev == "" {
		return false
	}
	now, err := w.Fingerprint()
	return err == nil && now == prev
}
