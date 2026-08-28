package memory

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，注册为 "sqlite"
)

// 作用域：项目记忆落项目桶，全局记忆落 <Home>/memory/global.db。
// 「这个项目的构建命令」属于项目，「用户偏好中文回复」属于全局。
const (
	ScopeProject = "project"
	ScopeGlobal  = "global"
)

const (
	maxContentRunes    = 2000 // 单条记忆上限：再长就不是"记忆"而是文档，该写文件
	defaultMaxPerScope = 500
	nearDupThreshold   = 0.85 // trigram 相似度到这个程度视为同一条事实的不同说法
	nearDupCandidates  = 5
)

// Memory 一条记忆。
type Memory struct {
	ID           int64
	Scope        string
	ProjectID    string
	Kind         string // user | feedback | project | reference | decision | fact
	Key          string // 稳定键：有 key 就按 key 覆盖，没有就靠近重复检测
	Content      string
	Why          string
	Source       string // user | model | harness
	Veracity     float64
	Importance   float64
	CreatedAt    int64
	UpdatedAt    int64
	LastAccessed int64
	AccessCount  int
	SupersededBy int64
	Tags         string
}

// MemoryType 兼容旧字段名（渲染与旧调用点用）。
func (m Memory) MemoryType() string { return m.Kind }

// Opts 写入记忆时的可选参数。
// 没有 Scope 字段：作用域是**库的属性**而不是写入参数——调用方选哪个库，就写哪个作用域。
// （曾经允许按参数指定，结果是"降级到项目库却仍标 global"的孤儿行：谁都查不到。）
type Opts struct {
	Kind, Key, Why, Source, Tags string
	Veracity, Importance         float64
}

// Recaller 是召回能力的抽象（agent 依赖它，测试可注入假实现）。
type Recaller interface {
	Recall(query string, topK int) ([]Memory, error)
}

// Store 是一个作用域的 SQLite 记忆库。
type Store struct {
	db          *sql.DB
	scope       string
	projectID   string
	maxPerScope int
}

// Open 打开/创建一个作用域的记忆库；scope 为空按 project 处理。
func Open(path, scope, projectID string) (*Store, error) {
	if scope == "" {
		scope = ScopeProject
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, scope: scope, projectID: projectID, maxPerScope: defaultMaxPerScope}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Scope / ProjectID 返回本库的作用域标识。
func (s *Store) Scope() string     { return s.scope }
func (s *Store) ProjectID() string { return s.projectID }

// SetMaxPerScope 覆盖条数上限（0 = 不限）。
func (s *Store) SetMaxPerScope(n int) { s.maxPerScope = n }

const schemaV2 = `
CREATE TABLE IF NOT EXISTS memories (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  scope         TEXT NOT NULL DEFAULT 'project',
  project_id    TEXT NOT NULL DEFAULT '',
  kind          TEXT NOT NULL DEFAULT 'fact',
  key           TEXT,
  content       TEXT NOT NULL,
  why           TEXT,
  source        TEXT NOT NULL DEFAULT 'user',
  veracity      REAL NOT NULL DEFAULT 1.0,
  importance    REAL NOT NULL DEFAULT 0.5,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  last_accessed INTEGER,
  access_count  INTEGER NOT NULL DEFAULT 0,
  superseded_by INTEGER,
  tags          TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS memories_key ON memories(scope, project_id, key) WHERE key IS NOT NULL AND key <> '';
CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(content, tags, tokenize='trigram');
`

func (s *Store) init() error {
	if _, err := s.db.Exec(schemaV2); err != nil {
		return err
	}
	if _, err := s.db.Exec(fileNotesSchema); err != nil {
		return err
	}
	return s.migrateLegacy()
}

// migrateLegacy 把 P5 的 working_memory 搬进 memories。幂等：只在 memories 为空时搬一次；
// 旧表保留不删——记忆是用户数据，出问题要能回查。
func (s *Store) migrateLegacy() error {
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM memories`).Scan(&n); err != nil || n > 0 {
		return err
	}
	var legacy int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='working_memory'`).Scan(&legacy); err != nil || legacy == 0 {
		return err
	}
	rows, err := s.db.Query(`SELECT id, content, source, veracity, importance, memory_type, created_at FROM working_memory`)
	if err != nil {
		return err
	}
	defer rows.Close()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for rows.Next() {
		var m Memory
		if err := rows.Scan(&m.ID, &m.Content, &m.Source, &m.Veracity, &m.Importance, &m.Kind, &m.CreatedAt); err != nil {
			return err
		}
		res, err := tx.Exec(`INSERT INTO memories (scope, project_id, kind, content, source, veracity, importance, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?)`,
			s.scope, s.projectID, m.Kind, m.Content, m.Source, m.Veracity, m.Importance, m.CreatedAt, m.CreatedAt)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO memories_fts(rowid, content, tags) VALUES (?,?,'')`, id, m.Content); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return tx.Commit()
}

// secretPatterns 命中即整条拒绝：半条密钥仍是泄漏面，所以不做部分打码。
var secretPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"OpenAI 风格 key", regexp.MustCompile(`sk-[A-Za-z0-9_\-]{16,}`)},
	{"GitHub token", regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{20,}`)},
	{"AWS access key", regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
	{"私钥", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"Slack token", regexp.MustCompile(`xox[baprs]-[A-Za-z0-9\-]{10,}`)},
	{"key=value 形式的凭据", regexp.MustCompile(`(?i)\b(api[_-]?key|access[_-]?token|secret|password|passwd)\b\s*[:=]\s*\S{12,}`)},
}

