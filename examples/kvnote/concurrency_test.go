package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConcurrentWriterProcess(t *testing.T) {
	if os.Getenv("KVNOTE_WRITER_HELPER") != "1" {
		return
	}
	// The parent releases all writers together, exercising OS locks across
	// independent processes rather than relying on a Go mutex in one process.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(os.Getenv("KVNOTE_BARRIER")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			os.Exit(3)
		}
		time.Sleep(time.Millisecond)
	}
	args := strings.Split(os.Getenv("KVNOTE_ARGUMENTS"), "\x1f")
	os.Exit(run(args, strings.NewReader(""), os.Stdout, os.Stderr))
}

func TestConcurrentUpdatesPreserveEveryWriter(t *testing.T) {
	for round := 0; round < 3; round++ {
		dir := t.TempDir()
		path := filepath.Join(dir, "notes.json")
		barrier := filepath.Join(dir, "start")
		initial := map[string]string{"delete-me": "old", "preserve": "original"}
		for i := 0; i < 10000; i++ {
			initial[fmt.Sprintf("base-%d", i)] = strings.Repeat("v", 32)
		}
		data, _ := json.Marshal(initial)
		writeFile(t, path, string(data))
		type child struct {
			cmd    *exec.Cmd
			output *bytes.Buffer
		}
		var children []child
		for i := 0; i < 8; i++ {
			args := []string{"--file", path, "set", fmt.Sprintf("writer-%d", i), "ok"}
			if i == 6 {
				source := filepath.Join(dir, "import.json")
				writeFile(t, source, `{"imported":"yes"}`)
				args = []string{"--file", path, "import", source}
			} else if i == 7 {
				args = []string{"--file", path, "del", "delete-me"}
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestConcurrentWriterProcess$")
			cmd.Env = append(os.Environ(), "KVNOTE_WRITER_HELPER=1", "KVNOTE_BARRIER="+barrier, "KVNOTE_ARGUMENTS="+strings.Join(args, "\x1f"))
			out := &bytes.Buffer{}
			cmd.Stdout = out
			cmd.Stderr = out
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			children = append(children, child{cmd, out})
		}
		writeFile(t, barrier, "go")
		for _, c := range children {
			if err := c.cmd.Wait(); err != nil {
				t.Fatalf("writer: %v: %s", err, c.output)
			}
		}
		var got map[string]string
		if err := json.Unmarshal([]byte(readFile(t, path)), &got); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 6; i++ {
			if got[fmt.Sprintf("writer-%d", i)] != "ok" {
				t.Fatalf("round %d lost writer %d", round, i)
			}
		}
		if _, exists := got["delete-me"]; exists {
			t.Fatal("delete was lost")
		}
		if got["imported"] != "yes" || got["preserve"] != "original" || len(got) != 10008 {
			t.Fatalf("lost update: entries=%d", len(got))
		}
	}
}

type failedOutput struct{}

func (failedOutput) Write([]byte) (int, error) { return 0, fmt.Errorf("output unavailable") }
func TestOutputFailureIsNotSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.json")
	writeFile(t, path, `{"key":"value"}`)
	for _, args := range [][]string{{"list"}, {"get", "key"}} {
		var stderr bytes.Buffer
		if code := run(append([]string{"--file", path}, args...), strings.NewReader(""), failedOutput{}, &stderr); code == 0 {
			t.Fatal("output failure reported success")
		}
	}
}
