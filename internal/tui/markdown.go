package tui

import (
	"strings"

	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
)

// ============ 基础渲染 ============

var (
	glamourRenderer *glamour.TermRenderer
	glamourWidth    int // 渲染器构建时的换行宽度，宽度变了才重建
)

func initMarkdown(w int) {
	wrapWidth := max(40, w-4)
	if glamourRenderer != nil && glamourWidth == wrapWidth {
		return
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle("dracula"),
		glamour.WithWordWrap(wrapWidth),
	)
	if err != nil {
		return
	}
	glamourRenderer = r
	glamourWidth = wrapWidth
}

// renderMarkdown 把 Markdown 文本渲染成 ANSI 行数组(全量渲染)。
func renderMarkdown(text string, width int) []string {
	if width <= 0 {
		return strings.Split(text, "\n")
	}
	initMarkdown(width)
	if glamourRenderer == nil {
		return strings.Split(text, "\n")
	}
	out, err := glamourRenderer.Render(text)
	if err != nil {
		return strings.Split(text, "\n")
	}
	return strings.Split(strings.TrimSpace(out), "\n")
}

// renderThinking 把思考过程渲染成灰色背景块(自动换行)。阶段4 简化版，无折叠。
func renderThinking(content string, width int) []string {
	style := lipgloss.NewStyle().Background(lipgloss.Color("235")).Foreground(lipgloss.Color("250"))
	indent := 2
	textWidth := max(10, width-indent-2)

	var lines []string
	for _, para := range strings.Split(content, "\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		var cur []rune
		curW := 0
		for _, r := range para {
			rw := lipgloss.Width(string(r))
			if curW+rw > textWidth && len(cur) > 0 {
				lines = append(lines, string(cur))
				cur = cur[:0]
				curW = 0
			}
			cur = append(cur, r)
			curW += rw
		}
		if len(cur) > 0 {
			lines = append(lines, string(cur))
		}
	}
	if len(lines) == 0 {
		return nil
	}

	out := make([]string, len(lines))
	for i, l := range lines {
		padding := max(0, textWidth-lipgloss.Width(l))
		out[i] = strings.Repeat(" ", indent) + style.Render(l+strings.Repeat(" ", padding))
	}
	return out
}

// ============ 流式增量渲染 ============
// 缓存"稳定前缀"的渲染结果，每帧只重渲染变动的尾部，避免闪烁与 O(n²) 开销。
// 算法移植自 crush (github.com/charmbracelet/crush) 的 streaming_markdown.go。

// streamingMarkdown 保存当前流式消息的增量渲染状态。
type streamingMarkdown struct {
	width              int
	stablePrefix       string // 已确认安全、已缓存渲染的文本前缀
	stablePrefixRender string // stablePrefix 对应的已渲染文本
}

func (s *streamingMarkdown) Reset() {
	s.width = 0
	s.stablePrefix = ""
	s.stablePrefixRender = ""
}

// Render 渲染 Markdown，复用缓存的稳定前缀，只重渲染变动尾部。
func (s *streamingMarkdown) Render(content string, width int) []string {
	initMarkdown(width)
	if glamourRenderer == nil {
		return strings.Split(content, "\n")
	}

	// 宽度变化，或 content 不是已缓存前缀的扩展(发生回退) -> 全量重渲染并重建缓存
	if width != s.width || !strings.HasPrefix(content, s.stablePrefix) {
		s.Reset()
		s.width = width
		out := renderMarkdown(content, width)
		s.tryAdvanceFromEmpty(content, width)
		return out
	}

	// 从后往前找一个安全边界
	boundary := findSafeMarkdownBoundary(content)
	if boundary < 0 {
		return renderMarkdown(content, width) // 找不到 -> 退化为全量渲染(不缓存)
	}

	// 边界已被缓存覆盖 -> 只渲染尾部
	if boundary <= len(s.stablePrefix) {
		trail := content[len(s.stablePrefix):]
		return strings.Split(glueRenders(s.stablePrefixRender, renderTrailing(trail)), "\n")
	}

	// 发现新的安全区块：并入稳定前缀并缓存其渲染
	newChunk := content[len(s.stablePrefix):boundary]
	s.stablePrefixRender = glueRenders(s.stablePrefixRender, renderTrailing(newChunk))
	s.stablePrefix = content[:boundary]

	trail := content[boundary:]
	if trail == "" {
		return strings.Split(s.stablePrefixRender, "\n")
	}
	return strings.Split(glueRenders(s.stablePrefixRender, renderTrailing(trail)), "\n")
}