func detectSecret(s string) string {
	for _, p := range secretPatterns {
		if p.re.MatchString(s) {
			return p.name
		}
	}
	return ""
}

// Remember 写一条记忆，返回 id 与「是否更新了已有条目」。
// 有 key 按 key 覆盖；没有 key 就做近重复检测——同一件事说三遍不该占三个位置。
func (s *Store) Remember(content string, o Opts) (int64, bool, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return 0, false, errors.New("content 不能为空")
	}
	if what := detectSecret(content); what != "" {
		return 0, false, fmt.Errorf("内容疑似包含%s，拒绝写入记忆；记引用（配置项名、文件路径）而不是值", what)
	}
	if r := []rune(content); len(r) > maxContentRunes {
		content = string(r[:maxContentRunes]) + "…"
		o.Tags = strings.TrimPrefix(o.Tags+",truncated", ",")
	}
	o = s.withDefaults(o)
	now := time.Now().Unix()

	if o.Key != "" {
		if id, err := s.idByKey(o.Key, s.scope); err != nil {
			return 0, false, err
		} else if id > 0 {
			return id, true, s.update(id, content, o, now)
		}
	} else if id, err := s.nearDuplicate(content); err != nil {
		return 0, false, err
	} else if id > 0 {
		return id, true, s.update(id, content, o, now)
	}

	id, err := s.insert(content, o, now)
	if err != nil {
		return 0, false, err
	}
	return id, false, s.evictOverflow()
}

func (s *Store) withDefaults(o Opts) Opts {
	if o.Kind == "" {
		o.Kind = "fact"
	}
	if o.Source == "" {
		o.Source = "user"
	}
	if o.Veracity == 0 {
		o.Veracity = 1.0
	}
	if o.Importance == 0 {
		o.Importance = 0.5
	}
	return o
}

func (s *Store) insert(content string, o Opts, now int64) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO memories
		(scope, project_id, kind, key, content, why, source, veracity, importance, created_at, updated_at, tags)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		s.scope, s.projectID, o.Kind, nullIfEmpty(o.Key), content, o.Why, o.Source, o.Veracity, o.Importance, now, now, o.Tags)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT INTO memories_fts(rowid, content, tags) VALUES (?,?,?)`, id, content, o.Tags); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// update 覆盖已有条目：保留 created_at 与 access_count（这条记忆的历史不该因为改写而清零）。
func (s *Store) update(id int64, content string, o Opts, now int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE memories SET content=?, kind=?, why=COALESCE(NULLIF(?,''), why),
		 source=?, veracity=?, importance=MAX(importance, ?), updated_at=?, tags=COALESCE(NULLIF(?,''), tags)
		 WHERE id=?`,
		content, o.Kind, o.Why, o.Source, o.Veracity, o.Importance, now, o.Tags, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE memories_fts SET content=?, tags=? WHERE rowid=?`, content, o.Tags, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) idByKey(key, scope string) (int64, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM memories WHERE scope=? AND project_id=? AND key=?`,
		scope, s.projectID, key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// nearDuplicate 找出与新内容说的是同一件事的旧条目（没有就返回 0）。
