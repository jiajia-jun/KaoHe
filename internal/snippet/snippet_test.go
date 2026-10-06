package snippet

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAroundKeepsWholeRunes(t *testing.T) {
	// 中文一个字三个字节：只要窗口是按字节算的，这里就会出现半个字
	text := strings.Repeat("前", 100) + "命中词" + strings.Repeat("后", 100)
	got, ok := Around(text, "命中词", 10)
	if !ok {
		t.Fatal("应当命中")
	}
	if !utf8.ValidString(got) {
		t.Fatalf("截出来的不是合法 UTF-8：%q", got)
	}
	if !strings.Contains(got, "命中词") {
		t.Fatalf("窗口里应当含关键词：%q", got)
	}
	// 前后各 10 个字加关键词本身，再加省略号
	if n := utf8.RuneCountInString(got); n != 10+3+10+2 {
		t.Fatalf("窗口长度应为 25 个字符，实际 %d：%q", n, got)
	}
}

func TestAroundEllipsisOnlyWhenTruncated(t *testing.T) {
	// 短文本整段都在窗口内：不该出现省略号，否则会让人以为后面还有内容
	got, _ := Around("很短的一句话", "句子", 50)
	if strings.Contains(got, "…") {
		t.Fatalf("没有截断就不该加省略号：%q", got)
	}

	// 只截了一头：省略号也只该有一个
	got, _ = Around(strings.Repeat("a", 50)+"target", "target", 5)
	if strings.Count(got, "…") != 1 {
		t.Fatalf("只截了前面，应当只有一个省略号：%q", got)
	}
	if !strings.HasPrefix(got, "…") {
		t.Fatalf("应当从头省略：%q", got)
	}
}

func TestAroundCaseInsensitive(t *testing.T) {
	// 库里的正文匹配用的是 ILIKE，展示时必须找得到，否则界面标不出是哪一段
	got, ok := Around("关于 Index Job 的说明", "index job", 5)
	if !ok {
		t.Fatal("大小写不同也应当命中")
	}
	if !strings.Contains(got, "Index Job") {
		t.Fatalf("应当返回原文里的写法：%q", got)
	}
}

func TestAroundFoldsRunesWithoutChangingLength(t *testing.T) {
	// 'İ' 整体 ToLower 之后是两个 rune，若先把整串折叠再定位，下标会偏。
	// 这里只要确保折叠不改变长度、定位依然正确。
	got, ok := Around("İstanbul 是个城市", "Istanbul", 3)
	if !ok {
		t.Fatal("应当命中")
	}
	if !strings.Contains(got, "İstanbul") {
		t.Fatalf("应当返回原文：%q", got)
	}
}

func TestAroundMisses(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		query string
	}{
		{"正文里没有这个词", "一份和检索无关的说明", "关键词"},
		{"空关键词", "有正文", ""},
		{"空正文", "", "关键词"},
		{"关键词比正文还长", "短", "很长的关键词"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got, ok := Around(c.text, c.query, 10); ok {
				t.Fatalf("应当返回 false，实际得到 %q", got)
			}
		})
	}
}

func TestAroundContextFallback(t *testing.T) {
	// context <= 0 时退回默认值，而不是截出一个 0 长度的空片段
	got, ok := Around(strings.Repeat("甲", 100)+"目标"+strings.Repeat("乙", 100), "目标", 0)
	if !ok {
		t.Fatal("应当命中")
	}
	if utf8.RuneCountInString(got) != DefaultContext*2+2+2 {
		t.Fatalf("应当按 DefaultContext 截取：%q", got)
	}
}
