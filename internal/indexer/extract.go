// Package indexer 把上传的文档变成可检索的文本片段与向量。
package indexer

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

// maxExtractBytes 是抽取正文的字节上限。
// 上传上限是 20 MiB，这里不再额外收紧，只防止读入异常大的输入。
const maxExtractBytes = 20 << 20

// ExtractText 读取文本类文件的内容。
//
// 编码处理：真实场景里的中文 .txt 有相当比例是 GBK（Windows 记事本另存为的默认值），
// 直接按 UTF-8 读会得到满屏替换字符，而且不报错 —— 那种「能索引但搜不到」最难排查。
// 所以按 UTF-8 严格校验，失败再退到 GBK。
//
// Markdown 的标记语法（#、**、列表符号）刻意保留不清洗：
// 检索结果要展示原文片段，清掉标记反而与用户看到的文件内容对不上，
// 而分词器对这些符号本来就不敏感。
func ExtractText(r io.Reader) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxExtractBytes+1))
	if err != nil {
		return "", fmt.Errorf("读取文件内容失败: %w", err)
	}
	if len(raw) > maxExtractBytes {
		return "", fmt.Errorf("文件超过正文抽取上限 %d 字节", maxExtractBytes)
	}

	text, err := decodeText(raw)
	if err != nil {
		return "", err
	}
	return Normalize(text), nil
}

func decodeText(raw []byte) (string, error) {
	// 带 BOM 的 UTF-8 会把 BOM 当成正文的第一个字符，先去掉
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})

	if utf8.Valid(raw) {
		return string(raw), nil
	}

	decoded, _, err := transform.Bytes(simplifiedchinese.GBK.NewDecoder(), raw)
	if err != nil {
		return "", fmt.Errorf("无法识别文件编码，既不是 UTF-8 也不是 GBK: %w", err)
	}
	// GBK 解码器遇到非法字节会替换成 U+FFFD 而不是报错，所以解码成功不代表内容就是文本。
	// 一份被改名成 .txt 的二进制文件会在这里被拦下，而不是变成一堆替换字符进索引。
	if replacementRatio(string(decoded)) > 0.01 {
		return "", fmt.Errorf("文件内容不是可识别的文本")
	}
	return string(decoded), nil
}

// replacementRatio 统计替换字符的占比，用来识别「解出来一堆 U+FFFD」的情况。
func replacementRatio(s string) float64 {
	total, bad := 0, 0
	for _, r := range s {
		total++
		if r == utf8.RuneError {
			bad++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(bad) / float64(total)
}

const (
	// 用转义写，避免这两个字符在编辑器里不可见、被误当成普通空格改掉
	nbsp             = "\u00a0" // 不换行空格，从网页或 Word 复制来的文本里很常见
	ideographicSpace = "\u3000" // 全角空格
)

// Normalize 统一换行与空白，让切片结果不受编辑器差异影响。
func Normalize(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	// 这两种空格若不归一化，会粘在词里，让检索与切片都把它当成词的一部分
	text = strings.ReplaceAll(text, nbsp, " ")
	text = strings.ReplaceAll(text, ideographicSpace, " ")
	// 三个以上连续换行折成两个，段落边界保留、空白不占篇幅
	for strings.Contains(text, "\n\n\n") {
		text = strings.ReplaceAll(text, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(text)
}
