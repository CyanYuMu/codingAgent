package memory

import (
	"database/sql"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，注册为 "sqlite"
)

// Memory 一条记忆。
type Memory struct {
	ID         int64
	Content    string
	Source     string
	Veracity   float64
	Importance float64
	MemoryType string
	CreatedAt  int64
}

// MemoryOpts 写入记忆时的可选参数。
type MemoryOpts struct {
	Source     string
	Veracity   float64
	Importance float64
	MemoryType string
}

// Recaller 是召回能力的抽象（agent 依赖它，测试可注入假实现）。
type Recaller interface {
	Recall(query string, topK int) ([]Memory, error)
}

// Store 是 SQLite 记忆存储。
type Store struct {
	db *sql.DB
}

// Open 打开/创建记忆库。
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	schema := `
	CREATE TABLE IF NOT EXISTS working_memory (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		content     TEXT NOT NULL,
		source      TEXT NOT NULL DEFAULT 'user',
		veracity    REAL NOT NULL DEFAULT 1.0,
		importance  REAL NOT NULL DEFAULT 0.5,
		memory_type TEXT NOT NULL DEFAULT 'fact',
		created_at  INTEGER NOT NULL
	);
	CREATE VIRTUAL TABLE IF NOT EXISTS memory_fts USING fts5(content, tokenize='trigram');
	`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Remember 写一条记忆（幂等、不阻塞会话）。
func (s *Store) Remember(content string, opts MemoryOpts) error {
	if opts.Source == "" {
		opts.Source = "user"
	}
	if opts.Veracity == 0 {
		opts.Veracity = 1.0
	}
	if opts.Importance == 0 {
		opts.Importance = 0.5
	}
	if opts.MemoryType == "" {
		opts.MemoryType = "fact"
	}

	// 两表写入用事务，避免 working_memory 与 memory_fts 不一致（FTS 孤儿行）
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		`INSERT INTO working_memory (content, source, veracity, importance, memory_type, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		content, opts.Source, opts.Veracity, opts.Importance, opts.MemoryType, time.Now().Unix(),
	)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO memory_fts(rowid, content) VALUES (?, ?)`, id, content); err != nil {
		return err
	}
	return tx.Commit()
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }
