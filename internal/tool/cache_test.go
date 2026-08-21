package tool

import (
	"testing"
	"time"
)

func TestScanCachePutGetInvalidate(t *testing.T) {
	c := NewScanCache(time.Minute)
	c.Put("root", []string{"a.go", "b.go"})
	if entries, ok := c.Get("root"); !ok || len(entries) != 2 {
		t.Fatalf("Get = %v, %v", entries, ok)
	}
	c.Invalidate("root")
	if _, ok := c.Get("root"); ok {
		t.Fatal("Invalidate 后应 miss")
	}
}

func TestScanCacheExpiry(t *testing.T) {
	c := NewScanCache(time.Millisecond) // 1ms TTL
	c.Put("root", []string{"a.go"})
	time.Sleep(2 * time.Millisecond)
	if _, ok := c.Get("root"); ok {
		t.Fatal("过期后应 miss")
	}
}
