package memory

import (
	"math"
	"sort"
	"time"
)

// halfLife 是 recency 衰减的半衰期（72 小时）。
const halfLife = 72 * 3600

// scoreMemory 融合四个信号：FTS 匹配度 + importance + recency，再乘 veracity。
// 返回 0-1 的分数。
func scoreMemory(m Memory, ftsRank float64, now int64) float64 {
	age := float64(now - m.CreatedAt)
	recency := math.Exp(-age / halfLife)
	return (0.5*ftsRank + 0.3*m.Importance + 0.2*recency) * m.Veracity
}

// normalizeBm25 把 bm25（负值，越小越相关）归一化到 0-1（越大越相关）。
func normalizeBm25(bm float64) float64 {
	return 1.0 / (1.0 - bm)
}

// Recall 多信号召回 topK 条相关记忆（纯代码，不靠模型）。
func (s *Store) Recall(query string, topK int) ([]Memory, error) {
	now := time.Now().Unix()
	rows, err := s.db.Query(`
		SELECT m.id, m.content, m.source, m.veracity, m.importance, m.memory_type, m.created_at,
		       bm25(memory_fts)
		FROM memory_fts
		JOIN working_memory m ON m.id = memory_fts.rowid
		WHERE memory_fts MATCH ?
		ORDER BY bm25(memory_fts)`, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type scored struct {
		m Memory
		s float64
	}
	var candidates []scored
	for rows.Next() {
		var m Memory
		var bm float64
		if err := rows.Scan(&m.ID, &m.Content, &m.Source, &m.Veracity, &m.Importance, &m.MemoryType, &m.CreatedAt, &bm); err != nil {
			return nil, err
		}
		candidates = append(candidates, scored{m, scoreMemory(m, normalizeBm25(bm), now)})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].s > candidates[j].s })
	if len(candidates) > topK {
		candidates = candidates[:topK]
	}
	out := make([]Memory, len(candidates))
	for i, c := range candidates {
		out[i] = c.m
	}
	return out, nil
}
