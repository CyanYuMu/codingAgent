package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// runCLI drives the in-process entry point with injected streams and returns
// the exit code, stdout and stderr.
func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code = run(args, strings.NewReader(""), &out, &errBuf)
	return code, out.String(), errBuf.String()
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestSetGetListDel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.json")

	if code, out, errOut := runCLI(t, "--file", path, "set", "greeting", "hello"); code != 0 {
		t.Fatalf("set: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if code, out, errOut := runCLI(t, "--file", path, "set", "empty", ""); code != 0 {
		t.Fatalf("set empty value: code=%d stdout=%q stderr=%q", code, out, errOut)
	}

	// get outputs the raw value followed by exactly one newline.
	code, out, errOut := runCLI(t, "--file", path, "get", "greeting")
	if code != 0 || out != "hello\n" || errOut != "" {
		t.Fatalf("get: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	code, out, _ = runCLI(t, "--file", path, "get", "empty")
	if code != 0 || out != "\n" {
		t.Fatalf("get empty: code=%d stdout=%q", code, out)
	}

	_, list, _ := runCLI(t, "--file", path, "list")
	var got map[string]string
	if err := json.Unmarshal([]byte(list), &got); err != nil {
		t.Fatalf("list output not JSON: %q (%v)", list, err)
	}
	want := map[string]string{"greeting": "hello", "empty": ""}
	if len(got) != len(want) || got["greeting"] != "hello" || got["empty"] != "" {
		t.Fatalf("list = %v, want %v", got, want)
	}

	if code, out, errOut := runCLI(t, "--file", path, "del", "greeting"); code != 0 {
		t.Fatalf("del: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if code, out, _ := runCLI(t, "--file", path, "get", "greeting"); code == 0 || out != "" {
		t.Fatalf("get after del should fail with empty stdout: code=%d stdout=%q", code, out)
	}
}

func TestMissingFileTreatedAsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.json")

	if code, out, errOut := runCLI(t, "--file", path, "list"); code != 0 || out != "{}\n" || errOut != "" {
		t.Fatalf("list missing file: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if code, out, _ := runCLI(t, "--file", path, "get", "nope"); code == 0 || out != "" {
		t.Fatalf("get on missing file should fail: code=%d stdout=%q", code, out)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("read-only commands must not create the file: stat err=%v", err)
	}
}

func TestMissingKeyGetDelFail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.json")
	writeFile(t, path, `{"a":"1"}`)

	code, out, errOut := runCLI(t, "--file", path, "get", "missing")
	if code == 0 || out != "" || !strings.Contains(errOut, "missing") {
		t.Fatalf("get missing key: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	code, out, errOut = runCLI(t, "--file", path, "del", "missing")
	if code == 0 || out != "" {
		t.Fatalf("del missing key: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if readFile(t, path) != `{"a":"1"}` {
		t.Fatalf("failed del must not modify file: %q", readFile(t, path))
	}
}

func TestKeyValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.json")
	writeFile(t, path, `{"keep":"safe"}`)
	before := readFile(t, path)

	for _, key := range []string{"", "   ", "\t"} {
		if code, _, _ := runCLI(t, "--file", path, "set", key, "v"); code != 2 {
			t.Fatalf("set with bad key %q: code=%d, want 2", key, code)
		}
		if code, _, _ := runCLI(t, "--file", path, "get", key); code != 2 {
			t.Fatalf("get with bad key %q: code=%d, want 2", key, code)
		}
		if code, _, _ := runCLI(t, "--file", path, "del", key); code != 2 {
			t.Fatalf("del with bad key %q: code=%d, want 2", key, code)
		}
	}
	if readFile(t, path) != before {
		t.Fatalf("invalid key must not modify file: %q", readFile(t, path))
	}
}

func TestUsageErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.json")
	cases := [][]string{
		{"--file", path, "set", "only-key"},
		{"--file", path, "set", "k", "v", "extra"},
		{"--file", path, "get"},
		{"--file", path, "get", "k", "extra"},
		{"--file", path, "del"},
		{"--file", path, "list", "extra"},
		{"--file", path, "bogus"},
		{"--file", path},
	}
	for _, args := range cases {
		if code, _, _ := runCLI(t, args...); code != 2 {
			t.Fatalf("args %v: code=%d, want 2", args, code)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("usage errors must not create file: stat err=%v", err)
	}
}

// TestCorruptFileProtection verifies that unreadable/invalid data never causes a
// write, leaving the original bytes untouched.
func TestCorruptFileProtection(t *testing.T) {
	bad := []struct {
		name    string
		content string
	}{
		{"corrupt", `{"a":`},
		{"trailing", `{"a":"1"} {"b":"2"}`},
		{"trailing-garbage", `{"a":"1"}garbage`},
		{"null", `null`},
		{"array", `["a","b"]`},
		{"number", `42`},
		{"bool", `true`},
		{"nested-object", `{"a":{"b":"c"}}`},
		{"non-string-value", `{"a":1}`},
		{"null-value", `{"a":null}`},
		{"array-value", `{"a":["x"]}`},
		{"empty", ""},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "notes.json")
			writeFile(t, path, tc.content)

			for _, args := range [][]string{
				{"--file", path, "set", "k", "v"},
				{"--file", path, "del", "k"},
				{"--file", path, "get", "k"},
				{"--file", path, "list"},
			} {
				code, out, errOut := runCLI(t, args...)
				if code == 0 {
					t.Fatalf("args %v on %s: expected failure", args, tc.name)
				}
				if out != "" {
					t.Fatalf("args %v on %s: stdout should be empty, got %q", args, tc.name, out)
				}
				if errOut == "" {
					t.Fatalf("args %v on %s: stderr should explain the failure", args, tc.name)
				}
				if got := readFile(t, path); got != tc.content {
					t.Fatalf("file changed after %v: got %q want %q", args, got, tc.content)
				}
			}
		})
	}
}

func TestWriteIsAtomicAndPrivate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.json")

	if code, _, errOut := runCLI(t, "--file", path, "set", "k", "v"); code != 0 {
		t.Fatalf("set failed: code=%d stderr=%q", code, errOut)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("new file perm = %o, want 600", perm)
		}
	}

	// No leftover temp files after a successful write.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 2 || entries[0].Name() != "notes.json" || entries[1].Name() != "notes.json.lock" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("unexpected directory contents: %v", names)
	}

	// A failed operation must not leave temp files either.
	writeFile(t, path, `{corrupt`)
	if code, _, _ := runCLI(t, "--file", path, "set", "x", "y"); code == 0 {
		t.Fatal("expected failure on corrupt file")
	}
	entries, _ = os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("temp file leaked after failure: %v", entries)
	}
}

