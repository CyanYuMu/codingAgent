package eval

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"einoclaw-build/internal/agent"
	"einoclaw-build/internal/memory"
	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
	"einoclaw-build/internal/tool"
)

// evalInstruction 评测用 agent 的系统指令（非空，避免空 content 被模型拒绝）。
const evalInstruction = "你是一个编程智能体，完成用户任务，必要时用工具。"

// Fixture 一个评测夹具：prompt + 起始文件 + 期望文件。
type Fixture struct {
	Name     string
	Prompt   string
	Input    map[string]string // 相对路径 → 内容
	Expected map[string]string // 相对路径 → 内容
}

// Result 一次评测的结果。
type Result struct {
	Name   string
	Pass   bool
	Detail string
}

// Run 在隔离 workdir 里跑 agent，比较 expected 文件。
// 用 os.Chdir 切到 workdir，让 write_file/read_file/glob 的相对路径也落到隔离目录。
func Run(ctx context.Context, fx Fixture, m model.Model, mem memory.Recaller) Result {
	workdir, err := os.MkdirTemp("", "eval-*")
	if err != nil {
		return Result{Name: fx.Name, Pass: false, Detail: "workdir: " + err.Error()}
	}
	defer os.RemoveAll(workdir)

	oldCwd, _ := os.Getwd()
	if err := os.Chdir(workdir); err != nil {
		return Result{Name: fx.Name, Pass: false, Detail: err.Error()}
	}
	defer os.Chdir(oldCwd)

	for path, content := range fx.Input {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return Result{Name: fx.Name, Pass: false, Detail: err.Error()}
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			return Result{Name: fx.Name, Pass: false, Detail: err.Error()}
		}
	}

	bash := runtime.NewBash(".")
	registry := tool.NewRegistry()
	for _, t := range tool.Builtins(bash) {
		registry.Register(t)
	}
	ag := agent.New(fx.Name, evalInstruction, m, registry, permission.ModeYolo, nil, mem)

	var text string
	for ev := range ag.Run(ctx, []message.Message{message.NewUserMessage(fx.Prompt)}) {
		if ev.Type == agent.EventMessageEnd {
			text = textOf(ev.Ended.Message)
		}
	}

	if diffs := verify(".", fx.Expected); len(diffs) > 0 {
		return Result{Name: fx.Name, Pass: false, Detail: "diff: " + strings.Join(diffs, ", ")}
	}
	return Result{Name: fx.Name, Pass: true, Detail: text}
}

// verify 逐文件比较，返回不一致的文件路径。
// 去末尾空白后比较（容忍 agent 输出带末尾换行/空格），保留行内结构与前导缩进。
func verify(workdir string, expected map[string]string) []string {
	var diffs []string
	for path, want := range expected {
		got, err := os.ReadFile(filepath.Join(workdir, path))
		if err != nil || strings.TrimRight(string(got), " \t\r\n") != strings.TrimRight(want, " \t\r\n") {
			diffs = append(diffs, path)
		}
	}
	return diffs
}

// LoadFixture 从目录加载夹具：prompt.md + input/ + expected/。
func LoadFixture(dir string) (Fixture, error) {
	prompt, err := os.ReadFile(filepath.Join(dir, "prompt.md"))
	if err != nil {
		return Fixture{}, err
	}
	input, err := readFiles(filepath.Join(dir, "input"))
	if err != nil {
		return Fixture{}, err
	}
	expected, err := readFiles(filepath.Join(dir, "expected"))
	if err != nil {
		return Fixture{}, err
	}
	return Fixture{Name: filepath.Base(dir), Prompt: string(prompt), Input: input, Expected: expected}, nil
}

func readFiles(dir string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[rel] = string(b)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return out, nil // 目录不存在 = 空（夹具可不含 input/）
}

func textOf(m message.Message) string {
	var sb strings.Builder
	for _, b := range m.Blocks {
		if b.Kind == message.BlockText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}
