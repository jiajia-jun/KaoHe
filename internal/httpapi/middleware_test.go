package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// newTestRouter 装一个和 NewRouter 同序的中间件栈，日志收到内存里查看。
func newTestRouter(t *testing.T, slow time.Duration) (*gin.Engine, *observer.ObservedLogs) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)

	r := gin.New()
	r.Use(RequestID(logger), AccessLog(logger, slow), Recovery(logger))
	return r, logs
}

// newRequest 构造一个 GET 测试请求。
//
// 统一走 NewRequestWithContext：不带 context 的那个版本会被 lint 的 noctx 拦下，
// 而且这里每加一个请求都要写一遍 context.Background() 也很吵。
// 不收 method 参数：这些用例全是 GET，等真出现别的动词再加。
func newRequest(target string) *http.Request {
	return httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
}

func TestSanitizeRequestID(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"常规 uuid", "abc-123_DEF.456", "abc-123_DEF.456"},
		{"空串当作没给", "", ""},
		{"超长一律丢弃", strings.Repeat("a", maxRequestIDLen+1), ""},
		{"刚好到上限则保留", strings.Repeat("a", maxRequestIDLen), strings.Repeat("a", maxRequestIDLen)},
		// 换行是重点：它能让客户端在日志里凭空多伪造出一整行。
		{"含换行", "abc\nlevel=error msg=伪造", ""},
		{"含回车", "abc\rdef", ""},
		{"含空格", "abc def", ""},
		{"含引号", `abc"def`, ""},
		{"非 ASCII", "请求标识", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeRequestID(tc.in); got != tc.want {
				t.Errorf("sanitizeRequestID(%q) = %q，期望 %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRequestIDEchoesValidID(t *testing.T) {
	r, logs := newTestRouter(t, 0)
	r.GET("/ping", func(c *gin.Context) { c.String(http.StatusOK, "pong") })

	req := newRequest("/ping")
	req.Header.Set(requestIDHeader, "client-supplied-42")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// 回显出去，前端与验收脚本才能把一次请求和服务端日志对上
	if got := w.Header().Get(requestIDHeader); got != "client-supplied-42" {
		t.Errorf("响应头应回显 requestId，实际 %q", got)
	}
	if logs.FilterField(zap.String("requestId", "client-supplied-42")).Len() == 0 {
		t.Error("访问日志里应当带上 requestId")
	}
}

func TestRequestIDReplacesHostileID(t *testing.T) {
	r, logs := newTestRouter(t, 0)
	r.GET("/ping", func(c *gin.Context) { c.String(http.StatusOK, "pong") })

	req := newRequest("/ping")
	req.Header.Set(requestIDHeader, "evil\nlevel=error msg=这是伪造的一行")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	got := w.Header().Get(requestIDHeader)
	if strings.ContainsAny(got, "\n\r") {
		t.Fatalf("响应头里不该出现换行: %q", got)
	}
	// 客户端给的 id 被拒之后必须换成自己生成的，而不是留空
	if got == "" {
		t.Fatal("非法 requestId 应当被替换成新生成的，而不是留空")
	}
	// 关键：伪造的那一行**不能**作为独立条目出现
	for _, e := range logs.All() {
		if strings.Contains(e.Message, "这是伪造的一行") {
			t.Errorf("客户端内容泄漏成了日志消息: %+v", e)
		}
	}
}

// 这是本次顺路修掉的一个真 bug。
//
// 原先写的是 r.Use(gin.Recovery(), accessLog())：先注册的更靠外，
// 所以 Recovery 包住了 accessLog，panic 会把 accessLog 的栈帧一起掀掉，
// 于是**出错最严重的那次请求反而没有访问日志**。
// 这个用例钉住正确的顺序，谁把两个中间件调换回去就会红。
func TestPanicStillProducesAccessLog(t *testing.T) {
	r, logs := newTestRouter(t, 0)
	r.GET("/boom", func(_ *gin.Context) { panic("模拟的崩溃") })

	req := newRequest("/boom")
	w := httptest.NewRecorder()

	// panic 不该冒到测试进程里 —— 冒出来就说明 Recovery 没接住
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("panic 应当返回 500，实际 %d", w.Code)
	}

	// 返回体必须是那套标准错误契约，前端与验收脚本都依赖它
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("panic 的响应体应当是 JSON: %v，原始内容 %q", err, w.Body.String())
	}
	if body["code"] != codeInternalError {
		t.Errorf("panic 的响应体应当是 %s，实际 %q", codeInternalError, body["code"])
	}

	panicEntries := logs.FilterMessage("请求 panic").All()
	if len(panicEntries) != 1 {
		t.Fatalf("应当恰好记一条 panic 日志，实际 %d 条", len(panicEntries))
	}
	if panicEntries[0].Level != zapcore.ErrorLevel {
		t.Errorf("panic 应当记成 Error，实际 %v", panicEntries[0].Level)
	}
	if len(panicEntries[0].ContextMap()["stack"].(string)) == 0 {
		t.Error("panic 日志里必须带上堆栈，否则事后无从查起")
	}

	// 真正的断言：访问日志也在
	accessEntries := logs.FilterMessage("请求").All()
	if len(accessEntries) != 1 {
		t.Fatalf("panic 的请求也必须留下一条访问日志，实际 %d 条", len(accessEntries))
	}
	if got := accessEntries[0].ContextMap()["status"]; got != int64(http.StatusInternalServerError) {
		t.Errorf("访问日志里的 status 应为 500，实际 %v", got)
	}
}

