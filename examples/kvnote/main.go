// Command kvnote is a tiny JSON-backed key/value note store that only uses the
// Go standard library.
//
// Usage:
//
//	kvnote [--file PATH] set KEY VALUE
//	kvnote [--file PATH] get KEY
//	kvnote [--file PATH] list
//	kvnote [--file PATH] del KEY
//	kvnote [--file PATH] import JSONFILE
//
// The notes file is a JSON object whose values are strings. A missing file is
// treated as an empty object. Writes are atomic: data is written to a temporary
// file in the same directory and then renamed over the target.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// defaultFile is used when --file is not given.
const defaultFile = "notes.json"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run executes a single kvnote invocation. All standard streams are injected so
// the whole CLI can be driven from tests. It returns the process exit code:
// 0 on success, 1 for runtime failures (missing key, unreadable or invalid
// file), and 2 for usage or argument validation errors.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	_ = stdin // reserved for future interactive input

	flags := flag.NewFlagSet("kvnote", flag.ContinueOnError)
	flags.SetOutput(stderr)
	file := flags.String("file", defaultFile, "path to the JSON notes file")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	rest := flags.Args()
	if len(rest) == 0 {
		fmt.Fprintln(stderr, "kvnote: missing subcommand (want set, get, list, del or import)")
		return 2
	}

	switch cmd := rest[0]; cmd {
	case "set":
		if len(rest) != 3 {
			return usagef(stderr, "set needs exactly KEY and VALUE arguments")
		}
		key, value := rest[1], rest[2]
		if err := validateKey(key); err != nil {
			return usagef(stderr, "%v", err)
		}
		if err := updateNotes(*file, func(notes map[string]string) error {
			notes[key] = value
			return nil
		}); err != nil {
			return failf(stderr, "cannot write notes: %v", err)
		}
		return 0

	case "get":
		if len(rest) != 2 {
			return usagef(stderr, "get needs exactly KEY")
		}
		key := rest[1]
		if err := validateKey(key); err != nil {
			return usagef(stderr, "%v", err)
		}
		notes, err := loadNotes(*file)
		if err != nil {
			return failf(stderr, "cannot read notes: %v", err)
		}
		value, ok := notes[key]
		if !ok {
			return failf(stderr, "key %q not found", key)
		}
		if _, err := fmt.Fprintln(stdout, value); err != nil {
			return failf(stderr, "cannot write output: %v", err)
		}
		return 0

	case "list":
		if len(rest) != 1 {
			return usagef(stderr, "list takes no arguments")
		}
		notes, err := loadNotes(*file)
		if err != nil {
			return failf(stderr, "cannot read notes: %v", err)
		}
		data, err := json.Marshal(notes)
		if err != nil {
			return failf(stderr, "cannot encode notes: %v", err)
		}
		if _, err := fmt.Fprintln(stdout, string(data)); err != nil {
			return failf(stderr, "cannot write output: %v", err)
		}
		return 0

	case "del":
		if len(rest) != 2 {
			return usagef(stderr, "del needs exactly KEY")
		}
		key := rest[1]
		if err := validateKey(key); err != nil {
			return usagef(stderr, "%v", err)
		}
		if err := updateNotes(*file, func(notes map[string]string) error {
			if _, ok := notes[key]; !ok {
				return fmt.Errorf("key %q not found", key)
			}
			delete(notes, key)
			return nil
		}); err != nil {
			return failf(stderr, "cannot write notes: %v", err)
		}
		return 0

	case "import":
		if len(rest) != 2 {
			return usagef(stderr, "import needs exactly JSONFILE")
		}
		importPath := rest[1]
		// Validate both the source and the target completely before touching
		// either one; only then commit a single atomic replacement.
		incoming, err := readSourceFile(importPath)
		if err != nil {
			return failf(stderr, "cannot read import file: %v", err)
		}
		for key := range incoming {
			if err := validateKey(key); err != nil {
				return failf(stderr, "import file has invalid key %q: %v", key, err)
			}
		}
		if err := updateNotes(*file, func(notes map[string]string) error {
			for key, value := range incoming {
				notes[key] = value
			}
			return nil
		}); err != nil {
			return failf(stderr, "cannot write notes: %v", err)
		}
		return 0

	default:
		return usagef(stderr, "unknown subcommand %q", cmd)
	}
}

// updateNotes locks a stable sidecar inode before reading the target. Locking
// the JSON inode itself would stop protecting it after an atomic rename.
// All participating writers must use this path; external editors are not locked.
func updateNotes(path string, mutate func(map[string]string) error) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return err
	}
	path = filepath.Join(parent, filepath.Base(abs))
	if st, err := os.Lstat(path); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return errors.New("notes path must not be a symbolic link")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	// Keep the sidecar file: removing it would let a new writer lock a different
	// inode while another process is still queued on the original one.
	notes, err := loadNotes(path)
	if err != nil {
		return err
	}
	if err = mutate(notes); err != nil {
		return err
	}
	return storeNotes(path, notes)
}

// usagef prints a usage error to stderr and returns exit code 2.
func usagef(stderr io.Writer, format string, a ...any) int {
	fmt.Fprintf(stderr, "kvnote: "+format+"\n", a...)
	return 2
}

// failf prints a runtime error to stderr and returns exit code 1.
func failf(stderr io.Writer, format string, a ...any) int {
	fmt.Fprintf(stderr, "kvnote: "+format+"\n", a...)
	return 1
}

// validateKey rejects keys that are empty or contain only whitespace.
func validateKey(key string) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("key must not be empty or whitespace only")
	}
	return nil
}

// loadNotes reads and validates the notes file. A missing file yields an empty
// map and no error. Anything that is not a JSON object with string values is an
// error. loadNotes never writes to the file.
func loadNotes(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	notes, err := decodeNotes(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return notes, nil
}

// readSourceFile reads and validates a file that must exist, such as an import
// source. Unlike loadNotes it does not treat a missing file as an empty object.
func readSourceFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	notes, err := decodeNotes(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return notes, nil
}

// decodeNotes parses a JSON object of string values. It rejects null, arrays,
// numbers, booleans, nested objects, corrupt JSON and any trailing content
// after the first top-level value.
func decodeNotes(data []byte) (map[string]string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))

	var raw map[string]json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if raw == nil {
		return nil, errors.New("top-level JSON value must be an object")
	}

	// Reject a second top-level value or trailing garbage.
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("unexpected trailing data after JSON object")
	}

	notes := make(map[string]string, len(raw))
	for key, value := range raw {
		var decoded any
		if err := json.Unmarshal(value, &decoded); err != nil {
			return nil, fmt.Errorf("invalid value for key %q: %w", key, err)
		}
		s, ok := decoded.(string)
		if !ok {
			return nil, fmt.Errorf("value for key %q must be a string", key)
		}
		notes[key] = s
	}
	return notes, nil
}

// storeNotes serialises notes and atomically replaces path: the data is written
// to a fresh temporary file in the same directory (mode 0600) which is then
// renamed over the target. The original file is never truncated in place.
func storeNotes(path string, notes map[string]string) error {
	data, err := json.Marshal(notes)
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".kvnote-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	tmpName = "" // successfully renamed; nothing to clean up
	return nil
}
