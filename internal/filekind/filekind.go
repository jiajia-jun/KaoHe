// Package filekind 定义平台认可的格式，以及每种格式的能力边界。
//
// 单独成包是因为这些判断被两处用到，而它们必须一致：
// HTTP 层用它决定收不收这次上传、要不要建索引任务；
// worker 用它决定要不要抽取正文。两边各写一份清单，早晚会漏改一处，
// 出现「上传时说会索引、取任务时又不认」这种最难查的不一致。
package filekind

import "path/filepath"

const (
	// KindPDF 只做保存、下载与在线预览，不抽取正文
	KindPDF = "pdf"
	// KindText 可抽取正文，参与关键词正文检索与语义检索
	KindText = "text"
)

// Info 描述一种受支持格式的两项属性。
type Info struct {
	// ContentType 是下载与预览时回给浏览器的类型
	ContentType string
	// Kind 决定该格式有哪些能力
	Kind string
}

// registry 是唯一的一份格式清单。target 要求至少覆盖 PDF、TXT、Markdown。
var registry = map[string]Info{
	".pdf":      {ContentType: "application/pdf", Kind: KindPDF},
	".txt":      {ContentType: "text/plain; charset=utf-8", Kind: KindText},
	".md":       {ContentType: "text/markdown; charset=utf-8", Kind: KindText},
	".markdown": {ContentType: "text/markdown; charset=utf-8", Kind: KindText},
}

// Lookup 按扩展名查格式。ext 不区分大小写，调用方传入的应带点号。
func Lookup(ext string) (Info, bool) {
	info, ok := registry[normalizeExt(ext)]
	return info, ok
}

// LookupPath 直接按文件名或存储键判断，省得调用方自己再切一次扩展名。
func LookupPath(name string) (Info, bool) {
	return Lookup(filepath.Ext(name))
}

// IsExtractable 表示该格式的正文会被抽取并建立索引。
// PDF 返回 false 是本设计的明确取舍，不是「暂时没做」。
func IsExtractable(ext string) bool {
	info, ok := Lookup(ext)
	return ok && info.Kind == KindText
}

// SupportedExtensions 返回全部允许上传的扩展名，供 /config 接口下发给前端，
// 避免前端自己再维护一份清单。
func SupportedExtensions() []string {
	exts := make([]string, 0, len(registry))
	for ext := range registry {
		exts = append(exts, ext)
	}
	return exts
}

func normalizeExt(ext string) string {
	if ext == "" {
		return ext
	}
	if ext[0] != '.' {
		ext = "." + ext
	}
	return toLower(ext)
}

// toLower 只处理扩展名，不需要 unicode 包那套完整规则。
func toLower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
