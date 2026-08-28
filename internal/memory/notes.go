package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// fileNotesSchema 项目知识表：每个文件/目录一条结构化笔记（schema §5）。
// 来源是 explorer 类子 agent 的确定性产出，不调模型。
const fileNotesSchema = `
CREATE TABLE IF NOT EXISTS file_notes (
  project_id TEXT NOT NULL DEFAULT '',
  path       TEXT NOT NULL,
  summary    TEXT NOT NULL,
  symbols    TEXT,
  mtime      INTEGER NOT NULL DEFAULT 0,
  size       INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL,
  hit_count  INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (project_id, path)
);
`

// UpsertNote 写入/更新一条文件笔记。mtime/size 取自实际文件（相对进程 cwd，即项目根），
// 项目地图注入前用它检测过期；stat 不到就记 0/0（视为未变更，不误报过期）。
func (s *Store) UpsertNote(path, summary, symbols string) error {
	var mtime, size int64
	if st, err := os.Stat(path); err == nil {
		mtime, size = st.ModTime().Unix(), st.Size()
	}
	_, err := s.db.Exec(`
INSERT INTO file_notes (project_id, path, summary, symbols, mtime, size, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (project_id, path) DO UPDATE SET
  summary    = excluded.summary,
  symbols    = COALESCE(NULLIF(excluded.symbols, ''), file_notes.symbols),
  mtime      = excluded.mtime,
  size       = excluded.size,
  updated_at = excluded.updated_at`,
		s.projectID, path, summary, symbols, mtime, size, time.Now().Unix())
	return err
}

// NoteHit 给命中过笔记的文件计数（read_notes 顶替内容时调用；排序用）。
func (s *Store) NoteHit(path string) error {
	_, err := s.db.Exec(`UPDATE file_notes SET hit_count = hit_count + 1 WHERE project_id = ? AND path = ?`,
		s.projectID, path)
	return err
}

// fileNote 是项目地图的一行渲染视图。
type fileNote struct{ path, summary string }

// ProjectMap 渲染 <project-map>：按目录分组的「path — summary」，按 hit_count desc、updated_at desc
// 排序；预算 budget 为估算 token（rune/2），超出的行省略计数；mtime/size 与记录不一致的行标
// （可能已过时）。空库返回空串。stat 失败（文件已删）也标过时。
func (s *Store) ProjectMap(budget int) string {
	rows, err := s.db.Query(`SELECT path, summary, mtime, size FROM file_notes
		WHERE project_id = ? ORDER BY hit_count DESC, updated_at DESC`, s.projectID)
	if err != nil {
		return ""
	}
	defer rows.Close()

	var notes []fileNote
	for rows.Next() {
		var n fileNote
		var mtime, size int64
		if err := rows.Scan(&n.path, &n.summary, &mtime, &size); err != nil {
			continue
		}
		if noteStale(n.path, mtime, size) {
			n.summary += "（可能已过时）"
		}
		notes = append(notes, n)
	}
	if len(notes) == 0 {
		return ""
	}
	return renderProjectMap(notes, budget)
}

// noteStale 判断笔记是否过期：文件的 mtime/size 与记录不一致（或已无法 stat）。
func noteStale(path string, mtime, size int64) bool {
	st, err := os.Stat(path)
	if err != nil {
		return true
	}
	return st.ModTime().Unix() != mtime || st.Size() != size
}

// renderProjectMap 按预算渲染分组树。纯函数（可单测）。
func renderProjectMap(notes []fileNote, budget int) string {
	var lines []string
	used, elided := 0, 0
	// 按目录分组（组顺序 = 排序后组内最靠前笔记的位置，保持 hit/recency 语义）
	type group struct {
		dir   string
		notes []fileNote
	}
	var groups []*group
	byDir := map[string]*group{}
	for _, n := range notes {
		dir := filepath.Dir(n.path)
		g := byDir[dir]
		if g == nil {
			g = &group{dir: dir}
			byDir[dir] = g
			groups = append(groups, g)
		}
		g.notes = append(g.notes, n)
	}
	for _, g := range groups {
		if g.dir != "." && len(g.notes) > 1 {
			if used+len([]rune(g.dir))/2+1 > budget {
				elided += len(g.notes)
				continue
			}
			lines = append(lines, g.dir+"/")
			used += len([]rune(g.dir))/2 + 1
		}
		for _, n := range g.notes {
			line := n.path + " — " + n.summary
			if g.dir != "." && len(g.notes) > 1 {
				line = "  " + filepath.Base(n.path) + " — " + n.summary
			}
			cost := len([]rune(line))/2 + 1
			if used+cost > budget {
				elided++
				continue
			}
			lines = append(lines, line)
			used += cost
		}
	}
	if len(lines) == 0 {
		return ""
	}
	if elided > 0 {
		lines = append(lines, fmt.Sprintf("(…%d more)", elided))
	}
	return "<project-map>\n" + strings.Join(lines, "\n") + "\n</project-map>"
}
