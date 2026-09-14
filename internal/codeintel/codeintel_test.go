package codeintel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	rt "einoclaw-build/internal/runtime"
)

func TestGoAST(t *testing.T) {
	a, err := AnalyzeGo("demo.go", []byte("package demo\ntype Box struct{}\nfunc (b Box) Value() int { return 3 }\nfunc Broken( {\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Diagnostics) == 0 || a.Diagnostics[0].Line != 4 {
		t.Fatalf("diagnostics: %+v", a.Diagnostics)
	}
	if len(a.Symbols) < 2 || a.Symbols[0].Name != "Box" || a.Symbols[1].Kind != "method" {
		t.Fatalf("symbols: %+v", a.Symbols)
	}
}

func TestFrameValidation(t *testing.T) {
	for _, input := range []string{"Content-Length: -1\r\n\r\n", "Content-Length: 16777217\r\n\r\n", "Content-Length: 2\r\nContent-Length: 2\r\n\r\n{}", "Content-Length: 10\r\n\r\n{}", strings.Repeat("a", 9000) + "\r\n"} {
		if _, err := readFrame(bufio.NewReader(strings.NewReader(input))); err == nil {
			t.Fatal("invalid frame accepted")
		}
	}
	var wire bytes.Buffer
	c := rpcClient{w: &wire}
	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": 1, "result": "中文"}); err != nil {
		t.Fatal(err)
	}
	m, err := readFrame(bufio.NewReader(&wire))
	if err != nil || string(m.Result) != "\"中文\"" {
		t.Fatalf("UTF-8 framing: %s %v", m.Result, err)
	}
}

func TestLSPHelper(t *testing.T) {
	if len(os.Args) == 0 || !strings.HasPrefix(os.Args[len(os.Args)-1], "lsp-fixture") {
		return
	}
	silent := os.Args[len(os.Args)-1] == "lsp-fixture-silent"
	c := rpcClient{r: bufio.NewReader(os.Stdin), w: os.Stdout}
	for {
		m, err := readFrame(c.r)
		if err != nil {
			os.Exit(0)
		}
		if m.Method == "exit" {
			os.Exit(0)
		}
		if len(m.ID) == 0 {
			continue
		}
		if silent {
			time.Sleep(time.Minute)
			os.Exit(0)
		}
		var result any
		if m.Method == "initialize" {
			_ = c.send(map[string]any{"jsonrpc": "2.0", "id": "config", "method": "workspace/configuration", "params": map[string]any{"items": []any{map[string]any{}}}})
			response, e := readFrame(c.r)
			if e != nil || string(response.Result) != "[{}]" {
				os.Exit(3)
			}
			result = map[string]any{"capabilities": map[string]any{}}
		} else if m.Method == "textDocument/documentSymbol" {
			result = []any{map[string]any{"name": "Hello", "kind": 12}}
		}
		if err = c.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result}); err != nil {
			os.Exit(4)
		}
	}
}

func TestLSPQueryLifecycleAndTimeout(t *testing.T) {
	w := t.TempDir()
	s, err := rt.NewProcessSandbox(w, rt.SandboxConfig{Mode: "off"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := LSPConfig{Command: bin, Args: []string{"-test.run=^TestLSPHelper$", "--", "lsp-fixture"}, Timeout: 3 * time.Second}
	result, err := Query(context.Background(), s, cfg, w, filepath.Join(w, "a.go"), "package p", "textDocument/documentSymbol", 0, 0)
	if err != nil || !bytes.Contains(result, []byte("Hello")) {
		t.Fatalf("result=%s err=%v", result, err)
	}
	cfg.Args[len(cfg.Args)-1] = "lsp-fixture-silent"
	cfg.Timeout = 100 * time.Millisecond
	start := time.Now()
	if _, err = Query(context.Background(), s, cfg, w, filepath.Join(w, "a.go"), "package p", "textDocument/documentSymbol", 0, 0); err == nil {
		t.Fatal("hung server accepted")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("server not reaped on timeout")
	}
}

func TestRealGopls(t *testing.T) {
	if os.Getenv("CODECLAW_NATIVE_TEST") != "1" {
		t.Skip("set CODECLAW_NATIVE_TEST=1 for real gopls")
	}
	bin, err := exec.LookPath("gopls")
	if err != nil {
		t.Fatal(err)
	}
	w := t.TempDir()
	w, _ = filepath.EvalSymlinks(w)
	source := "package demo\nfunc Hello() int { return 42 }\nfunc Use() int { return Hello() }\n"
	if err = os.WriteFile(filepath.Join(w, "go.mod"), []byte("module example.com/demo\ngo 1.23\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(w, "demo.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	goRoot, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		t.Fatal(err)
	}
	s, err := rt.NewProcessSandbox(w, rt.SandboxConfig{ReadRoots: []string{runtime.GOROOT(), strings.TrimSpace(string(goRoot))}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	raw, err := Query(context.Background(), s, LSPConfig{Command: bin, Timeout: 30 * time.Second}, w, filepath.Join(w, "demo.go"), source, "textDocument/documentSymbol", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var symbols []map[string]any
	if err = json.Unmarshal(raw, &symbols); err != nil {
		t.Fatal(err)
	}
	if len(symbols) != 2 || symbols[0]["name"] != "Hello" {
		t.Fatal(fmt.Sprint(symbols))
	}
	position := strings.Index(strings.Split(source, "\n")[2], "Hello")
	raw, err = Query(context.Background(), s, LSPConfig{Command: bin, Timeout: 30 * time.Second}, w, filepath.Join(w, "demo.go"), source, "textDocument/definition", 2, position)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("demo.go")) || !bytes.Contains(raw, []byte(`"line":1`)) {
		t.Fatalf("wrong definition: %s", raw)
	}
}
