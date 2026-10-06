package httpapi

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// 错误码常量。前端据此区分处理方式，不必解析中文文案。
const (
	codeBadRequest  = "bad_request"
	codeNotFound    = "not_found"
	codeTooLarge    = "file_too_large"
	codeUnsupported = "unsupported_type"
	// codeConflict 表示请求本身合法，但与当前数据状态冲突（同级重名、分类还有子分类）。
	// 与 bad_request 分开，界面才能把这类问题就地提示在输入框旁边。
	codeConflict      = "conflict"
	codeInternalError = "internal_error"
)

// fail 统一错误响应，保证前端在任何失败路径上都能拿到同样的 JSON 结构。
// 错误信息只描述用户能理解的原因，不含磁盘路径、SQL 或堆栈。
func fail(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{
		"code":    code,
		"message": message,
	})
}

func failInternal(c *gin.Context, err error) {
	// 详细原因（含 SQL、磁盘路径）只进服务端日志，不回传给客户端
	slog.Error("请求处理失败",
		"method", c.Request.Method,
		"path", c.Request.URL.Path,
		"err", err,
	)
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
		"code":    codeInternalError,
		"message": "服务器内部错误，请稍后重试",
	})
}

// parseIDParam 解析路径参数为正整数，失败时直接写 400 并返回 false。
func parseIDParam(c *gin.Context, name string) (int64, bool) {
	raw := c.Param(name)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		fail(c, http.StatusBadRequest, codeBadRequest, fmt.Sprintf("参数 %s 必须是正整数", name))
		return 0, false
	}
	return id, true
}

// humanSize 把字节数格式化成用户能读懂的写法，用于超限提示。
func humanSize(n int64) string {
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
	)
	switch {
	case n >= gib:
		return fmt.Sprintf("%.1f GiB", float64(n)/float64(gib))
	case n >= mib:
		v := float64(n) / float64(mib)
		if v == float64(int64(v)) {
			return fmt.Sprintf("%d MiB", int64(v))
		}
		return fmt.Sprintf("%.1f MiB", v)
	case n >= kib:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(kib))
	default:
		return fmt.Sprintf("%d 字节", n)
	}
}
