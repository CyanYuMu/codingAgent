package tool

import (
	"path/filepath"
	"slices"
	"sync"

	"einoclaw-build/internal/workspace"
)

// fileGuard 会话级文件状态：read_file 的已读记录（区间去重）+ write/edit 的「先读后改」守卫。
// 三个文件工具共享一个实例（Builtins 装配），避免两份状态漂移；换会话由 Registry.ResetConv 清空。
// 状态有两个前提：去重靠「内容仍在上文中」（压缩/剪枝后失效，见 invalidateReads），
// 守卫靠「本会话真的读过且此后未被外部改动」（指纹兜底，不依赖区间）。
type fileGuard struct {
	mu        sync.Mutex
	reads     map[string]*readRecord
	workspace *workspace.Workspace
	hashes    map[string]string
}

// readRecord 一个文件的已读状态：内容指纹（mtime+size）与已读行区间（1 起闭区间，不重叠有序）。
type readRecord struct {
	mtime, size int64
	ranges      []lineRange
}

type lineRange struct{ from, to int }

func newFileGuard() *fileGuard {
	return &fileGuard{reads: map[string]*readRecord{}}
}

// fsKey 路径键归一：Clean 让 ./f.txt 与 f.txt 命中同一条记录；无法归一的形态不一致时守卫 fail-safe 拒绝。
func fsKey(path string) string { return filepath.Clean(path) }

// recordRead 登记/合并一次读取（read_file 成功读取真实文件后调用）；文件变更（指纹不符）则重置区间历史。
func (g *fileGuard) recordRead(path string, mtime, size int64, from, to int) {
	k := fsKey(path)
	g.mu.Lock()
	defer g.mu.Unlock()
	r := g.reads[k]
	if r == nil || r.mtime != mtime || r.size != size {
		r = &readRecord{mtime: mtime, size: size}
		g.reads[k] = r
	}
	r.ranges = insertRange(r.ranges, from, to)
}

// alreadyRead 判断文件未变更且请求区间已被已读区间并集覆盖。
func (g *fileGuard) alreadyRead(path string, mtime, size int64, from, to int) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	r := g.reads[fsKey(path)]
	if r == nil || r.mtime != mtime || r.size != size {
		return false
	}
	need := to - from + 1
	got := 0
	for _, rg := range r.ranges { // 区间互不重叠：剪裁后直接累加即并集大小
		got += max(0, min(rg.to, to)-max(rg.from, from)+1)
	}
	return got >= need
}

// markWritten 写成功后把指纹更新为写入后的 stat 并清空区间：此后 read 要真读新内容（旧区间是旧文），
// 后续 edit 不再因指纹过期要求重读——刚写/刚改的内容模型是知道的。
func (g *fileGuard) markWritten(path string, mtime, size int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reads[fsKey(path)] = &readRecord{mtime: mtime, size: size}
}

// freshRead edit 前置检查：本会话登记过该文件，且登记指纹与调用方传入的当前指纹一致。
// false = 没读过，或读过之后被外部修改——两种情况都要求重新 read_file。
func (g *fileGuard) freshRead(path string, mtime, size int64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	r := g.reads[fsKey(path)]
	return r != nil && r.mtime == mtime && r.size == size
}

// invalidateReads 压缩/剪枝成功后调用：清空已读区间——旧内容已进摘要/占位，「内容仍在上文中」不再成立；
// 指纹保留，edit 守卫仍认「本会话读过」（它只依赖 mtime 未被外部改动，不依赖区间）。
func (g *fileGuard) invalidateReads() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, r := range g.reads {
		r.ranges = nil
	}
}

// reset 换会话清空全部状态。
func (g *fileGuard) reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reads = map[string]*readRecord{}
	g.hashes = nil
}

// insertRange 把 [from,to] 并入有序不重叠的区间列表（相邻合并）。
func insertRange(ranges []lineRange, from, to int) []lineRange {
	merged := lineRange{from, to}
	out := make([]lineRange, 0, len(ranges)+1)
	for _, r := range ranges {
		if r.to+1 < merged.from || merged.to+1 < r.from {
			out = append(out, r)
		} else {
			merged.from = min(merged.from, r.from)
			merged.to = max(merged.to, r.to)
		}
	}
	out = append(out, merged)
	slices.SortFunc(out, func(a, b lineRange) int { return a.from - b.from })
	return out
}
