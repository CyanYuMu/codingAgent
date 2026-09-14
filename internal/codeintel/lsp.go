package codeintel

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"einoclaw-build/internal/runtime"
)

type LSPConfig struct {
	Command    string        `yaml:"command"`
	Args       []string      `yaml:"args"`
	LanguageID string        `yaml:"language_id"`
	Timeout    time.Duration `yaml:"timeout"`
}

func fileURI(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}

// Query starts a bounded stdio LSP instance with read-only project access.
// User positions are converted by the caller to zero-based UTF-16 positions.
func Query(ctx context.Context, sandbox *runtime.ProcessSandbox, cfg LSPConfig, root, path, source, method string, line, character int) (json.RawMessage, error) {
	if cfg.Command == "" {
		return nil, fmt.Errorf("LSP is not configured; set user-level lsp.command (e.g. gopls)")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.Timeout > 2*time.Minute {
		cfg.Timeout = 2 * time.Minute
	}
	if cfg.LanguageID == "" {
		cfg.LanguageID = "go"
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	cmd, err := sandbox.ReadOnly().Command(ctx, root, cfg.Command, cfg.Args...)
	if err != nil {
		return nil, err
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		return nil, err
	}
	stderr := runtime.NewSink(2000, 2000)
	defer stderr.Close()
	cmd.Stderr = stderr
	if err = cmd.Start(); err != nil {
		in.Close()
		out.Close()
		return nil, err
	}
	defer func() { in.Close(); cancel(); _ = cmd.Wait() }()
	c := &rpcClient{r: bufio.NewReader(out), w: in}
	init, err := c.call("initialize", map[string]any{"processId": nil, "rootUri": fileURI(root), "workspaceFolders": []any{map[string]any{"uri": fileURI(root), "name": filepath.Base(root)}}, "capabilities": map[string]any{"workspace": map[string]any{"configuration": true}, "general": map[string]any{"positionEncodings": []string{"utf-16"}}}})
	if err != nil {
		return nil, fmt.Errorf("LSP initialize: %w; stderr: %s", err, stderr.Result())
	}
	var capabilities struct {
		Capabilities struct {
			PositionEncoding string `json:"positionEncoding"`
		} `json:"capabilities"`
	}
	if err = json.Unmarshal(init, &capabilities); err != nil {
		return nil, err
	}
	if enc := capabilities.Capabilities.PositionEncoding; enc != "" && enc != "utf-16" {
		return nil, fmt.Errorf("unsupported LSP position encoding: %s", enc)
	}
	if err = c.notify("initialized", map[string]any{}); err != nil {
		return nil, err
	}
	if err = c.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": fileURI(path), "languageId": cfg.LanguageID, "version": 1, "text": source}}); err != nil {
		return nil, err
	}
	params := map[string]any{"textDocument": map[string]any{"uri": fileURI(path)}}
	if method != "textDocument/documentSymbol" {
		params["position"] = map[string]int{"line": line, "character": character}
	}
	if method == "textDocument/references" {
		params["context"] = map[string]bool{"includeDeclaration": true}
	}
	result, err := c.call(method, params)
	if err != nil {
		return nil, fmt.Errorf("LSP %s: %w; stderr: %s", method, err, stderr.Result())
	}
	// Server gets a normal shutdown handshake; the overall deadline bounds it.
	if _, err = c.call("shutdown", nil); err == nil {
		_ = c.notify("exit", nil)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return result, nil
}

const maxFrameBytes = 16 << 20

type rpcMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}
type rpcClient struct {
	r    *bufio.Reader
	w    io.Writer
	next int
}

func (c *rpcClient) send(value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(b) > maxFrameBytes {
		return fmt.Errorf("LSP request exceeds frame limit")
	}
	_, err = fmt.Fprintf(c.w, "Content-Length: %d\r\n\r\n%s", len(b), b)
	return err
}
func (c *rpcClient) notify(method string, params any) error {
	return c.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}
func (c *rpcClient) call(method string, params any) (json.RawMessage, error) {
	c.next++
	id := c.next
	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for count := 0; count < 10000; count++ {
		m, err := readFrame(c.r)
		if err != nil {
			return nil, err
		}
		if m.Method != "" {
			if len(m.ID) > 0 {
				response := map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": nil}
				switch m.Method {
				case "workspace/configuration":
					var p struct {
						Items []json.RawMessage `json:"items"`
					}
					if err := json.Unmarshal(m.Params, &p); err != nil {
						return nil, err
					}
					items := make([]any, len(p.Items))
					for i := range items {
						items[i] = map[string]any{}
					}
					response["result"] = items
				case "window/workDoneProgress/create":
				case "workspace/applyEdit":
					response["result"] = map[string]any{"applied": false, "failureReason": "read-only language service; submit workspace changes through apply_changes"}
				default:
					delete(response, "result")
					response["error"] = map[string]any{"code": -32601, "message": "client method unsupported"}
				}
				if err = c.send(response); err != nil {
					return nil, err
				}
			}
			continue
		}
		if string(m.ID) != strconv.Itoa(id) {
			return nil, fmt.Errorf("unexpected LSP response id %s", m.ID)
		}
		if m.Error != nil {
			return nil, fmt.Errorf("RPC %d: %s", m.Error.Code, m.Error.Message)
		}
		return m.Result, nil
	}
	return nil, fmt.Errorf("excessive LSP notifications")
}
func readFrame(r *bufio.Reader) (rpcMessage, error) {
	var m rpcMessage
	n := -1
	headers := 0
	for {
		line, err := r.ReadSlice('\n')
		if err != nil {
			return m, err
		}
		headers += len(line)
		if headers > 8192 {
			return m, fmt.Errorf("LSP headers too large")
		}
		if string(line) == "\r\n" {
			break
		}
		k, v, ok := strings.Cut(strings.TrimSpace(string(line)), ":")
		if !ok {
			return m, fmt.Errorf("malformed LSP header")
		}
		if strings.EqualFold(k, "Content-Length") {
			if n != -1 {
				return m, fmt.Errorf("duplicate Content-Length")
			}
			n, err = strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return m, err
			}
		}
	}
	if n <= 0 || n > maxFrameBytes {
		return m, fmt.Errorf("invalid LSP Content-Length: %d", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return m, err
	}
	err := json.Unmarshal(b, &m)
	return m, err
}
