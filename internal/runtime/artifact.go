package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ArtifactScheme 是产物引用的 URL 前缀：artifact://<id>。
const ArtifactScheme = "artifact://"

var artifactName = regexp.MustCompile(`^(\d+)\.[A-Za-z0-9_-]+\.log$`)

// ArtifactStore 管理一个会话的产物目录：<dir>/<id>.<tool>.log，id 单调递增。
// 首次分配前扫描已有文件取最大 id，resume 不会覆盖旧产物。
// 它同时是会话内 URL 的路由表：除内置的 artifact:// 外，装配层可以注册 agent:// / history:// 等方案，
// 这样「读回大内容」对模型永远只有 read_file 一个入口。
type ArtifactStore struct {
	dir     string
	mu      sync.Mutex
	next    int64
	init    bool
	schemes map[string]func(rest string) (string, error)
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

// AddScheme 注册一个额外的 URL 方案（如 agent / history）；重复注册后者覆盖。
func (s *ArtifactStore) AddScheme(scheme string, resolve func(rest string) (string, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.schemes == nil {
		s.schemes = map[string]func(string) (string, error){}
	}
	s.schemes[scheme] = resolve
}

// Resolve 把会话内 URL 解析为文件路径：按 "<scheme>://" 前缀分派，无前缀时按 artifact id 处理。
func (s *ArtifactStore) Resolve(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	i := strings.Index(ref, "://")
	if i <= 0 {
		return s.resolveArtifact(ref)
	}
	scheme, rest := ref[:i], ref[i+3:]
	if scheme == "artifact" {
		return s.resolveArtifact(rest)
	}
	s.mu.Lock()
	fn, ok := s.schemes[scheme]
	known := s.schemeNamesLocked()
	s.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("不认识的 URL 方案 %q；可用：%s", scheme, strings.Join(known, ", "))
	}
	return fn(rest)
}

func (s *ArtifactStore) resolveArtifact(id string) (string, error) {
	id = strings.TrimSpace(id)
	if _, err := strconv.Atoi(id); err != nil {
		return "", fmt.Errorf("artifact id 必须是数字，got %q", id)
	}
	matches, _ := filepath.Glob(filepath.Join(s.dir, id+".*.log"))
	if len(matches) == 0 {
		return "", fmt.Errorf("artifact %s 不存在", id)
	}
	return matches[0], nil
}

func (s *ArtifactStore) schemeNamesLocked() []string {
	out := []string{"artifact"}
	for k := range s.schemes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
