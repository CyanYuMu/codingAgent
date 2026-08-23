package session

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"einoclaw-build/internal/message"
)

func TestManagerNewListSwitch(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s1, err := m.New("/proj")
	if err != nil {
		t.Fatal(err)
	}
	_ = s1.Append(msg(message.RoleUser, "first question\nsecond line"))
	_ = s1.SetTitle("修复登录")
	id1 := s1.Header().ID
	s1.Close()
	time.Sleep(10 * time.Millisecond)
	s2, _ := m.New("/proj")
	_ = s2.Append(msg(message.RoleUser, "another"))
	s2.Close()

	infos, err := m.List()
	if err != nil || len(infos) != 2 {
		t.Fatalf("list = %+v err %v", infos, err)
	}
	var found bool
	for _, in := range infos {
		if in.ID == id1 {
			found = true
			if in.Title != "修复登录" || in.FirstUser != "first question" || in.Label() != "修复登录" {
				t.Fatalf("info = %+v", in)
			}
		}
	}
	if !found {
		t.Fatal("s1 not listed")
	}

	// 唯一前缀切换（去掉末尾两位仍唯一）
	sw, err := m.Switch(id1[:len(id1)-2])
	if err != nil || sw.Header().ID != id1 {
		t.Fatalf("switch = %v err %v", sw, err)
	}
	ms, _ := sw.Replay()
	if len(ms) != 1 || !strings.HasPrefix(ms[0].Blocks[0].Text, "first question") {
		t.Fatalf("switched replay = %+v", ms)
	}
	sw.Close()

	cur, _ := m.Current("/proj")
	if cur.Header().ID != id1 {
		t.Fatalf("current = %q, want %q", cur.Header().ID, id1)
	}
	ad, _ := m.ArtifactDir(cur)
	if filepath.Base(ad) != id1 || filepath.Dir(ad) != m.Dir() {
		t.Fatalf("artifact dir = %q", ad)
	}
	cur.Close()

	// 模糊前缀（两个会话都以日期开头）应报错
	if _, err := m.Switch(id1[:4]); err == nil {
		t.Fatal("ambiguous prefix should error")
	}
}

func TestManagerCurrentCreatesWhenEmpty(t *testing.T) {
	m, _ := NewManager(t.TempDir())
	s, err := m.Current("/p")
	if err != nil || s.Header().CWD != "/p" {
		t.Fatalf("current = %+v err %v", s, err)
	}
	s.Close()
	infos, _ := m.List()
	if len(infos) != 1 {
		t.Fatalf("list = %d", len(infos))
	}
}
