package session

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"sync"
)

// Storage 是会话日志的底层存取抽象。
type Storage interface {
	Append(e Entry) error      // 追加一行
	Entries() ([]Entry, error) // 读回全部行（原始顺序，不处理 reset）
	Close() error
}

// FileStorage 把日志写进一个 JSONL 文件。
type FileStorage struct {
	path string
	f    *os.File
	w    *bufio.Writer
}

func NewFileStorage(path string) (*FileStorage, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	return &FileStorage{path: path, f: f, w: bufio.NewWriter(f)}, nil
}

func (fs *FileStorage) Append(e Entry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := fs.w.Write(b); err != nil {
		return err
	}
	if err := fs.w.WriteByte('\n'); err != nil {
		return err
	}
	return fs.w.Flush()
}

func (fs *FileStorage) Entries() ([]Entry, error) {
	data, err := os.ReadFile(fs.path)
	if err != nil {
		return nil, err
	}
	var entries []Entry
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e Entry
		if json.Unmarshal([]byte(line), &e) != nil {
			continue // 跳过残行（崩溃截断）
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func (fs *FileStorage) Close() error { return fs.f.Close() }

// MemoryStorage 内存实现，供单测用。
type MemoryStorage struct {
	mu      sync.Mutex
	entries []Entry
}

func (m *MemoryStorage) Append(e Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, e)
	return nil
}

func (m *MemoryStorage) Entries() ([]Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Entry, len(m.entries))
	copy(out, m.entries)
	return out, nil
}

func (m *MemoryStorage) Close() error { return nil }
