package workspace

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// Change requires an observed hash; "missing" is the only create precondition.
type Change struct {
	Path       string `json:"path"`
	BeforeHash string `json:"before_sha256"`
	Content    string `json:"content"`
	Delete     bool   `json:"delete,omitempty"`
}
type FileChange struct {
	Path    string      `json:"path"`
	Before  []byte      `json:"before"`
	After   []byte      `json:"after"`
	Existed bool        `json:"existed"`
	Deleted bool        `json:"deleted"`
	Mode    fs.FileMode `json:"mode"`
}
type Transaction struct {
	ID        string       `json:"id"`
	State     string       `json:"state"`
	CreatedAt time.Time    `json:"created_at"`
	Files     []FileChange `json:"files"`
}

func errorsJoin(errs ...error) error { return errors.Join(errs...) }
func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (w *Workspace) lockJournal() error {
	return syscall.Flock(int(w.lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
func (w *Workspace) unlockJournal() { _ = syscall.Flock(int(w.lock.Fd()), syscall.LOCK_UN) }

// Apply preflights the entire batch, journals before-images durably, then uses
// same-directory atomic renames. Interrupted batches are recoverable with Undo.
// This is not a cross-file atomic transaction against external writers.
func (w *Workspace) Apply(changes []Change) (Transaction, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.lockJournal(); err != nil {
		return Transaction{}, err
	}
	defer w.unlockJournal()
	tx := Transaction{ID: newID(), State: "prepared", CreatedAt: time.Now().UTC()}
	if len(changes) == 0 || len(changes) > 64 {
		return tx, fmt.Errorf("changes must contain 1..64 files")
	}
	seen := map[string]bool{}
	total := 0
	for _, c := range changes {
		p, err := w.check(c.Path, true)
		if err != nil {
			return tx, err
		}
		if p == "." || seen[p] {
			return tx, fmt.Errorf("duplicate or invalid path: %s", p)
		}
		seen[p] = true
		b, err := w.ReadFile(p)
		exists := !os.IsNotExist(err)
		if err != nil && exists {
			return tx, err
		}
		actual := "missing"
		mode := fs.FileMode(0o644)
		if exists {
			actual = Hash(b)
			st, err := w.Stat(p)
			if err != nil {
				return tx, err
			}
			mode = st.Mode().Perm()
		}
		if c.BeforeHash != actual {
			return tx, fmt.Errorf("conflict in %s: expected %s, found %s; read again", p, c.BeforeHash, actual)
		}
		if c.Delete && !exists {
			return tx, fmt.Errorf("cannot delete missing file: %s", p)
		}
		if len(c.Content) > MaxFileBytes {
			return tx, fmt.Errorf("change exceeds file size limit: %s", p)
		}
		total += len(b) + len(c.Content)
		if total > 32<<20 {
			return tx, fmt.Errorf("transaction exceeds 32 MiB")
		}
		tx.Files = append(tx.Files, FileChange{Path: p, Before: b, After: []byte(c.Content), Existed: exists, Deleted: c.Delete, Mode: mode})
	}
	if err := w.save(tx); err != nil {
		return tx, err
	}
	w.verified = ""
	for _, f := range tx.Files {
		if err := w.matches(f.Path, f.Before, f.Existed); err != nil {
			return tx, fmt.Errorf("transaction %s interrupted: %w; use undo", tx.ID, err)
		}
		if err := w.replace(f.Path, f.After, f.Mode, f.Deleted); err != nil {
			return tx, fmt.Errorf("transaction %s interrupted: %w; use undo", tx.ID, err)
		}
	}
	tx.State = "applied"
	return tx, w.save(tx)
}

func (w *Workspace) matches(path string, want []byte, exists bool) error {
	b, err := w.ReadFile(path)
	if !exists && os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !exists || Hash(b) != Hash(want) {
		return fmt.Errorf("file changed concurrently: %s", path)
	}
	return nil
}

// replace holds a parent directory handle, so renames never follow a final-file
// symlink or truncate an existing hard link's other name.
func (w *Workspace) replace(path string, data []byte, mode fs.FileMode, remove bool) error {
	p, err := w.check(path, true)
	if err != nil {
		return err
	}
	if err = w.root.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	dir, err := w.root.OpenRoot(filepath.Dir(p))
	if err != nil {
		return err
	}
	defer dir.Close()
	if remove {
		if err = dir.Remove(filepath.Base(p)); err != nil {
			return err
		}
		d, err := dir.Open(".")
		if err != nil {
			return err
		}
		defer d.Close()
		return d.Sync()
	}
	name := ".codeclaw-write-" + newID()
	f, err := dir.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer dir.Remove(name)
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err = errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err = dir.Rename(name, filepath.Base(p)); err != nil {
		return err
	}
	d, err := dir.Open(".")
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (w *Workspace) save(tx Transaction) error {
	b, err := json.Marshal(tx)
	if err != nil {
		return err
	}
	name := ".pending-" + newID()
	f, err := w.journal.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer w.journal.Remove(name)
	_, e1 := f.Write(b)
	e2 := f.Sync()
	e3 := f.Close()
	if err = errors.Join(e1, e2, e3); err != nil {
		return err
	}
	if err = w.journal.Rename(name, tx.ID+".json"); err != nil {
		return err
	}
	d, err := w.journal.Open(".")
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (w *Workspace) load(id string) (Transaction, error) {
	var tx Transaction
	if len(id) != 32 {
		return tx, fmt.Errorf("invalid transaction id")
	}
	if _, err := hex.DecodeString(id); err != nil {
		return tx, fmt.Errorf("invalid transaction id")
	}
	b, err := w.journal.ReadFile(id + ".json")
	if err != nil {
		return tx, err
	}
	err = json.Unmarshal(b, &tx)
	if err == nil && tx.ID != id {
		err = fmt.Errorf("transaction id mismatch")
	}
	return tx, err
}

// Undo refuses to overwrite any contents not equal to the recorded before or
// after version. It also recovers partially applied/prepared transactions.
func (w *Workspace) Undo(id string) (Transaction, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.lockJournal(); err != nil {
		return Transaction{}, err
	}
	defer w.unlockJournal()
	tx, err := w.load(id)
	if err != nil {
		return tx, err
	}
	if tx.State == "undone" {
		return tx, fmt.Errorf("transaction already undone")
	}
	var restore []FileChange
	for _, f := range tx.Files {
		if _, err := w.check(f.Path, true); err != nil {
			return tx, err
		}
		if w.matches(f.Path, f.Before, f.Existed) == nil {
			continue
		}
		if err := w.matches(f.Path, f.After, !f.Deleted); err != nil {
			return tx, fmt.Errorf("undo conflict: %w", err)
		}
		restore = append(restore, f)
	}
	w.verified = ""
	for _, f := range restore {
		if err = w.matches(f.Path, f.After, !f.Deleted); err != nil {
			return tx, err
		}
		if err = w.replace(f.Path, f.Before, f.Mode, !f.Existed); err != nil {
			return tx, err
		}
	}
	tx.State = "undone"
	return tx, w.save(tx)
}

// History omits file contents so inspection does not flood the model context.
func (w *Workspace) History() ([]Transaction, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	names, err := fs.Glob(w.journal.FS(), "*.json")
	if err != nil {
		return nil, err
	}
	var out []Transaction
	for _, name := range names {
		tx, err := w.load(name[:len(name)-5])
		if err != nil {
			return nil, err
		}
		for i := range tx.Files {
			tx.Files[i].Before = nil
			tx.Files[i].After = nil
		}
		out = append(out, tx)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > 20 {
		out = out[:20]
	}
	return out, nil
}