func TestAccessLogLevelFollowsStatus(t *testing.T) {
	cases := []struct {
		status int
		want   zapcore.Level
	}{
		{http.StatusOK, zapcore.InfoLevel},
		{http.StatusNotFound, zapcore.WarnLevel},
		{http.StatusConflict, zapcore.WarnLevel},
		{http.StatusInternalServerError, zapcore.ErrorLevel},
	}

	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			r, logs := newTestRouter(t, 0)
			r.GET("/x", func(c *gin.Context) { c.Status(tc.status) })

			r.ServeHTTP(httptest.NewRecorder(), newRequest("/x"))

			entries := logs.FilterMessage("请求").All()
			if len(entries) != 1 {
				t.Fatalf("期望 1 条访问日志，实际 %d 条", len(entries))
			}
			if entries[0].Level != tc.want {
				t.Errorf("%d 应记成 %v，实际 %v", tc.status, tc.want, entries[0].Level)
			}
		})
	}
}

// 慢请求即使成功也要升到 Warn，并带上 slow=true ——
// 没有这个字段，一个耗时 3 秒的 200 和一个 404 在日志里就没法区分。
func TestAccessLogMarksSlowRequest(t *testing.T) {
	r, logs := newTestRouter(t, time.Nanosecond)
	r.GET("/slow", func(c *gin.Context) {
		time.Sleep(2 * time.Millisecond)
		c.String(http.StatusOK, "ok")
	})

	r.ServeHTTP(httptest.NewRecorder(), newRequest("/slow"))

	entries := logs.FilterMessage("请求").All()
	if len(entries) != 1 {
		t.Fatalf("期望 1 条访问日志，实际 %d 条", len(entries))
	}
	if entries[0].Level != zapcore.WarnLevel {
		t.Errorf("慢请求应当记成 Warn，实际 %v", entries[0].Level)
	}
	if entries[0].ContextMap()["slow"] != true {
		t.Errorf("慢请求应当带 slow=true，实际 %v", entries[0].ContextMap())
	}

	// 门槛设成 0 表示不判断，此时再慢也不该被标记
	r2, logs2 := newTestRouter(t, 0)
	r2.GET("/slow", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	r2.ServeHTTP(httptest.NewRecorder(), newRequest("/slow"))
	if _, marked := logs2.All()[0].ContextMap()["slow"]; marked {
		t.Error("门槛为 0 时不该标记慢请求")
	}
}

// 检索词是用户输入，没理由长期留在磁盘上：路径里只记 Path，不记 query。
func TestAccessLogOmitsQueryString(t *testing.T) {
	r, logs := newTestRouter(t, 0)
	r.GET("/search", func(c *gin.Context) { c.Status(http.StatusOK) })

	r.ServeHTTP(httptest.NewRecorder(),
		newRequest("/search?q=机密检索词"))

	path, _ := logs.FilterMessage("请求").All()[0].ContextMap()["path"].(string)
	if path != "/search" {
		t.Errorf("访问日志只该记路径，实际 %q", path)
	}
}

// LoggerFrom 在没经过 RequestID 中间件时必须给出一个能用的 Nop logger，
// 否则任何一条走上下文取 logger 的日志路径都会 panic。
func TestLoggerFromFallsBackToNop(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/x", func(c *gin.Context) {
		LoggerFrom(c).Error("没有中间件时也不该 panic")
		c.Status(http.StatusOK)
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, newRequest("/x"))

	// 能走到这里就说明那条 Error 没有 panic 出来；
	// 状态码顺带证明 handler 是跑完全程的，而不是中途被哪一层截断。
	if w.Code != http.StatusOK {
		t.Errorf("handler 应当正常跑完，实际状态码 %d", w.Code)
	}
}
