package memory

import (
	"strings"
	"unicode"

	"einoclaw-build/internal/message"
)

// FTS5 的 trigram 分词器要求查询词至少 3 个字符，且 MATCH 的参数是一门有语法的小语言：
// `?` `(` `)` `-` `*` `"` 直接出现在里面会让整条查询报语法错。用户的问句里全是这些字符，
// 所以**永远不要把原文交给 MATCH** —— 先切成词、丢掉太短的、给每一项加引号，再用 OR 连起来。
const (
	maxQueryTerms       = 24   // 查询项上限：再多对召回质量没帮助，只会拖慢
	minTermRunes        = 3    // trigram 的硬要求
	cjkWindow           = 3    // 中文按 3 字滑窗展开成 trigram 能匹配的形式
	maxRecallQueryRunes = 4000 // 召回查询的长度上限
	recallUserTurns     = 3    // 用最近几个用户 turn 构造查询
)

// FTSQuery 把自然语言问句清洗成 FTS5 安全的查询串。
// 返回 "" 表示没有可用实词——调用方应改走兜底召回，而不是拿空串去 MATCH。
func FTSQuery(q string) string {
	terms := make([]string, 0, maxQueryTerms)
	seen := make(map[string]bool, maxQueryTerms)
	add := func(t string) bool {
		if seen[t] || len(terms) >= maxQueryTerms {
			return len(terms) < maxQueryTerms
		}
		seen[t] = true
		terms = append(terms, t)
		return true
	}
	for _, tok := range tokenize(q) {
		for _, t := range expandTerm(tok) {
			if !add(t) {
				break
			}
		}
		if len(terms) >= maxQueryTerms {
			break
		}
	}
	if len(terms) == 0 {
		return ""
	}
	quoted := make([]string, len(terms))
	for i, t := range terms {
		quoted[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
	}
	return strings.Join(quoted, " OR ")
}

// tokenize 按空白与标点切词（字母数字与 CJK 之外的一律当分隔符）。
func tokenize(q string) []string {
	return strings.FieldsFunc(q, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// expandTerm 把一个词展开成可用于 trigram 匹配的项：
// CJK 词按 3 字滑窗展开；拉丁词整体保留（长度不足则丢弃）。
func expandTerm(tok string) []string {
	r := []rune(tok)
	if len(r) < minTermRunes {
		return nil
	}
	if !hasCJK(r) {
		return []string{strings.ToLower(tok)}
	}
	out := make([]string, 0, len(r)-cjkWindow+1)
	for i := 0; i+cjkWindow <= len(r); i++ {
		out = append(out, string(r[i:i+cjkWindow]))
	}
	return out
}

func hasCJK(rs []rune) bool {
	for _, r := range rs {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) {
			return true
		}
	}
	return false
}

// Similarity 返回两段文本的 trigram Jaccard 相似度（0–1），用于近重复判定。
// 选 trigram 而不是编辑距离：对中英文都一致，且与 FTS 用的是同一套切分直觉。
func Similarity(a, b string) float64 {
	sa, sb := trigrams(a), trigrams(b)
	if len(sa) == 0 || len(sb) == 0 {
		if a != "" && a == b {
			return 1
		}
		return 0
	}
	inter := 0
	for g := range sa {
		if sb[g] {
			inter++
		}
	}
	union := len(sa) + len(sb) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// trigrams 把文本归一（小写、压缩空白）后切成 3-rune 集合。
func trigrams(s string) map[string]bool {
	r := []rune(strings.Join(strings.Fields(strings.ToLower(s)), " "))
	if len(r) < 3 {
		return nil
	}
	out := make(map[string]bool, len(r))
	for i := 0; i+3 <= len(r); i++ {
		out[string(r[i:i+3])] = true
	}
	return out
}

// BuildRecallQuery 用最近几个用户 turn 拼召回查询。
// 只取用户说的话：assistant 的长篇输出会把查询稀释成噪声。
func BuildRecallQuery(history []message.Message) string {
	var texts []string
	for i := len(history) - 1; i >= 0 && len(texts) < recallUserTurns; i-- {
		if history[i].Role != message.RoleUser {
			continue
		}
		if t := strings.TrimSpace(messageText(history[i])); t != "" {
			texts = append(texts, t)
		}
	}
	// 倒序收集的，反转回时间序
	for i, j := 0, len(texts)-1; i < j; i, j = i+1, j-1 {
		texts[i], texts[j] = texts[j], texts[i]
	}
	q := strings.Join(texts, "\n")
	if r := []rune(q); len(r) > maxRecallQueryRunes {
		q = string(r[len(r)-maxRecallQueryRunes:]) // 留最近的部分
	}
	return q
}

func messageText(m message.Message) string {
	var sb strings.Builder
	for _, b := range m.Blocks {
		if b.Kind == message.BlockText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}