// tryAdvanceFromEmpty 全量渲染后，尝试从中找出一个安全前缀缓存起来(供后续增量)。
func (s *streamingMarkdown) tryAdvanceFromEmpty(content string, width int) {
	boundary := findSafeMarkdownBoundary(content)
	if boundary <= 0 {
		return
	}
	prefix := content[:boundary]
	out, err := glamourRenderer.Render(prefix)
	if err != nil {
		return
	}
	s.stablePrefix = prefix
	s.stablePrefixRender = trimMargins(out)
	s.width = width
}

func renderTrailing(text string) string {
	if text == "" {
		return ""
	}
	out, err := glamourRenderer.Render(text)
	if err != nil {
		return text
	}
	return trimMargins(out)
}

// glueRenders 把两段已渲染文本拼接，中间补一个空行(glamour 各自渲染会带上下边距，先 trim 再用空行分隔)。
func glueRenders(prefix, trail string) string {
	prefix = trimMargins(prefix)
	trail = trimMargins(trail)
	switch {
	case prefix == "" && trail == "":
		return ""
	case prefix == "":
		return trail
	case trail == "":
		return prefix
	default:
		return prefix + "\n\n" + trail
	}
}

func trimMargins(s string) string {
	return strings.Trim(s, " \t\n")
}

// ============ 安全边界检测 ============
// 目标：在 content 里从后往前找一个位置 p，使 content[:p] 是"结构完整的 Markdown"，
// 渲染一次就不会再变；content[p:] 是还在变的尾部，每帧重渲染。

// findSafeMarkdownBoundary 返回安全边界位置，找不到返回 -1。
func findSafeMarkdownBoundary(content string) int {
	if len(content) == 0 {
		return -1
	}
	// 从末尾往前，逐个"空行位置"尝试，返回第一个安全的
	for p := blankLineBefore(content, len(content)); p > 0; p = blankLineBefore(content, p-1) {
		if !isSafeBoundaryAt(content, p) {
			continue
		}
		return p
	}
	return -1
}

// blankLineBefore 返回 content[:until] 之前最近一个"空行(或全空白行)之后"的位置。
// 即定位到一个空行，返回该空行下一行的起点(空行是天然的段落分隔，是候选边界)。
func blankLineBefore(content string, until int) int {
	if until <= 0 {
		return -1
	}
	end := until
	for end > 0 {
		nl := strings.LastIndexByte(content[:end], '\n')
		if nl < 0 {
			return -1
		}
		prev := strings.LastIndexByte(content[:nl], '\n')
		for prev >= 0 {
			gap := content[prev+1 : nl] // 两个换行之间的那一行
			if isBlankOrSpaces(gap) {
				return nl + 1 // 空行的下一行起点
			}
			break
		}
		end = nl
	}
	return -1
}

func isBlankOrSpaces(s string) bool {
	for i := range len(s) {
		if s[i] != ' ' && s[i] != '\t' {
			return false
		}
	}
	return true
}

// isSafeBoundaryAt 判断在位置 p 切一刀是否安全(4 道检查)。
func isSafeBoundaryAt(content string, p int) bool {
	prefix := content[:p]

	// 1. 代码围栏(```/~~~)必须成对，否则切在代码块中间
	if countFenceLines(prefix)%2 != 0 {
		return false
	}
	// 2. 前缀里(围栏外)不能有"悬空"结构：未闭合的列表项 / HTML 块 / 链接引用定义
	if prefixHasOpenHazard(prefix) {
		return false
	}
	// 3. 前缀最后一行不能是"会开启一个结构"的行(> 引用、列表项、缩进代码、表格、setext 下划线)
	lastLine := lastNonBlankLine(prefix)
	if lastLine != "" && lineOpensConstruct(lastLine) {
		return false
	}
	// 4. 切点之后第一行不能是 setext 下划线(===/---)，否则会把前面的文本变成标题
	if rest := content[p:]; rest != "" {
		first := firstNonBlankLine(rest)
		if isSetextUnderlineCandidate(first) {
			return false
		}
	}
	return true
}

// prefixHasOpenHazard 围栏外若出现列表项/HTML/链接引用定义，视为有未闭合结构(它们可能跨行延续)。
func prefixHasOpenHazard(prefix string) bool {
	inFence := false
	for line := range splitLines(prefix) {
		if isFenceLine(line) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" {
			continue
		}
		if isListItemMarker(trimmed) {
			return true
		}
		if isHTMLBlockOpener(line) {
			return true
		}
		if isLinkRefDefinition(line) {
			return true
		}
	}
	return false
}

func countFenceLines(s string) int {
	n := 0
	for line := range splitLines(s) {
		if isFenceLine(line) {
			n++
		}
	}
	return n
}

