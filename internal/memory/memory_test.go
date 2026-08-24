package memory

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "m.db"), ScopeProject, "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func remember(t *testing.T, s *Store, content string, o Opts) int64 {
	t.Helper()
	id, _, err := s.Remember(content, o)
	if err != nil {
		t.Fatalf("Remember(%q): %v", content, err)
	}
	return id
}

func TestScoreMultiSignal(t *testing.T) {
	now := time.Now().Unix()
	fresh := Memory{Importance: 0.9, Veracity: 1, UpdatedAt: now, Scope: ScopeProject}
	stale := Memory{Importance: 0.2, Veracity: 1, UpdatedAt: now - 30*24*3600, Scope: ScopeProject}
	if scoreMemory(fresh, 0.5, now) <= scoreMemory(stale, 0.5, now) {
		t.Fatal("重要且新的记忆应排在前面")
	}
	global := fresh
	global.Scope = ScopeGlobal
	if scoreMemory(global, 0.5, now) >= scoreMemory(fresh, 0.5, now) {
		t.Fatal("同分时项目内记忆应优先于全局记忆")
	}
	doubted := fresh
	doubted.Veracity = 0.1
	if scoreMemory(doubted, 0.5, now) >= scoreMemory(fresh, 0.5, now) {
		t.Fatal("可信度应能压低分数")
	}
}

func TestOpenCreatesV2Schema(t *testing.T) {
	s := openTest(t)
	for _, table := range []string{"memories", "memories_fts"} {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name=?`, table).Scan(&n); err != nil || n == 0 {
			t.Fatalf("缺表 %s（err=%v）", table, err)
		}
	}
}

func TestMigratesLegacyWorkingMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE working_memory (id INTEGER PRIMARY KEY AUTOINCREMENT, content TEXT NOT NULL,
			source TEXT NOT NULL DEFAULT 'user', veracity REAL NOT NULL DEFAULT 1.0,
			importance REAL NOT NULL DEFAULT 0.5, memory_type TEXT NOT NULL DEFAULT 'fact',
			created_at INTEGER NOT NULL);
		INSERT INTO working_memory (content, created_at) VALUES ('构建命令是 go build ./...', 1000);
		INSERT INTO working_memory (content, created_at) VALUES ('用户偏好中文回复', 1001);
		INSERT INTO working_memory (content, created_at) VALUES ('测试用 go test ./...', 1002);`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path, ScopeProject, "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if n, _ := s.Count(); n != 3 {
		t.Fatalf("迁移后条数 = %d, want 3", n)
	}
	got, err := s.Recall("构建命令是什么？", 5)
	if err != nil || len(got) == 0 {
		t.Fatalf("迁移后应能召回：%v %v", got, err)
	}
	// 旧表保留
	var legacy int
	if err := s.db.QueryRow(`SELECT count(*) FROM working_memory`).Scan(&legacy); err != nil || legacy != 3 {
		t.Fatalf("旧表不该被删：%d %v", legacy, err)
	}
	// 幂等：再开一次不重复搬
	s2, err := Open(path, ScopeProject, "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if n, _ := s2.Count(); n != 3 {
		t.Fatalf("重复迁移了：%d", n)
	}
}

func TestRememberUpsertByKey(t *testing.T) {
	s := openTest(t)
	id1 := remember(t, s, "构建命令是 go build", Opts{Key: "build-cmd"})
	first, _ := s.Get(id1)

	time.Sleep(1100 * time.Millisecond) // updated_at 是秒级
	id2, updated, err := s.Remember("构建命令是 env -u GOROOT go build ./...", Opts{Key: "build-cmd"})
	if err != nil || !updated || id2 != id1 {
		t.Fatalf("同 key 应覆盖同一行：id=%d updated=%v err=%v", id2, updated, err)
	}
	if n, _ := s.Count(); n != 1 {
		t.Fatalf("条数 = %d, want 1", n)
	}
	got, _ := s.Get(id1)
	if !strings.Contains(got.Content, "GOROOT") {
		t.Fatalf("内容没更新：%q", got.Content)
	}
	if got.CreatedAt != first.CreatedAt {
		t.Fatal("created_at 不该变")
	}
	if got.UpdatedAt <= first.UpdatedAt {
		t.Fatalf("updated_at 应前进：%d → %d", first.UpdatedAt, got.UpdatedAt)
	}
}

