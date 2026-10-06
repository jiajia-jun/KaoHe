package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"KaoHe/internal/logging"
)

// loggerKey 是请求级 logger 在 gin.Context 里的键。
//
// gin 的 Context.Keys 是 map[any]any，用私有空结构体当键就不会和其他中间件
// 塞进去的字符串键撞上。
type loggerKey struct{}

const (
	requestIDHeader = "X-Request-Id"
	// maxRequestIDLen 限制客户端给的 id 长度：它既会进日志也会进响应头，
	// 不设上限的话一个超长的头就足以把两边都撑坏。
	maxRequestIDLen = 64
)

// LoggerFrom 取本请求的 logger；没经过 RequestID 中间件时返回 Nop。
//
// 这个取值函数是「请求链路」这一项能成立的关键：failInternal 是个没有 *Server
// 接收者的自由函数，却又是所有 handler 错误路径的汇合点，
// 只有把子 logger 放进 gin 上下文，它才能零改动地带上 requestId。
func LoggerFrom(c *gin.Context) *zap.Logger {
	return loggerFromOr(c, nil)
}

// loggerFromOr 与 LoggerFrom 相同，但在上下文里没有 logger 时回落到 fallback。
//
// panic 与访问日志都不该因为「少注册了一个中间件」而变成一条空日志，
// 所以这两处用带兜底的版本。
func loggerFromOr(c *gin.Context, fallback *zap.Logger) *zap.Logger {
	if v, ok := c.Get(loggerKey{}); ok {
		if l, ok := v.(*zap.Logger); ok {
			return l
		}
	}
	if fallback != nil {
		return fallback
	}
	return zap.NewNop()
}

// RequestID 为每个请求确定一个 id：沿用客户端传的，没有就生成一个。
// 它会回写到响应头，并把带该字段的子 logger 放进上下文。
//
// 注册顺序必须在 AccessLog 之前，否则访问日志里就没有 requestId，
// 一次请求产生的几行日志也就串不到一起。
func RequestID(base *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := sanitizeRequestID(c.GetHeader(requestIDHeader))
		if id == "" {
			id = newRequestID()
		}
		// 回显出去，前端与验收脚本据此把一次请求和服务端日志对上。
		c.Header(requestIDHeader, id)
		c.Set(loggerKey{}, base.With(zap.String("requestId", id)))
		c.Next()
	}
}

// sanitizeRequestID 只放行长度受限的 [A-Za-z0-9._-]，不合规就当作没给。
//
// 客户端给的字符串直接进日志就是日志注入：夹一个换行就能凭空伪造出一整行，
// 让日志整体不再可信；直接进响应头则可能撑爆响应。
// 所以这里用白名单而不是过滤掉几个危险字符 —— 后者永远漏得掉一个。
func sanitizeRequestID(s string) string {
	if s == "" || len(s) > maxRequestIDLen {
		return ""
	}
	for i := 0; i < len(s); i++ {
		switch ch := s[i]; {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9':
		case ch == '-', ch == '_', ch == '.':
		default:
			return ""
		}
	}
	return s
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 读失败是极端情况。退回时间戳仍然足以区分
		// 一次排查里的各个请求，也好过让请求直接失败。
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// AccessLog 记一条访问日志，等级随结果变化。
//
// slow 是「多久算慢」的门槛，0 表示不判断。
func AccessLog(base *zap.Logger, slow time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		cost := time.Since(start)
		status := c.Writer.Status()
		size := c.Writer.Size()
		if size < 0 {
			// 一个字都没写时 gin 的 size 是 -1，直接报出去会让人以为在倒扣字节。
			size = 0
		}

		fields := []zap.Field{
			zap.String("method", c.Request.Method),
			// 用 URL.Path 而不是 RequestURI：后者会把 ?q= 里的检索词一并带进日志，
			// 而那是用户输入，没理由长期留在磁盘上。
			zap.String("path", c.Request.URL.Path),
			zap.Int("status", status),
			zap.Int("bytes", size),
			// gin 默认信任代理，所以这里是 nginx 传来的 X-Forwarded-For。
			// api 没有暴露到宿主机、只有 nginx 能连到它，所以这正是我们想记的客户端地址。
			// 别顺手加 r.SetTrustedProxies(nil)：那会把所有客户端塌缩成 nginx 的容器 IP。
			zap.String("clientIp", c.ClientIP()),
			logging.Cost(cost),
		}

		isSlow := slow > 0 && cost >= slow
		if isSlow {
			// 没有这个字段，一个耗时 3 秒的 200 和一个 404 在日志里就没法区分，
			// 也就筛不出慢请求。
			fields = append(fields, zap.Bool("slow", true))
		}

		l := loggerFromOr(c, base)
		switch {
		case status >= http.StatusInternalServerError:
			l.Error("请求", fields...)
		case status >= http.StatusBadRequest, isSlow:
			// 慢的成功请求也升到 Warn：这样 LOG_LEVEL=warn 时留下的正好全是异常。
			l.Warn("请求", fields...)
		default:
			l.Info("请求", fields...)
		}
	}
}

// Recovery 兜住 handler 里的 panic，只经 zap 一条路记下来。
//
// 不能沿用 gin.Recovery：它直接往 stderr 写，绕过了 slog，换成 zap 之后
// 也同样绕过 zap —— 那恰恰是最需要事后查、却始终进不了日志文件的一类事件。
func Recovery(base *zap.Logger) gin.HandlerFunc {
	// 用 io.Discard 顶掉 gin 自带的 stderr 输出，免得同一个 panic 记在两处。
	return gin.CustomRecoveryWithWriter(io.Discard, func(c *gin.Context, recovered any) {
		loggerFromOr(c, base).Error("请求 panic",
			zap.Any("panic", recovered),
			// 整条栈放进一个字段：它在 JSON 里是转义过的一行，
			// 既保持可 grep，又不会把一次 panic 拆成几十条日志。
			zap.String("stack", string(debug.Stack())),
			zap.String("method", c.Request.Method),
			zap.String("path", c.Request.URL.Path),
		)
		// 必须回 JSON，不能只 AbortWithStatus(500)：
		// 前端与验收脚本依赖 response.go 里那套 {"code": ...} 错误契约。
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
			"code":    codeInternalError,
			"message": "服务器内部错误，请稍后重试",
		})
	})
}
