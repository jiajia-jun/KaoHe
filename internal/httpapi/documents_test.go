package httpapi

import (
	"slices"
	"sort"
	"strings"
	"testing"

	"KaoHe/internal/filekind"
)

// 这条用例守的是一个真实发生过的漂移：.markdown 已在 filekind 的 registry 里注册、
// 也能正常上传，但「暂不支持该文件格式」的提示里一直只写 .md ——
// 用户照着提示把扩展名改成 .markdown，反而还是被拒。
//
// 断言写成「文案里的清单 == registry」而不是「包含某个字符串」：
// 后者会让 .md 命中 .markdown 而永远通过，正好漏掉这类前缀包含的漂移。
func TestUnsupportedExtMessageCoversEveryRegisteredExt(t *testing.T) {
	msg := unsupportedExtMessage()

	_, list, ok := strings.Cut(msg, "目前支持 ")
	if !ok {
		t.Fatalf("文案格式变了，用例取不到扩展名清单：%s", msg)
	}

	got := strings.Split(list, " / ")
	want := filekind.SupportedExtensions()
	sort.Strings(want)

	if !slices.Equal(got, want) {
		t.Fatalf("文案里的清单与 registry 不一致\n文案：%v\n清单：%v\n提示：%s", got, want, msg)
	}
}
