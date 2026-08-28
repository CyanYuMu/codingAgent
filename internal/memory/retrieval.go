package memory

import (
	"database/sql"
	"math"
	"sort"
	"time"
)

// halfLife 是 recency 衰减的半衰期（72 小时）。
const halfLife = 72 * 3600

// scopeBoost 同分时项目内的记忆更贴题，全局记忆是背景。
func scopeBoost(scope string) float64 {
	if scope == ScopeGlobal {
		return 0.6
	}
	return 1.0
}

// scoreMemory 融合五个信号：FTS 匹配度、重要性、新鲜度、被用过的次数、作用域，再乘可信度。
// 全是纯代码：召回质量的调参要能被单测钉住，不能靠模型判断。
func scoreMemory(m Memory, ftsRank float64, now int64) float64 {
	recency := math.Exp(-float64(now-m.UpdatedAt) / halfLife)
	access := math.Log1p(float64(m.AccessCount)) / math.Log(10) // 10 次 ≈ 1.0
	return (0.45*ftsRank + 0.20*m.Importance + 0.15*recency +
		0.10*math.Min(access, 1) + 0.10*scopeBoost(m.Scope)) * m.Veracity
}

// normalizeBm25 把 bm25（负值，越小越相关）归一化到 0-1（越大越相关）。
func normalizeBm25(bm float64) float64 { return 1.0 / (1.0 - bm) }

// columns 生成查询列。join FTS 表时必须带 m. 前缀：两张表都有 content 列，不限定就是 ambiguous。
func columns(p string) string {
	return `SELECT ` + p + `id, ` + p + `scope, ` + p + `project_id, ` + p + `kind, COALESCE(` + p + `key,''), ` +
		p + `content, COALESCE(` + p + `why,''), ` + p + `source, ` + p + `veracity, ` + p + `importance, ` +
		p + `created_at, ` + p + `updated_at, COALESCE(` + p + `last_accessed,0), ` + p + `access_count, ` +
		`COALESCE(` + p + `superseded_by,0), COALESCE(` + p + `tags,'')`
}

type scored struct {
	m     Memory
	score float64
}

func scanMemory(rows *sql.Rows, extra ...any) (Memory, error) {
	var m Memory
	dst := []any{&m.ID, &m.Scope, &m.ProjectID, &m.Kind, &m.Key, &m.Content, &m.Why, &m.Source,
		&m.Veracity, &m.Importance, &m.CreatedAt, &m.UpdatedAt, &m.LastAccessed, &m.AccessCount,
		&m.SupersededBy, &m.Tags}
	return m, rows.Scan(append(dst, extra...)...)
}

// candidates 取候选集：有清洗后的查询就走 FTS，没有就按重要性与新鲜度兜底。
// query 为空是正常情况（"你还记得什么" 这类问句没有可用实词），不是错误。
func (s *Store) candidates(query string, limit int, scope string) ([]scored, error) {
	if query == "" {
		return s.fallbackCandidates(limit, scope)
	}
	sqlText := columns("m.") + `, bm25(memories_fts)
		FROM memories_fts JOIN memories m ON m.id = memories_fts.rowid
		WHERE memories_fts MATCH ? AND m.veracity > 0 AND m.scope = ?
		ORDER BY bm25(memories_fts)`
	args := []any{query, scope}
	if limit > 0 {
		sqlText += ` LIMIT ?`
		args = append(args, limit*4) // 多取一些交给打分排序
	}
	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	now := time.Now().Unix()
	var out []scored
	for rows.Next() {
		var bm float64
		m, err := scanMemory(rows, &bm)
		if err != nil {
			return nil, err
		}
		out = append(out, scored{m, scoreMemory(m, normalizeBm25(bm), now)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		// trigram 对同义改写很脆（问"语言偏好"匹配不到记着的"用户偏好"）。
		// 一条都没命中时退回重要性/新鲜度：记忆是背景上下文，宁可给几条略偏的，
		// 也好过让用户觉得"记了等于没记"。
		return s.fallbackCandidates(limit, scope)
	}
	return out, nil
}

// fallbackCandidates 无实词时的兜底：按重要性与新鲜度取，保证「你还记得什么」也有结果。
func (s *Store) fallbackCandidates(limit int, scope string) ([]scored, error) {
	sqlText := columns("") + ` FROM memories WHERE veracity > 0 AND scope = ? ORDER BY importance DESC, updated_at DESC`
	args := []any{scope}
	if limit > 0 {
		sqlText += ` LIMIT ?`
		args = append(args, limit*4)
	}
	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	now := time.Now().Unix()
	var out []scored
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, scored{m, scoreMemory(m, 0, now)})
	}
	return out, rows.Err()
}

// Recall 多信号召回 topK 条相关记忆，并回写访问计数。
// 查询先经 FTSQuery 清洗——把用户原文直接交给 MATCH 会在含 ?()"- 时报语法错，
// 而那个错误一旦被上层吞掉，表现就是「记忆功能看起来在，实际永远召回不到」。
func (s *Store) Recall(query string, topK int) ([]Memory, error) {
	if topK <= 0 {
		topK = 5
	}
	cands, err := s.candidates(FTSQuery(query), topK, s.scope)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	if len(cands) > topK {
		cands = cands[:topK]
	}
	out := make([]Memory, len(cands))
	ids := make([]int64, len(cands))
	for i, c := range cands {
		out[i], ids[i] = c.m, c.m.ID
	}
	s.touch(ids) // 用过的记忆更可能再被用到；失败不影响召回本身
	return out, nil
}

// touch 回写访问计数与时间。
func (s *Store) touch(ids []int64) {
	if len(ids) == 0 {
		return
	}
	now := time.Now().Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.Exec(`UPDATE memories SET access_count = access_count + 1, last_accessed = ? WHERE id = ?`, now, id); err != nil {
			return
		}
	}
	_ = tx.Commit()
}

func sortByScoreAsc(xs []scored) {
	sort.SliceStable(xs, func(i, j int) bool { return xs[i].score < xs[j].score })
}

// union 把多个库当一个召回源：各自召回后合并、按分数排序、按内容去重。
type union struct{ stores []*Store }

// Union 构造多库召回器（项目库 + 全局库）。单个 store 出错不拖垮其它库。
func Union(stores ...*Store) Recaller {
	live := make([]*Store, 0, len(stores))
	for _, s := range stores {
		if s != nil {
			live = append(live, s)
		}
	}
	return union{stores: live}
}

func (u union) Recall(query string, topK int) ([]Memory, error) {
	now := time.Now().Unix()
	var all []scored
	var firstErr error
	for _, s := range u.stores {
		ms, err := s.Recall(query, topK)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, m := range ms {
			all = append(all, scored{m, scoreMemory(m, 0.5, now)}) // 已在各库内排过序，这里只做跨库对齐
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].score > all[j].score })
	seen := map[string]bool{}
	out := make([]Memory, 0, topK)
	for _, c := range all {
		if seen[c.m.Content] {
			continue
		}
		seen[c.m.Content] = true
		if out = append(out, c.m); len(out) >= topK {
			break
		}
	}
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}
