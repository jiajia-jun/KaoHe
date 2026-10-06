package indexer

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestChunkKeepsEveryRune(t *testing.T) {
	// 切片的长度单位是字符而不是字节：只要有一处按字节切，
	// 中文就会变成乱码，而乱码进模型得到的向量是没有意义的
	text := strings.Repeat("这是一句用来测试切片的中文。", 120)
	chunks, truncated := Chunk(text)
	if truncated {
		t.Fatal("这个长度不该触发截断")
	}
	if len(chunks) < 2 {
		t.Fatalf("这段文本应当切成多片，实际 %d 片", len(chunks))
	}
	for i, c := range chunks {
		if !utf8.ValidString(c) {
			t.Fatalf("第 %d 片不是合法 UTF-8：%q", i, c)
		}
		if n := utf8.RuneCountInString(c); n > ChunkSize {
			t.Fatalf("第 %d 片有 %d 个字符，超过上限 %d", i, n, ChunkSize)
		}
	}
}

func TestChunkOverlapsSoBoundaryAnswerIsNotSplit(t *testing.T) {
	// 一个问题的答案正好压在片段边界上时，没有重叠就两边都只沾一半
	line := strings.Repeat("甲", 39) + "。" // 一行 40 个字符
	lines := make([]string, 40)
	for i := range lines {
		lines[i] = line
	}
	chunks, _ := Chunk(strings.Join(lines, "\n"))
	if len(chunks) < 2 {
		t.Fatalf("应当切成两片以上，实际 %d 片", len(chunks))
	}
	for i := 1; i < len(chunks); i++ {
		// 上一片的结尾应当原样出现在这一片的开头
		tail := lastRunes(chunks[i-1], 10)
		if !strings.Contains(firstRunes(chunks[i], 45), tail) {
			t.Fatalf("第 %d 片开头没有带上第 %d 片的结尾 %q：%q",
				i, i-1, tail, firstRunes(chunks[i], 45))
		}
	}
}

func TestChunkNeverExceedsLimit(t *testing.T) {
	// 重叠尾巴只有在装得下时才带上；带不下就宁可不要重叠，
	// 也不能让片段超过 ChunkSize —— 那是留给模型 token 上限的余量
	inputs := []string{
		strings.Repeat("甲", 39) + "。",  // 恰好一行
		strings.Repeat("甲", ChunkSize), // 恰好一个满片
		strings.Repeat("甲", ChunkSize) + "\n" + strings.Repeat("乙", ChunkSize),
		strings.Repeat("没有句末标点的一整行", 200), // 硬切的路径
		strings.Repeat("一句完整的话。", 500),    // 按句末标点切的路径
	}
	for n, text := range inputs {
		chunks, _ := Chunk(text)
		for i, c := range chunks {
			if got := utf8.RuneCountInString(c); got > ChunkSize {
				t.Fatalf("第 %d 组第 %d 片有 %d 个字符，超过上限 %d", n, i, got, ChunkSize)
			}
		}
	}
}

func TestChunkSplitsOnLineBoundary(t *testing.T) {
	// 语料是文档，一行往往就是一个完整条目；从行中间切开会让两片都读不出意思
	line := strings.Repeat("条目内容", 20) // 80 个字符
	text := strings.Join([]string{line, line, line, line, line, line}, "\n")
	chunks, _ := Chunk(text)

	for i, c := range chunks {
		// 每片去掉末尾的重叠尾巴后，应当由完整的行拼成：
		// 也就是说片内不应当出现「半行」——用行边界检查：每片都以完整行结尾
		if strings.HasSuffix(c, "条目内") || strings.HasSuffix(c, "条目") {
			t.Fatalf("第 %d 片切在了行中间：%q", i, c)
		}
	}
}

func TestChunkSplitsLongLineOnSentenceEnd(t *testing.T) {
	// 单行超过上限时退到句末标点，而不是硬切
	sentence := strings.Repeat("这是一句话。", 100) // 600 个字符，超过 ChunkSize
	chunks, _ := Chunk(sentence)
	if len(chunks) < 2 {
		t.Fatal("超长行应当被拆开")
	}
	if !strings.HasSuffix(chunks[0], "。") {
		t.Fatalf("应当在句末标点处切开，实际结尾：%q", lastRunes(chunks[0], 10))
	}
}

func TestChunkDropsTinyTail(t *testing.T) {
	// 太短的尾巴通常是上一片切剩下的半个句子，单独成片只会污染检索结果
	text := strings.Repeat("甲", ChunkSize) + "\n短"
	chunks, _ := Chunk(text)
	for _, c := range chunks {
		if utf8.RuneCountInString(c) < minChunkRunes {
			t.Fatalf("不该留下这么短的片段：%q", c)
		}
	}
}

func TestChunkEmptyInput(t *testing.T) {
	for _, text := range []string{"", "   ", "\n\n\t\n"} {
		chunks, truncated := Chunk(text)
		if len(chunks) != 0 || truncated {
			t.Fatalf("空白正文 %q 不该产生片段，得到 %d 片", text, len(chunks))
		}
	}
}

func TestChunkCapsHugeDocument(t *testing.T) {
	// 20 MiB 的中文文本切成 400 字一段有五万多段，写进 pgvector 要占几百 MB，
	// 必须在切的时候就封顶，而不是等写库时才发现
	text := strings.Repeat("很长的一段内容。\n", 200000)
	chunks, truncated := Chunk(text)
	if !truncated {
		t.Fatal("超出上限时应当报告截断")
	}
	if len(chunks) != maxChunksPerDocument {
		t.Fatalf("片段数应当封顶在 %d，实际 %d", maxChunksPerDocument, len(chunks))
	}
}

func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

func lastRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = r[len(r)-n:]
	}
	return string(r)
}
