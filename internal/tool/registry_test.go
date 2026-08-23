package tool

import (
	"context"
	"testing"

	"einoclaw-build/internal/permission"
	"einoclaw-build/internal/runtime"
)

type fakeTool struct {
	name string
	tier permission.Tier
}

func (f fakeTool) Name() string               { return f.name }
func (f fakeTool) Description() string        { return "d" }
func (f fakeTool) Parameters() map[string]any { return nil }
func (f fakeTool) Tier() permission.Tier      { return f.tier }
func (f fakeTool) Concurrency() Concurrency   { return ConcurrencyShared }
func (f fakeTool) Execute(ctx context.Context, args map[string]any, sink *runtime.Sink) error {
	sink.Write([]byte("ok"))
	return nil
}

func TestRegistryRegisterGet(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeTool{name: "read", tier: permission.TierRead})
	if _, ok := r.Get("read"); !ok {
		t.Fatal("Get(read) 应为 ok")
	}
	if _, ok := r.Get("nope"); ok {
		t.Fatal("Get(nope) 应为 !ok")
	}
}

func TestRegistrySpecs(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeTool{name: "read", tier: permission.TierRead})
	specs := r.Specs()
	if len(specs) != 1 || specs[0].Name != "read" {
		t.Fatalf("specs = %+v", specs)
	}
}
