package memory

import (
	"path/filepath"
	"testing"
)

func TestScoreMultiSignal(t *testing.T) {
	now := int64(1000000)
	m := Memory{Importance: 0.9, Veracity: 0.8, CreatedAt: now - 3600} // 1 小时前
	high := scoreMemory(m, 0.8, now)
	m2 := m
	m2.Importance = 0.1
	low := scoreMemory(m2, 0.8, now)
	if high <= low {
		t.Fatalf("高 importance 应得更高分：high=%v low=%v", high, low)
	}
}

func TestRememberRecall(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "mem.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	_ = s.Remember("用户偏好用 Go 语言", MemoryOpts{Source: "user", Importance: 0.9, Veracity: 1.0, MemoryType: "preference"})
	_ = s.Remember("用户昨天喝了咖啡", MemoryOpts{Source: "user", Importance: 0.1, Veracity: 1.0, MemoryType: "fact"})

	mems, err := s.Recall("偏好用 Go", 5)
	if err != nil || len(mems) == 0 {
		t.Fatalf("recall = %d, err = %v", len(mems), err)
	}
	if mems[0].Content != "用户偏好用 Go 语言" {
		t.Fatalf("top1 应为偏好记忆，got %q", mems[0].Content)
	}
}