func (s *Store) nearDuplicate(content string) (int64, error) {
	cands, err := s.candidates(FTSQuery(content), nearDupCandidates, s.scope)
	if err != nil {
		return 0, err
	}
	for _, c := range cands {
		if Similarity(content, c.m.Content) >= nearDupThreshold {
			return c.m.ID, nil
		}
	}
	return 0, nil
}

// evictOverflow 超出上限时淘汰分数最低的条目（真删，含 FTS 行）。
func (s *Store) evictOverflow() error {
	if s.maxPerScope <= 0 {
		return nil
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM memories WHERE scope=? AND project_id=?`,
		s.scope, s.projectID).Scan(&n); err != nil {
		return err
	}
	over := n - s.maxPerScope
	if over <= 0 {
		return nil
	}
	all, err := s.candidates("", 0, s.scope) // 兜底查询：全量
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	for i := range all {
		all[i].score = scoreMemory(all[i].m, 0, now)
	}
	sortByScoreAsc(all)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i := 0; i < over && i < len(all); i++ {
		if _, err := tx.Exec(`DELETE FROM memories WHERE id=?`, all[i].m.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM memories_fts WHERE rowid=?`, all[i].m.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Get 按 id 读一条（含已失效的）。
func (s *Store) Get(id int64) (Memory, error) {
	rows, err := s.db.Query(columns("")+` FROM memories WHERE id=?`, id)
	if err != nil {
		return Memory{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Memory{}, sql.ErrNoRows
	}
	return scanMemory(rows)
}

// Forget 让一条记忆失效（按 id 或 key）：置 veracity=0 并在 why 里记下原因。
// 不删行——过时的记忆要能回答"当初为什么这么记、什么时候作废的"。
func (s *Store) Forget(idOrKey, reason string) error {
	id, err := s.resolveRef(idOrKey)
	if err != nil {
		return err
	}
	note := "已失效"
	if reason != "" {
		note += "：" + reason
	}
	_, err = s.db.Exec(`UPDATE memories SET veracity=0, why=TRIM(COALESCE(why,'')||' | '||?), updated_at=? WHERE id=?`,
		note, time.Now().Unix(), id)
	return err
}

// Invalidate 把一条记忆标记为被另一条取代。
func (s *Store) Invalidate(id, supersededBy int64) error {
	_, err := s.db.Exec(`UPDATE memories SET veracity=0, superseded_by=?, updated_at=? WHERE id=?`,
		supersededBy, time.Now().Unix(), id)
	return err
}

func (s *Store) resolveRef(idOrKey string) (int64, error) {
	if n, err := strconv.ParseInt(strings.TrimSpace(idOrKey), 10, 64); err == nil {
		return n, nil
	}
	id, err := s.idByKey(strings.TrimSpace(idOrKey), s.scope)
	if err != nil {
		return 0, err
	}
	if id == 0 {
		return 0, fmt.Errorf("没有 key 为 %q 的记忆", idOrKey)
	}
	return id, nil
}

// ClearProject 清空当前作用域的记忆（/forget 用；不动其它作用域）。
func (s *Store) ClearProject() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM memories_fts WHERE rowid IN
		(SELECT id FROM memories WHERE scope=? AND project_id=?)`, s.scope, s.projectID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM memories WHERE scope=? AND project_id=?`, s.scope, s.projectID); err != nil {
		return err
	}
	return tx.Commit()
}

// Clear 兼容旧调用点：等价于清当前作用域。
func (s *Store) Clear() error { return s.ClearProject() }

// Count 返回当前作用域的条数（测试与状态栏用）。
func (s *Store) Count() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM memories WHERE scope=? AND project_id=?`, s.scope, s.projectID).Scan(&n)
	return n, err
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
