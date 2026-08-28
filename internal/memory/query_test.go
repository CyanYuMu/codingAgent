package memory

import (
	"strings"
	"testing"

	"einoclaw-build/internal/message"
)

func TestFTSQueryEscapesAndDropsShortTerms(t *testing.T) {
	got := FTSQuery("构建命令是什么？(go build)")
	for _, bad := range []string{"?", "(", ")", "？"} {
		if strings.Contains(got, bad) {
			t.Fatalf("清洗后仍含裸标点 %q：%s", bad, got)
		}
	}
	if !strings.Contains(got, `"build"`) {
		t.Fatalf("长度 ≥3 的英文词应保留：%s", got)
	}
	if strings.Contains(got, `"go"`) {
		t.Fatalf("trigram 分词器下 <3 字符的词应丢弃：%s", got)
	}
	for _, part := range strings.Split(got, " OR ") {
		if !strings.HasPrefix(part, `"`) || !strings.HasSuffix(part, `"`) {
			t.Fatalf("每一项都应被引号包裹：%q", part)
		}
	}
}

func TestFTSQueryCJKSlidingWindow(t *testing.T) {
	got := FTSQuery("记忆系统")
	if !strings.Contains(got, `"记忆系"`) || !strings.Contains(got, `"忆系统"`) {
		t.Fatalf("CJK 应按 3 字滑窗展开：%s", got)
	}
	if q := FTSQuery("记忆"); q != "" {
		t.Fatalf("2 字 CJK 在 trigram 下无法匹配，应丢弃：%q", q)
	}
}

func TestFTSQueryQuoteEscape(t *testing.T) {
	got := FTSQuery(`他说 "构建失败" 了`)
	if strings.Contains(got, `""构建失败""`) {
		return // 引号被转义即可
	}
	if strings.Count(got, `"`)%2 != 0 {
		t.Fatalf("引号未成对，会让 FTS 语法出错：%s", got)
	}
}

func TestFTSQueryEmptyWhenNoUsableTerms(t *testing.T) {
	for _, in := range []string{"", "？！。", "go", "  ", "a b"} {
		if got := FTSQuery(in); got != "" {
			t.Fatalf("FTSQuery(%q) = %q，应为空以便走兜底召回", in, got)
		}
	}
}

func TestFTSQueryCapsTerms(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 60; i++ {
		sb.WriteString("token")
		sb.WriteByte(byte('a' + i%26))
		sb.WriteByte(' ')
	}
	got := FTSQuery(sb.String())
	if n := strings.Count(got, " OR ") + 1; n > maxQueryTerms {
		t.Fatalf("查询项数 = %d，应上限 %d", n, maxQueryTerms)
	}
}

func TestFTSQueryDedupes(t *testing.T) {
	got := FTSQuery("build build build")
	if strings.Count(got, `"build"`) != 1 {
		t.Fatalf("重复词应去重：%s", got)
	}
}

func TestSimilarity(t *testing.T) {
	cases := []struct {
		name, a, b string
		min, max   float64
	}{
		{"完全相同", "用户偏好中文回复", "用户偏好中文回复", 1, 1},
		{"同义改写", "用户偏好中文回复", "用户偏好中文回复。", 0.85, 1},
		{"无关事实", "构建命令是 go build", "用户住在杭州", 0, 0.3},
		{"空串", "", "abc", 0, 0},
		{"都空", "", "", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Similarity(tc.a, tc.b)
			if got < tc.min || got > tc.max {
				t.Fatalf("Similarity(%q,%q) = %.2f，期望 [%.2f, %.2f]", tc.a, tc.b, got, tc.min, tc.max)
			}
		})
	}
}

func TestBuildRecallQueryUsesRecentUserTurns(t *testing.T) {
	msgs := []message.Message{
		message.NewUserMessage("第一轮问题"),
		{Role: message.RoleAssistant, Blocks: []message.ContentBlock{{Kind: message.BlockText, Text: "回答"}}},
		message.NewUserMessage("第二轮问题"),
		message.NewUserMessage("第三轮问题"),
		message.NewUserMessage("第四轮问题"),
	}
	got := BuildRecallQuery(msgs)
	if strings.Contains(got, "第一轮") {
		t.Fatalf("只应取最近 3 个用户 turn：%s", got)
	}
	for _, want := range []string{"第二轮", "第三轮", "第四轮"} {
		if !strings.Contains(got, want) {
			t.Fatalf("缺 %s：%s", want, got)
		}
	}
	if strings.Contains(got, "回答") {
		t.Fatalf("不该带上 assistant 文本：%s", got)
	}
}

func TestBuildRecallQueryClips(t *testing.T) {
	long := strings.Repeat("很长的问题", 2000)
	got := BuildRecallQuery([]message.Message{message.NewUserMessage(long)})
	if len([]rune(got)) > maxRecallQueryRunes {
		t.Fatalf("查询长度 = %d，应截断到 %d", len([]rune(got)), maxRecallQueryRunes)
	}
}
