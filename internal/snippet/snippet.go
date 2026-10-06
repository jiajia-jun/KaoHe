// Package snippet 从命中的正文里截出可读的一小段，供检索结果展示。
//
// 这里刻意不按字节下标切分：中文一个字占三个字节，按字节窗口截出来的是
// 半个字，在界面上显示成乱码。所有窗口计算都在 rune 上做。
package snippet

import (
	"strings"
	"unicode"
)

// DefaultContext 是命中位置前后各保留的字符数。
// 太小看不出上下文，太大在结果列表里一屏放不下几条。
const DefaultContext = 48

// Around 在 text 里找到第一处 query，返回它前后各留 context 个字符的窗口。
//
// 找不到 query 时返回 false：调用方据此决定是不展示片段，还是退回成开头一段。
// 空 query 也返回 false —— 那意味着这次命中不是来自正文，窗口没有意义。
func Around(text, query string, context int) (string, bool) {
	if query == "" || text == "" {
		return "", false
	}
	if context < 1 {
		context = DefaultContext
	}

	body := []rune(text)
	at := indexRunes(body, []rune(query))
	if at < 0 {
		return "", false
	}

	start := max(at-context, 0)
	end := min(at+len([]rune(query))+context, len(body))

	var b strings.Builder
	b.Grow((end - start) * 2)
	// 省略号只在真的被截断时加：一段完整的正文后面挂个「…」会让人以为还有内容
	if start > 0 {
		b.WriteString("…")
	}
	b.WriteString(string(body[start:end]))
	if end < len(body) {
		b.WriteString("…")
	}
	return b.String(), true
}

// indexRunes 在 haystack 里找 needle 的第一处位置，返回 rune 下标。
// 没找到返回 -1；needle 为空返回 -1，避免调用方把空匹配当成命中。
//
// 比较时逐个 rune 做大小写折叠，而不是先把两边整体 ToLower 再比：
// 英文关键词在库里是 ILIKE 匹配的，展示片段时也得找得到，否则会出现
// 「数据库说这行命中了，界面上却标不出是哪一段」。
// 不整体 ToLower 是因为它不保证长度不变（例如 'İ' 折叠后是两个 rune），
// 一旦长度变了，折算回原串的下标就会偏。
func indexRunes(haystack, needle []rune) int {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return -1
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j, r := range needle {
			if unicode.ToLower(haystack[i+j]) != unicode.ToLower(r) {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
