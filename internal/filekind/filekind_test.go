package filekind

import "testing"

func TestLookupIsCaseInsensitive(t *testing.T) {
	// 用户上传的是 .PDF 还是 .pdf 不该影响结果
	for _, ext := range []string{".md", ".MD", ".Md", "md", "MD"} {
		info, ok := Lookup(ext)
		if !ok {
			t.Fatalf("%q 应当被识别", ext)
		}
		if info.Kind != KindText {
			t.Fatalf("%q 应当是文本类，实际 %q", ext, info.Kind)
		}
	}
}

func TestLookupPath(t *testing.T) {
	cases := []struct {
		name string
		kind string
	}{
		{"01_代码提交规范_v2.1.md", KindText},
		{"09_本地部署故障排查_v1.3.txt", KindText},
		{"10_文档分类与归档规范_v1.0.PDF", KindPDF},
		// 存储键是 doc_xxx + 扩展名，判断要能直接吃文件名，也吃存储键
		{"doc_9f3a1c.md", KindText},
	}
	for _, c := range cases {
		info, ok := LookupPath(c.name)
		if !ok {
			t.Fatalf("%q 应当被识别", c.name)
		}
		if info.Kind != c.kind {
			t.Fatalf("%q 应当是 %q，实际 %q", c.name, c.kind, info.Kind)
		}
	}
}

func TestUnsupportedFormat(t *testing.T) {
	for _, ext := range []string{".docx", ".exe", "", ".", ".mdx", ".txt2"} {
		if _, ok := Lookup(ext); ok {
			t.Fatalf("%q 不该被接受", ext)
		}
	}
}

func TestIsExtractableMatchesRegistry(t *testing.T) {
	// HTTP 层用 Lookup 决定收不收，worker 用 IsExtractable 决定抽不抽正文。
	// 这两处必须同源：不一致就会出现「上传时说会索引、取任务时又不认」。
	if IsExtractable(".pdf") {
		t.Fatal("PDF 只做保存与预览，不抽正文")
	}
	for _, ext := range []string{".txt", ".md", ".markdown"} {
		if !IsExtractable(ext) {
			t.Fatalf("%q 应当可抽取正文", ext)
		}
	}
	if IsExtractable(".docx") {
		t.Fatal("不支持的格式不该被当成可抽取")
	}
}

func TestSupportedExtensionsCoversRegistry(t *testing.T) {
	exts := SupportedExtensions()
	if len(exts) != len(registry) {
		t.Fatalf("应当返回全部 %d 个扩展名，实际 %d 个", len(registry), len(exts))
	}
	for _, ext := range exts {
		if _, ok := registry[ext]; !ok {
			t.Fatalf("返回了不在清单里的扩展名 %q", ext)
		}
		if ext[0] != '.' {
			t.Fatalf("扩展名应当带点号：%q", ext)
		}
	}
}
