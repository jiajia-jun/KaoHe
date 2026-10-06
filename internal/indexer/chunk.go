package indexer

import (
	"strings"
	"unicode/utf8"
)

// 切片参数。
//
// 长度按「字符」而不是「字节」算：中文一个字符 3 字节，按字节切会把一个汉字劈成两半，
// 切出来的是乱码。模型侧的上限是 512 个 token，中文大致一字一 token，
// 因此 400 字符留出了 [CLS]、[SEP] 与分词差异的余量。
//
// 取值依据：片段太短会把一句话拆散、检索结果读不出上下文；太长则一段里混进多个主题，
// 向量被平均掉、语义定位变糊。400/80 是先验上的合理起点，
// M5 会用真实语料实测「命中片段是否落在正确那一段」来校准这两个数。
const (
	ChunkSize    = 400
	chunkOverlap = 80
	// minChunkRunes 以下的小尾巴直接丢掉，它通常是上一个片段切完剩下的半个句子
	minChunkRunes = 8
	// maxChunksPerDocument 防止一个超大文本把索引过程拖垮：
	// 20 MiB 的中文文本切成 400 字一段有五万多段，写进 pgvector 要占几百 MB。
	maxChunksPerDocument = 2000
)

// Chunk 把正文切成带重叠的片段。
//
// 切在行边界上而不是定长硬切：语料是文档，一行往往就是一个完整条目
// （检查项、接口说明、规范条目），从中间切开会让两个片段都读不出意思。
// 行本身太长时再退到句子边界，最后才硬切。
//
// 相邻片段重叠 chunkOverlap 个字符：一个问题的答案正好落在边界上时，
// 没有重叠就会两边都只沾一半，谁都匹配不上。
func Chunk(text string) (chunks []string, truncated bool) {
	atoms := splitAtoms(text)
	if len(atoms) == 0 {
		return nil, false
	}

	var buf []rune
	flush := func() {
		if len(buf) >= minChunkRunes {
			chunks = append(chunks, string(buf))
		}
	}

	for _, atom := range atoms {
		runes := []rune(atom)
		// 放不下就先把当前片段收掉，并用它的尾部作为下一个片段的开头
		if len(buf) > 0 && len(buf)+1+len(runes) > ChunkSize {
			flush()
			if len(chunks) >= maxChunksPerDocument {
				return chunks, true
			}
			buf = overlapTail(buf)
		}
		if len(buf) > 0 {
			buf = append(buf, '\n')
		}
		buf = append(buf, runes...)
	}
	flush()

	if len(chunks) > maxChunksPerDocument {
		return chunks[:maxChunksPerDocument], true
	}
	return chunks, false
}

// splitAtoms 把正文拆成「一行一段」的最小单元，过长的行再往下拆。
func splitAtoms(text string) []string {
	var atoms []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if utf8.RuneCountInString(line) <= ChunkSize {
			atoms = append(atoms, line)
			continue
		}
		atoms = append(atoms, splitLongLine(line)...)
	}
	return atoms
}

// splitLongLine 处理超长行：优先切在句末标点上，切不动了才按长度硬分。
func splitLongLine(line string) []string {
	var parts []string
	rest := []rune(line)
	for len(rest) > ChunkSize {
		cut := sentenceBoundary(rest[:ChunkSize])
		if cut <= 0 {
			// 整段都没有句末标点（例如一整行英文或代码），只能硬切
			cut = ChunkSize
		}
		parts = append(parts, strings.TrimSpace(string(rest[:cut])))
		rest = rest[cut:]
	}
	if tail := strings.TrimSpace(string(rest)); tail != "" {
		parts = append(parts, tail)
	}
	return parts
}

// sentenceBoundary 在窗口内从后往前找最后一个句末标点，返回切分位置（含标点）。
func sentenceBoundary(window []rune) int {
	for i := len(window) - 1; i > len(window)/2; i-- {
		switch window[i] {
		case '。', '！', '？', '；', '.', '!', '?', ';':
			return i + 1
		}
	}
	return 0
}

// overlapTail 取上一个片段的结尾部分作为下一个片段的开头。
// 起点会向后挪到最近的断句处，免得下一个片段从一个词的中间开始。
func overlapTail(buf []rune) []rune {
	if len(buf) <= chunkOverlap {
		return append([]rune(nil), buf...)
	}
	tail := buf[len(buf)-chunkOverlap:]
	// 只在前半段里找断点：挪得太多就等于没留重叠
	for i := 0; i < len(tail)/2; i++ {
		switch tail[i] {
		case '。', '！', '？', '；', '，', '、', '.', '!', '?', ';', ',', '\n', ' ':
			return append([]rune(nil), tail[i+1:]...)
		}
	}
	return append([]rune(nil), tail...)
}