// isFenceLine 是否代码围栏行(>=3 个 ` 或 ~，最多 3 空格缩进)。
func isFenceLine(line string) bool {
	i := 0
	for i < len(line) && i < 3 && line[i] == ' ' {
		i++
	}
	if i >= len(line) {
		return false
	}
	c := line[i]
	if c != '`' && c != '~' {
		return false
	}
	run := 0
	for i < len(line) && line[i] == c {
		i++
		run++
	}
	return run >= 3
}

func lastNonBlankLine(s string) string {
	last := ""
	for line := range splitLines(s) {
		if strings.TrimSpace(line) != "" {
			last = line
		}
	}
	return last
}

func firstNonBlankLine(s string) string {
	for line := range splitLines(s) {
		if strings.TrimSpace(line) != "" {
			return line
		}
	}
	return ""
}

// splitLines 迭代器：yield 每一行(不含换行符)。用 range-over-func。
func splitLines(s string) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		start := 0
		for i := 0; i < len(s); i++ {
			if s[i] == '\n' {
				if !yield(s[start:i]) {
					return
				}
				start = i + 1
			}
		}
		if start <= len(s)-1 {
			yield(s[start:])
		}
	}
}

// lineOpensConstruct 这行是否会"开启"一个可能延续的结构。
func lineOpensConstruct(line string) bool {
	if len(line) > 0 && line[0] == '\t' {
		return true
	}
	if strings.HasPrefix(line, "    ") {
		return true
	}
	trimmed := strings.TrimLeft(line, " \t")
	if trimmed == "" {
		return false
	}
	if trimmed[0] == '>' {
		return true
	}
	if isListItemMarker(trimmed) {
		return true
	}
	if strings.ContainsRune(line, '|') {
		return true // 可能是表格
	}
	if isSetextUnderlineCandidate(trimmed) {
		return true
	}
	return false
}

// isListItemMarker 是否列表标记(- * + 或 数字./))。
func isListItemMarker(line string) bool {
	if line == "" {
		return false
	}
	c := line[0]
	if c == '-' || c == '*' || c == '+' {
		return len(line) >= 2 && (line[1] == ' ' || line[1] == '\t')
	}
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i == 0 || i > 9 || i >= len(line) {
		return false
	}
	if line[i] != '.' && line[i] != ')' {
		return false
	}
	return i+1 < len(line) && (line[i+1] == ' ' || line[i+1] == '\t')
}

// isSetextUnderlineCandidate 是否 setext 标题下划线(全是 = 或全是 -)。
func isSetextUnderlineCandidate(line string) bool {
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	if i == len(line) {
		return false
	}
	c := line[i]
	if c != '=' && c != '-' {
		return false
	}
	j := i
	for j < len(line) && line[j] == c {
		j++
	}
	for j < len(line) {
		if line[j] != ' ' && line[j] != '\t' {
			return false
		}
		j++
	}
	return j-i >= 1
}

// isHTMLBlockOpener 是否 HTML 块开始行。
func isHTMLBlockOpener(line string) bool {
	i := 0
	for i < len(line) && i < 3 && line[i] == ' ' {
		i++
	}
	rest := line[i:]
	if len(rest) < 2 || rest[0] != '<' {
		return false
	}
	if strings.HasPrefix(rest, "<!--") {
		return true
	}
	if strings.HasPrefix(rest, "<?") {
		return true
	}
	if strings.HasPrefix(rest, "<![CDATA[") {
		return true
	}
	if len(rest) >= 3 && rest[1] == '!' && isASCIILetter(rest[2]) {
		return true
	}
	low := strings.ToLower(rest)
	for _, t := range []string{"<script", "<pre", "<style", "<textarea"} {
		if strings.HasPrefix(low, t) {
			next := byte(0)
			if len(low) > len(t) {
				next = low[len(t)]
			}
			if next == 0 || next == ' ' || next == '\t' || next == '>' {
				return true
			}
		}
	}
	j := 1
	if j < len(rest) && rest[j] == '/' {
		j++
	}
	if j >= len(rest) || !isASCIILetter(rest[j]) {
		return false
	}
	return true
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// isLinkRefDefinition 是否链接引用定义([label]: url)。
func isLinkRefDefinition(line string) bool {
	i := 0
	for i < len(line) && i < 3 && line[i] == ' ' {
		i++
	}
	if i >= len(line) || line[i] != '[' {
		return false
	}
	i++
	labelStart := i
	for i < len(line) && line[i] != ']' {
		i++
	}
	if i >= len(line) || i == labelStart {
		return false
	}
	i++
	if i >= len(line) || line[i] != ':' {
		return false
	}
	i++
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	return i < len(line)
}