func TestRememberNearDuplicateUpdates(t *testing.T) {
	s := openTest(t)
	remember(t, s, "用户偏好中文回复", Opts{})
	_, updated, err := s.Remember("用户偏好中文回复。", Opts{})
	if err != nil || !updated {
		t.Fatalf("近重复应更新已有条目：updated=%v err=%v", updated, err)
	}
	if n, _ := s.Count(); n != 1 {
		t.Fatalf("同一偏好说两次应只剩一条，实际 %d 条", n)
	}
}

func TestRememberDistinctFactsCoexist(t *testing.T) {
	s := openTest(t)
	remember(t, s, "构建命令是 go build ./...", Opts{})
	remember(t, s, "用户住在杭州，习惯晚上工作", Opts{})
	if n, _ := s.Count(); n != 2 {
		t.Fatalf("不同事实不该被合并：%d 条", n)
	}
}

func TestRememberRejectsSecrets(t *testing.T) {
	s := openTest(t)
	for _, bad := range []string{
		"API key 是 sk-abcdefghijklmnopqrstuvwx",
		"-----BEGIN OPENSSH PRIVATE KEY-----",
		"password = hunter2hunter2hunter2",
	} {
		if _, _, err := s.Remember(bad, Opts{}); err == nil {
			t.Fatalf("应拒绝写入密钥：%q", bad)
		}
	}
	if n, _ := s.Count(); n != 0 {
		t.Fatalf("被拒的内容不该落库：%d 条", n)
	}
}

func TestRememberTruncatesLongContent(t *testing.T) {
	s := openTest(t)
	id := remember(t, s, strings.Repeat("很长的记忆", 1000), Opts{})
	got, _ := s.Get(id)
	if len([]rune(got.Content)) > maxContentRunes+1 {
		t.Fatalf("超长内容应截断：%d runes", len([]rune(got.Content)))
	}
	if !strings.Contains(got.Tags, "truncated") {
		t.Fatalf("截断应留标记：%q", got.Tags)
	}
}

