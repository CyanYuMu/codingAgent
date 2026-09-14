package workspace

import (
	"encoding/json"
	"errors"
	"os"
)

func (w *Workspace) RecordVerification(report any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	b, err := json.Marshal(report)
	if err != nil {
		return err
	}
	f, err := w.journal.OpenFile("verification-"+newID()+".log", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, e1 := f.Write(b)
	e2 := f.Sync()
	e3 := f.Close()
	return errors.Join(e1, e2, e3)
}
