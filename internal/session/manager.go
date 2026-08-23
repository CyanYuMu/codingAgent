package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"einoclaw-build/internal/message"
)

// Manager 管理一个项目桶里的会话：<dir>/<id>.jsonl、<dir>/<id>/（产物）、<dir>/current。
type Manager struct{ dir string }

// Info 是会话清单里的一行。
type Info struct {
	ID, Title, FirstUser, Path string
	ModTime                    time.Time
}

// Label 返回展示名：标题 > 首句 > id。
func (in Info) Label() string {
	if in.Title != "" {
		return in.Title
	}
	if in.FirstUser != "" {
		return in.FirstUser
	}
	return in.ID
}

// NewManager 建管理器（项目桶目录）。
func NewManager(projectDir string) (*Manager, error) {
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		return nil, err
	}
	return &Manager{dir: projectDir}, nil
}

// Dir 返回项目桶目录。
func (m *Manager) Dir() string { return m.dir }

// Current 打开 current 指向的会话；无则新建。
func (m *Manager) Current(cwd string) (*Session, error) {
	id := m.currentID()
	if id == "" {
		return m.New(cwd)
	}
	if _, err := os.Stat(m.path(id)); err != nil {
		return m.New(cwd)
	}
	return m.open(id)
}

// New 新建会话（id = 时间戳_6hex）并设为当前。
func (m *Manager) New(cwd string) (*Session, error) {
	id := time.Now().Format("20060102-150405") + "_" + newID()[:6]
	st, err := NewFileStorage(m.path(id))
	if err != nil {
		return nil, err
	}
	s, err := NewWithHeader(Header{ID: id, CWD: cwd}, st)
	if err != nil {
		st.Close()
		return nil, err
	}
	return s, m.setCurrent(id)
}

// Switch 按 id 或唯一前缀切换到指定会话。
func (m *Manager) Switch(idPrefix string) (*Session, error) {
	infos, err := m.List()
	if err != nil {
		return nil, err
	}
	var match []string
	for _, in := range infos {
		if in.ID == idPrefix {
			match = []string{in.ID}
			break
		}
		if strings.HasPrefix(in.ID, idPrefix) {
			match = append(match, in.ID)
		}
	}
	if len(match) != 1 {
		return nil, fmt.Errorf("会话 %q 匹配到 %d 个", idPrefix, len(match))
	}
	s, err := m.open(match[0])
	if err != nil {
		return nil, err
	}
	return s, m.setCurrent(match[0])
}

// List 列出会话（最近在前），只读每个文件前几行取标题/首条用户消息；子 agent sidecar 不列出。
func (m *Manager) List() ([]Info, error) {
	des, err := os.ReadDir(m.dir)
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, de := range des {
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, ".jsonl") || strings.HasPrefix(name, "agent-") {
			continue
		}
		st, err := de.Info()
		if err != nil {
			continue
		}
		id := strings.TrimSuffix(name, ".jsonl")
		in := Info{ID: id, Path: m.path(id), ModTime: st.ModTime()}
		in.Title, in.FirstUser = peek(in.Path)
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	return out, nil
}

// peek 读前 12 行：header 标题 / title_change / 首条 user 文本。
func peek(path string) (title, firstUser string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 4<<20)
	for i := 0; i < 12 && sc.Scan(); i++ {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		switch e.Type {
		case EntrySession, EntryTitle:
			if e.Title != "" {
				title = e.Title
			}
		case EntryMessage:
			if firstUser == "" && e.Message != nil && e.Message.Role == message.RoleUser {
				for _, b := range e.Message.Blocks {
					if b.Kind == message.BlockText && strings.TrimSpace(b.Text) != "" {
						firstUser = strings.SplitN(strings.TrimSpace(b.Text), "\n", 2)[0]
						break
					}
				}
			}
		}
	}
	return title, firstUser
}

// ArtifactDir 返回会话产物目录（<dir>/<id>/）并确保存在。
func (m *Manager) ArtifactDir(s *Session) (string, error) {
	d := filepath.Join(m.dir, s.Header().ID)
	return d, os.MkdirAll(d, 0o755)
}

func (m *Manager) path(id string) string { return filepath.Join(m.dir, id+".jsonl") }

func (m *Manager) currentID() string {
	b, err := os.ReadFile(filepath.Join(m.dir, "current"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (m *Manager) setCurrent(id string) error {
	return os.WriteFile(filepath.Join(m.dir, "current"), []byte(id), 0o644)
}

func (m *Manager) open(id string) (*Session, error) {
	st, err := NewFileStorage(m.path(id))
	if err != nil {
		return nil, err
	}
	s, err := Open(st)
	if err != nil {
		st.Close()
		return nil, err
	}
	return s, nil
}