func TestDefaultFile(t *testing.T) {
	dir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)

	if code, _, errOut := runCLI(t, "set", "k", "v"); code != 0 {
		t.Fatalf("set with default file: code=%d stderr=%q", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, defaultFile)); err != nil {
		t.Fatalf("default notes.json not created: %v", err)
	}
}

// TestImportMergesAndOverrides verifies import merges a source object into the
// target, with source keys overriding existing ones, and that new keys appear.
func TestImportMergesAndOverrides(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "notes.json")
	src := filepath.Join(dir, "import.json")
	writeFile(t, target, `{"a":"old","b":"keep"}`)
	writeFile(t, src, `{"a":"new","c":"added"}`)

	if code, out, errOut := runCLI(t, "--file", target, "import", src); code != 0 || out != "" || errOut != "" {
		t.Fatalf("import: code=%d stdout=%q stderr=%q", code, out, errOut)
	}

	var got map[string]string
	if err := json.Unmarshal([]byte(readFile(t, target)), &got); err != nil {
		t.Fatalf("target not valid JSON: %v", err)
	}
	want := map[string]string{"a": "new", "b": "keep", "c": "added"}
	if len(got) != len(want) {
		t.Fatalf("import result = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("key %q = %q, want %q", k, got[k], v)
		}
	}
}

// TestImportIntoMissingTarget covers importing when the target does not exist
// yet: it should behave like an empty object.
func TestImportIntoMissingTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "notes.json")
	src := filepath.Join(dir, "import.json")
	writeFile(t, src, `{"x":"1"}`)

	if code, _, errOut := runCLI(t, "--file", target, "import", src); code != 0 {
		t.Fatalf("import into missing target: code=%d stderr=%q", code, errOut)
	}
	if got := strings.TrimSpace(readFile(t, target)); got != `{"x":"1"}` {
		t.Fatalf("target = %q, want {\"x\":\"1\"}", got)
	}
}

