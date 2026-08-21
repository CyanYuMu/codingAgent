package tool

import (
	"strings"
	"sync"
	"time"
)

// ScanCache 缓存目录枚举结果（path+type+mtime），绝不缓存内容/检索结果。
// 用「短 TTL + 写后失效」换近实时一致性。
type ScanCache struct {
	mu    sync.Mutex
	ttl   time.Duration
	items map[string]scanEntry
}

type scanEntry struct {
	createdAt time.Time
	entries   []string
}

func NewScanCache(ttl time.Duration) *ScanCache {
	return &ScanCache{ttl: ttl, items: map[string]scanEntry{}}
}

// Get 命中且未过期则返回缓存条目。
func (c *ScanCache) Get(key string) ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		return nil, false
	}
	if time.Since(e.createdAt) > c.ttl {
		delete(c.items, key) // 过期删除
		return nil, false
	}
	return e.entries, true
}

// Put 缓存一条目录枚举结果。
func (c *ScanCache) Put(key string, entries []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = scanEntry{createdAt: time.Now(), entries: entries}
}

// Invalidate 失效 key 以 prefix 开头的缓存条目（写文件后调用）。
func (c *ScanCache) Invalidate(prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.items {
		if strings.HasPrefix(k, prefix) {
			delete(c.items, k)
		}
	}
}