func TestForgetKeepsRowButExcludesFromRecall(t *testing.T) {
	s := openTest(t)
	id := remember(t, s, "构建命令是 go build ./...", Opts{Key: "build-cmd"})
	if err := s.Forget("build-cmd", "改用 makefile 了"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Recall("构建命令", 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range got {
		if m.ID == id {
			t.Fatal("失效的记忆不该被召回")
		}
	}
	row, err := s.Get(id)
	if err != nil || row.Veracity != 0 || !strings.Contains(row.Why, "makefile") {
		t.Fatalf("行应保留并记下原因：%+v err=%v", row, err)
	}
}

func TestForgetUnknownKeyErrors(t *testing.T) {
	if err := openTest(t).Forget("nope", ""); err == nil {
		t.Fatal("未知 key 应报错")
	}
}

func TestMaxPerScopeEvictsLowest(t *testing.T) {
	s := openTest(t)
	s.SetMaxPerScope(3)
	remember(t, s, "低价值的临时事实一", Opts{Importance: 0.1})
	keep1 := remember(t, s, "很重要的项目约定二", Opts{Importance: 0.9})
	keep2 := remember(t, s, "同样重要的决策三", Opts{Importance: 0.9})
	keep3 := remember(t, s, "刚刚发生的事情四", Opts{Importance: 0.9})
	if n, _ := s.Count(); n != 3 {
		t.Fatalf("超限后应淘汰到上限：%d", n)
	}
	for _, id := range []int64{keep1, keep2, keep3} {
		if _, err := s.Get(id); err != nil {
			t.Fatalf("高分记忆不该被淘汰：id=%d err=%v", id, err)
		}
	}
}

func TestRecallWithPunctuationQuery(t *testing.T) {
	s := openTest(t)
	remember(t, s, "这个项目的构建命令是 env -u GOROOT go build ./...", Opts{})
	got, err := s.Recall("这个项目的构建命令是什么？(build)", 5)
	if err != nil {
		t.Fatalf("含标点的问句不该让 FTS 报错：%v", err)
	}
	if len(got) == 0 {
		t.Fatal("应能召回构建命令")
	}
}

func TestRecallFallsBackWithoutTerms(t *testing.T) {
	s := openTest(t)
	remember(t, s, "用户偏好中文回复", Opts{Importance: 0.9})
	got, err := s.Recall("？？", 5)
	if err != nil || len(got) != 1 {
		t.Fatalf("无实词时应兜底召回：%v %v", got, err)
	}
}

func TestRecallWritesBackAccess(t *testing.T) {
	s := openTest(t)
	id := remember(t, s, "这个项目用 deepseek 模型做测试", Opts{})
	if _, err := s.Recall("deepseek 模型", 5); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(id)
	if got.AccessCount != 1 || got.LastAccessed == 0 {
		t.Fatalf("召回应回写访问信息：count=%d last=%d", got.AccessCount, got.LastAccessed)
	}
}

func TestGetMissingReturnsNoRows(t *testing.T) {
	if _, err := openTest(t).Get(999); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want ErrNoRows", err)
	}
}

func TestClearProjectDoesNotTouchOtherScope(t *testing.T) {
	dir := t.TempDir()
	proj, err := Open(filepath.Join(dir, "p.db"), ScopeProject, "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	defer proj.Close()
	global, err := Open(filepath.Join(dir, "g.db"), ScopeGlobal, "")
	if err != nil {
		t.Fatal(err)
	}
	defer global.Close()

	remember(t, proj, "项目约定：提交信息用中文", Opts{})
	remember(t, global, "用户偏好中文回复", Opts{})
	if err := proj.ClearProject(); err != nil {
		t.Fatal(err)
	}
	if n, _ := proj.Count(); n != 0 {
		t.Fatalf("项目库应被清空：%d", n)
	}
	if n, _ := global.Count(); n != 1 {
		t.Fatalf("全局库不该被动：%d", n)
	}
}

func TestUnionMergesAndDedupes(t *testing.T) {
	dir := t.TempDir()
	proj, _ := Open(filepath.Join(dir, "p.db"), ScopeProject, "proj-1")
	defer proj.Close()
	global, _ := Open(filepath.Join(dir, "g.db"), ScopeGlobal, "")
	defer global.Close()

	remember(t, proj, "用户偏好中文回复", Opts{})
	remember(t, global, "用户偏好中文回复", Opts{})
	remember(t, global, "用户在多个项目里都用 Go 语言", Opts{})

	got, err := Union(proj, global).Recall("用户偏好中文回复", 5)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, m := range got {
		seen[m.Content]++
	}
	if seen["用户偏好中文回复"] != 1 {
		t.Fatalf("跨库重复内容应去重：%+v", got)
	}
	if got[0].Scope != ScopeProject {
		t.Fatalf("同内容应优先取项目库的：%+v", got[0])
	}
}

func TestUnionSurvivesOneBadStore(t *testing.T) {
	s := openTest(t)
	remember(t, s, "用户偏好中文回复", Opts{})
	broken := openTest(t)
	broken.Close() // 关掉的库查询会报错
	got, err := Union(s, broken).Recall("中文回复", 5)
	if err != nil || len(got) != 1 {
		t.Fatalf("一个库坏掉不该拖垮召回：%v %v", got, err)
	}
}

func TestRecallFallsBackWhenFTSMissesParaphrase(t *testing.T) {
	s := openTest(t)
	remember(t, s, "用户偏好中文回复", Opts{Importance: 0.9})
	// trigram 匹配不到（"语言偏"/"用户的" 都不在记忆里），但仍应给出候选
	got, err := s.Recall("你记得用户的语言偏好吗", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Content != "用户偏好中文回复" {
		t.Fatalf("同义改写落空时应退回重要性排序：%+v", got)
	}
}
