package context

import (
	"strings"
	"testing"

	"einoclaw-build/internal/message"
	"einoclaw-build/internal/model"
	"einoclaw-build/internal/session"
)

// fileConv 造一段带文件工具调用的对话：read/write 交错（压缩安全切点需要 user 消息穿插），
// 外加非文件工具与会话内 URL 干扰。最后活动：根 main.go → cmd/agent/main.go → compaction.go → manager.go。
func fileConv() []message.Message {
	return []message.Message{
		ctxMsg(message.RoleUser, "u1"),
		callMsg("c1", "read_file", `{"file_path":"internal/context/manager.go"}`),
		callMsg("c2", "write_file", `{"file_path":"internal/context/manager.go","content":"x"}`), // RW
		ctxMsg(message.RoleUser, "u2"),
		callMsg("c3", "read_file", `{"file_path":"internal/context/compaction.go"}`), // Read
		ctxMsg(message.RoleUser, "u3"),
		callMsg("c4", "write_file", `{"file_path":"cmd/agent/main.go","content":"y"}`), // Write
		ctxMsg(message.RoleUser, "u4"),
		callMsg("c5", "read_file", `{"file_path":"artifact://3"}`), // 会话内 URL：忽略
		callMsg("c6", "bash", `{"command":"ls"}`),                  // 非文件工具
		callMsg("c7", "read_file", `{"file_path":"main.go"}`),      // 根下文件
		ctxMsg(message.RoleUser, "u5"),
	}
}

func TestFileOpsTree(t *testing.T) {
	tree := FileOpsTree(fileConv(), 20)
	for _, want := range []string{
		"<files>", "</files>",
		"internal/context/",
		"manager.go (RW)",
		"compaction.go (Read)",
		"cmd/agent/main.go (Write)", // 单文件目录：整行路径
		"main.go (Read)",            // 根下文件
	} {
		if !strings.Contains(tree, want) {
			t.Errorf("tree 缺少 %q:\n%s", want, tree)
		}
	}
	if strings.Contains(tree, "artifact") {
		t.Errorf("会话内 URL 不应进文件树:\n%s", tree)
	}
	// 排序：最近活动的文件/目录在前（根 main.go 最后活动 → 在 internal/context/ 组之前）
	if strings.Index(tree, "main.go (Read)") > strings.Index(tree, "internal/context/") {
		t.Errorf("应按最近活动排序:\n%s", tree)
	}
}

func TestFileOpsTreeLimitElides(t *testing.T) {
	var msgs []message.Message
	for i := range 25 { // fa.go 最老 … fy.go 最新
		msgs = append(msgs, callMsg("c", "read_file", `{"file_path":"d/f`+string(rune('a'+i))+`.go"}`))
	}
	tree := FileOpsTree(msgs, 20)
	if !strings.Contains(tree, "[…5 files elided…]") {
		t.Fatalf("超限应省略计数:\n%s", tree)
	}
	if strings.Contains(tree, "fa.go") { // 最老的 5 个被省略（按最近活动排序保留新的）
		t.Fatalf("最老文件应被省略:\n%s", tree)
	}
}

func TestFileOpsTreeEmptyWhenNoFileActivity(t *testing.T) {
	if tree := FileOpsTree([]message.Message{ctxMsg(message.RoleUser, "hi")}, 20); tree != "" {
		t.Fatalf("无文件活动应返回空串，got %q", tree)
	}
}

func TestSummaryIncludesFilesTag(t *testing.T) {
	s, _ := session.New("s1", &session.MemoryStorage{})
	fs := &fakeSummarizer{out: "S"}
	cm := New(s, fs, 1000, 6, nil)
	for _, m := range fileConv() {
		_ = cm.Record(m, model.Usage{})
	}
	if method, err := cm.Compact(t.Context()); err != nil || method != compactSummary {
		t.Fatalf("method=%q err=%v", method, err)
	}
	es, _ := s.Entries()
	var comp *session.Entry
	for i := len(es) - 1; i >= 0; i-- {
		if es[i].Type == session.EntryCompaction {
			comp = &es[i]
			break
		}
	}
	if comp == nil || !strings.Contains(comp.Compaction.Summary, "<files>") {
		t.Fatalf("摘要应附 <files> 树：%+v", comp)
	}
	// Build 回放里摘要消息同样带树
	built, _ := cm.Build(t.Context())
	if !strings.Contains(built[0].Blocks[0].Text, "<files>") {
		t.Fatalf("回放摘要缺 <files>：%q", built[0].Blocks[0].Text)
	}
}

func TestPostCompactionRecentFiles(t *testing.T) {
	s, _ := session.New("s1", &session.MemoryStorage{})
	fs := &fakeSummarizer{out: "S"}
	cm := New(s, fs, 1000, 6, nil)
	for _, m := range fileConv() {
		_ = cm.Record(m, model.Usage{})
	}
	if _, err := cm.Compact(t.Context()); err != nil {
		t.Fatal(err)
	}
	ms, _ := s.Replay()
	last := ms[len(ms)-1]
	if last.Role != message.RoleUser || !strings.Contains(last.Blocks[0].Text, "<recent-files>") {
		t.Fatalf("压缩后应有 <recent-files> 恢复消息，got %+v", last)
	}
	if !strings.Contains(last.Blocks[0].Text, "main.go") {
		t.Fatalf("最近文件清单应含最后活动的文件：%q", last.Blocks[0].Text)
	}
	// 恢复消息之后继续对话：压缩边界不受影响
	_ = s.Append(ctxMsg(message.RoleUser, "next"))
	ms2, _ := s.Replay()
	if len(ms2) != len(ms)+1 {
		t.Fatalf("压缩后应能继续追加：%d → %d", len(ms), len(ms2))
	}
}
