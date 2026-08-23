package runtime

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// Sink 累积工具输出，做「头尾窗口截断 + artifact 落盘」——分层 Context 的 L6 实现。
// 上下文里放「指针 + 精华」，完整结果在磁盘。
type Sink struct {
	mu         sync.Mutex
	headLimit  int
	tailLimit  int
	buf        []byte // 当前窗口（head + tail，中间被 elide）
	total      int    // 总写入字节数
	truncated  bool
	store      *ArtifactStore
	tool       string
	artifact   *os.File
	artifactID string
}

// NewSink 创建 sink；headLimit/tailLimit 是保留的头部/尾部字节数。
func NewSink(headLimit, tailLimit int) *Sink {
	return &Sink{headLimit: headLimit, tailLimit: tailLimit}
}

// SetArtifactStore 设置截断时落盘用的产物存储与工具名。
func (s *Sink) SetArtifactStore(store *ArtifactStore, tool string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store = store
	s.tool = tool
}

// Write 累积输出；超窗口时保留头尾、中间截断，并把完整内容落盘。
func (s *Sink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 已截断：新内容也要写进 artifact 保持完整
	if s.truncated && s.artifact != nil {
		_, _ = s.artifact.Write(p)
	}

	s.buf = append(s.buf, p...)
	s.total += len(p)

	if len(s.buf) > s.headLimit+s.tailLimit {
		if !s.truncated {
			s.openArtifactLocked()
			if s.artifact != nil {
				_, _ = s.artifact.Write(s.buf) // 首次截断：把当前完整内容落盘
			}
			s.truncated = true
		}
		// 窗口截断：头 headLimit + 尾 tailLimit
		head := append([]byte{}, s.buf[:s.headLimit]...)
		tail := append([]byte{}, s.buf[len(s.buf)-s.tailLimit:]...)
		s.buf = append(head, tail...)
	}
	return len(p), nil
}

// Result 返回给模型的结果文本：未截断=原文；截断=头尾 + elide 标记 + artifact 指针。
func (s *Sink) Result() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.truncated {
		return string(s.buf)
	}
	elided := s.total - len(s.buf)
	var sb strings.Builder
	sb.Write(s.buf[:s.headLimit])
	fmt.Fprintf(&sb, "\n...(%d bytes elided)...\n", elided)
	sb.Write(s.buf[s.headLimit:])
	if s.artifact != nil {
		fmt.Fprintf(&sb, "\n[完整输出已保存: %s%s ；用 read_file 的 file_path=\"%s%s\" 可按行读取]", ArtifactScheme, s.artifactID, ArtifactScheme, s.artifactID)
	}
	return sb.String()
}

// Truncated 报告输出是否被截断。
func (s *Sink) Truncated() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.truncated
}

// ArtifactID 返回落盘产物 id（未落盘为空）。
func (s *Sink) ArtifactID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.artifactID
}

// Close 关闭 artifact 文件。
func (s *Sink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.artifact != nil {
		return s.artifact.Close()
	}
	return nil
}

func (s *Sink) openArtifactLocked() {
	if s.store == nil {
		return // 未配置产物存储：只截断不落盘（Result 不显示指针）
	}
	id, f, err := s.store.Create(s.tool)
	if err != nil {
		return
	}
	s.artifactID = id
	s.artifact = f
}
