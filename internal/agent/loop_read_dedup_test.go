package agent

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
	"einoclaw-build/internal/tool"
)

// TestReadFileDedupSmoke 全链路冒烟：真循环 + 真 Builtins(read_file) + 脚本化模型。
// 模型连续两次 read_file 同一未变更文件 → 第二次工具结果必须是「未变更」提示而非全文，
// 且提示后的下一步模型调用能正常继续。这正是 P10.4 Task 13 的验收行为（会话内不重复读）。
func TestReadFileDedupSmoke(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	arg := `{"file_path":` + strconv.Quote(path) + `}`
	fm := &fakeModel{steps: []func() (model.ModelStream, error){
		callStep("c1", "read_file", arg),
		callStep("c2", "read_file", arg),
		textStep("done"),
	}}
	cc := NewMemoryContext(nil)
	_ = cc.Record(message.NewUserMessage("read the file twice"), model.Usage{})

	reg := tool.NewRegistry()
	for _, tl := range tool.Builtins(runtime.NewBash(dir), nil) {
		reg.Register(tl)
	}
	a := New("t", fm, reg, tool.NewExecutor(reg, permission.ModeYolo, nil), cc)
	a.retryBase = 0

	var results []string
	for e := range a.Run(context.Background(), nil) {
		switch e.Type {
		case EventToolEnd:
			results = append(results, e.ToolEnd.Content)
		case EventError:
			t.Fatalf("意外错误：%v", e.Err)
		}
	}
	if len(results) != 2 {
		t.Fatalf("应有 2 次工具结果，got %d：%v", len(results), results)
	}
	if !strings.Contains(results[0], "alpha") || strings.Contains(results[0], "未变更") {
		t.Fatalf("首次读应返回内容：%q", results[0])
	}
	if !strings.Contains(results[1], "未变更") || strings.Contains(results[1], "alpha") {
		t.Fatalf("第二次读应返回未变更提示：%q", results[1])
	}
	// 循环应正常收尾（模型最后一轮 textStep 被执行）
	if len(fm.calls) != 3 {
		t.Fatalf("模型应被调用 3 次（读×2 + 收尾），got %d", len(fm.calls))
	}
	validateToolPairing(t, mustBuild(t, cc))
}

// mustBuild 取 Context 当前消息（冒烟断言用）。
func mustBuild(t *testing.T, cc Context) []message.Message {
	t.Helper()
	msgs, err := cc.Build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}
