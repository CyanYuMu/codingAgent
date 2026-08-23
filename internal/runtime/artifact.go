package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// ArtifactScheme 是产物引用的 URL 前缀：artifact://<id>。
const ArtifactScheme = "artifact://"

var artifactName = regexp.MustCompile(`^(\d+)\.[A-Za-z0-9_-]+\.log$`)

// ArtifactStore 管理一个会话的产物目录：<dir>/<id>.<tool>.log，id 单调递增。
// 首次分配前扫描已有文件取最大 id，resume 不会覆盖旧产物。
type ArtifactStore struct {
	dir  string
	mu   sync.Mutex
	next int64
	init bool
}

// NewArtifactStore 构造产物存储；目录在首次分配时创建。
func NewArtifactStore(dir string) *ArtifactStore { return &ArtifactStore{dir: dir} }

// Dir 返回产物目录。
func (s *ArtifactStore) Dir() string { return s.dir }

func (s *ArtifactStore) scanLocked() error {
	if s.init {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	des, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, de := range des {
		if m := artifactName.FindStringSubmatch(de.Name()); m != nil {
			if n, err := strconv.ParseInt(m[1], 10, 64); err == nil && n >= s.next {
				s.next = n + 1
			}
		}
	}
	s.init = true
	return nil
}

// Create 分配一个新产物文件（调用方负责 Close）。
func (s *ArtifactStore) Create(tool string) (string, *os.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.scanLocked(); err != nil {
		return "", nil, err
	}
	id := strconv.FormatInt(s.next, 10)
	s.next++
	f, err := os.Create(filepath.Join(s.dir, id+"."+sanitizeTool(tool)+".log"))
	if err != nil {
		return "", nil, err
	}
	return id, f, nil
}

// Resolve 把 "artifact://N" 或 "N" 解析为文件路径。
func (s *ArtifactStore) Resolve(ref string) (string, error) {
	id := strings.TrimPrefix(strings.TrimSpace(ref), ArtifactScheme)
	if _, err := strconv.Atoi(id); err != nil {
		return "", fmt.Errorf("artifact id 必须是数字，got %q", ref)
	}
	matches, _ := filepath.Glob(filepath.Join(s.dir, id+".*.log"))
	if len(matches) == 0 {
		return "", fmt.Errorf("artifact %s 不存在", id)
	}
	return matches[0], nil
}

// sanitizeTool 把工具名收敛到 [A-Za-z0-9_-]，空则 "tool"。
func sanitizeTool(t string) string {
	var b strings.Builder
	for _, r := range t {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "tool"
	}
	return b.String()
}