// TestImportRejectsInvalidInputs ensures that any invalid source or target
// leaves the target bytes untouched and produces empty stdout.
func TestImportRejectsInvalidInputs(t *testing.T) {
	badSources := []struct {
		name    string
		content string
	}{
		{"corrupt", `{"a":`},
		{"trailing", `{"a":"1"} {"b":"2"}`},
		{"trailing-garbage", `{"a":"1"}x`},
		{"null", `null`},
		{"array", `["a"]`},
		{"number", `7`},
		{"non-string-value", `{"a":1}`},
		{"null-value", `{"a":null}`},
		{"array-value", `{"a":["x"]}`},
		{"blank-key", `{"   ":"v"}`},
		{"empty-key", `{"":"v"}`},
	}
	for _, tc := range badSources {
		t.Run("source/"+tc.name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "notes.json")
			src := filepath.Join(dir, "import.json")
			const original = `{"keep":"safe"}`
			writeFile(t, target, original)
			writeFile(t, src, tc.content)

			code, out, errOut := runCLI(t, "--file", target, "import", src)
			if code == 0 || out != "" || errOut == "" {
				t.Fatalf("import %s: code=%d stdout=%q stderr=%q", tc.name, code, out, errOut)
			}
			if got := readFile(t, target); got != original {
				t.Fatalf("target changed on %s: got %q want %q", tc.name, got, original)
			}
		})
	}

	// Invalid target must also abort the import without touching the target.
	t.Run("invalid-target", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "notes.json")
		src := filepath.Join(dir, "import.json")
		const original = `{broken`
		writeFile(t, target, original)
		writeFile(t, src, `{"a":"1"}`)

		code, out, errOut := runCLI(t, "--file", target, "import", src)
		if code == 0 || out != "" || errOut == "" {
			t.Fatalf("import with bad target: code=%d stdout=%q stderr=%q", code, out, errOut)
		}
		if got := readFile(t, target); got != original {
			t.Fatalf("target changed: got %q want %q", got, original)
		}
	})

	// A missing source file is a failure too, and must not alter the target.
	t.Run("missing-source", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "notes.json")
		const original = `{"keep":"safe"}`
		writeFile(t, target, original)

		code, out, errOut := runCLI(t, "--file", target, "import", filepath.Join(dir, "absent.json"))
		if code == 0 || out != "" || errOut == "" {
			t.Fatalf("import missing source: code=%d stdout=%q stderr=%q", code, out, errOut)
		}
		if got := readFile(t, target); got != original {
			t.Fatalf("target changed: got %q want %q", got, original)
		}
	})
}

// TestImportUsageErrors checks the argument arity for import.
func TestImportUsageErrors(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "notes.json")
	for _, args := range [][]string{
		{"--file", target, "import"},
		{"--file", target, "import", "a.json", "b.json"},
	} {
		if code, out, _ := runCLI(t, args...); code != 2 || out != "" {
			t.Fatalf("args %v: code=%d stdout=%q, want code 2 and empty stdout", args, code, out)
		}
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("usage error must not create target: stat err=%v", err)
	}
}

// TestEndToEndBinary builds the real kvnote binary and exercises it as a
// subprocess, covering exit codes and stream separation end to end.
func TestEndToEndBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping build in short mode")
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "kvnote")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	file := filepath.Join(tmp, "notes.json")
	runBin := func(args ...string) (code int, stdout, stderr string) {
		t.Helper()
		full := append([]string{"--file", file}, args...)
		cmd := exec.Command(bin, full...)
		var out, errBuf bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errBuf
		err := cmd.Run()
		if err != nil {
			var exitErr *exec.ExitError
			if !asExitError(err, &exitErr) {
				t.Fatalf("run %v: %v", full, err)
			}
			code = exitErr.ExitCode()
		}
		return code, out.String(), errBuf.String()
	}

	if code, _, errOut := runBin("set", "lang", "go"); code != 0 {
		t.Fatalf("set: code=%d stderr=%q", code, errOut)
	}
	if code, out, _ := runBin("get", "lang"); code != 0 || out != "go\n" {
		t.Fatalf("get: code=%d stdout=%q", code, out)
	}
	if code, out, errOut := runBin("get", "absent"); code == 0 || out != "" || errOut == "" {
		t.Fatalf("get absent: code=%d stdout=%q stderr=%q", code, out, errOut)
	}

	// import a source file through the real binary.
	src := filepath.Join(tmp, "import.json")
	writeFile(t, src, `{"lang":"rust","extra":"yes"}`)
	if code, out, errOut := runBin("import", src); code != 0 || out != "" || errOut != "" {
		t.Fatalf("import: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if code, out, _ := runBin("get", "lang"); code != 0 || out != "rust\n" {
		t.Fatalf("get after import: code=%d stdout=%q", code, out)
	}

	// Corrupt the file and confirm the binary refuses to touch it.
	corrupt := `{"lang":"go"` // missing brace
	writeFile(t, file, corrupt)
	if code, out, errOut := runBin("set", "x", "y"); code == 0 || out != "" || errOut == "" {
		t.Fatalf("set on corrupt: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if readFile(t, file) != corrupt {
		t.Fatalf("corrupt file modified: %q", readFile(t, file))
	}
}

func asExitError(err error, target **exec.ExitError) bool {
	if e, ok := err.(*exec.ExitError); ok {
		*target = e
		return true
	}
	return false
}
