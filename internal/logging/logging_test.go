package logging

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// tempDir 返回一个测试专属目录。
//
// 每个用例都必须显式给 Dir，**不能**落到 Options 的零值上 ——
// 那样测试会去写进程的默认路径，在 Windows 上就是盘符根目录。
func tempDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// readLines 读出日志文件里的每一行。
func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304：path 来自 t.TempDir()，不是外部输入
	if err != nil {
		t.Fatalf("读取日志文件失败: %v", err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func TestLevelParsing(t *testing.T) {
	cases := []struct {
		env  string
		want zapcore.Level
	}{
		{"", zapcore.InfoLevel}, // 空串是「没配」，zapcore 把它解析成 info
		{"debug", zapcore.DebugLevel},
		{"info", zapcore.InfoLevel},
		{"warn", zapcore.WarnLevel},
		{"error", zapcore.ErrorLevel},
		{"WARN", zapcore.WarnLevel}, // 大小写不敏感
	}

	for _, tc := range cases {
		t.Run("level="+tc.env, func(t *testing.T) {
			l, cleanup, err := New("api", Options{Level: tc.env})
			if err != nil {
				t.Fatalf("New 返回错误: %v", err)
			}
			defer func() { _ = cleanup() }()

			// 断言的是这个等级真的会放行/拦截，而不是内部字段被设成了什么。
			if got := l.Core().Enabled(tc.want); !got {
				t.Errorf("等级 %q 下应该放行 %v", tc.env, tc.want)
			}
			if tc.want > zapcore.DebugLevel {
				if l.Core().Enabled(tc.want - 1) {
					t.Errorf("等级 %q 下不该放行比它更低的级别", tc.env)
				}
			}
		})
	}
}

func TestInvalidLevelIsRejected(t *testing.T) {
	// 配错等级必须在启动时就报错，而不是等到第一次打日志时静默失效。
	for _, bad := range []string{"verbose", "trace", "1", "warnning"} {
		if _, _, err := New("api", Options{Level: bad}); err == nil {
			t.Errorf("等级 %q 应该被拒绝", bad)
		}
	}
}

// zap 把 warning 当成 warn 的别名接受（zapcore 的 unmarshalText 里明列了这一条），
// 所以 LOG_LEVEL=warning 是能用的。钉住它，免得日后有人以为它是笔误而删掉。
func TestWarningIsAnAliasForWarn(t *testing.T) {
	l, cleanup, err := New("api", Options{Level: "warning"})
	if err != nil {
		t.Fatalf("New 返回错误: %v", err)
	}
	defer func() { _ = cleanup() }()

	if l.Core().Enabled(zapcore.InfoLevel) {
		t.Error("warning 等价于 warn，不该放行 info")
	}
	if !l.Core().Enabled(zapcore.WarnLevel) {
		t.Error("warning 应当放行 warn")
	}
}

func TestDirEmptyWritesOnlyToStdout(t *testing.T) {
	// Dir 为空是「只写 stdout」，本机 go run / go test 的默认行为。
	// 它必须能构造成功，且 cleanup 可调用。
	l, cleanup, err := New("api", Options{Level: "info"})
	if err != nil {
		t.Fatalf("New 返回错误: %v", err)
	}
	if l == nil {
		t.Fatal("logger 不应为 nil")
	}
	if err := cleanup(); err != nil {
		t.Errorf("cleanup 返回错误: %v", err)
	}
}

func TestFileSinkWritesJSONWithFields(t *testing.T) {
	dir := tempDir(t)
	l, cleanup, err := New("worker", Options{Level: "info", Dir: dir})
	if err != nil {
		t.Fatalf("New 返回错误: %v", err)
	}
	defer func() { _ = cleanup() }()

	l.Info("索引完成", zap.Int64("jobId", 7), zap.Int("chunks", 3))

	lines := readLines(t, filepath.Join(dir, "worker.log"))
	if len(lines) != 1 {
		t.Fatalf("期望 1 行日志，实际 %d 行", len(lines))
	}

	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("文件里应当是 JSON，解析失败: %v\n原始行: %s", err, lines[0])
	}

	if entry["msg"] != "索引完成" {
		t.Errorf("msg 字段不对: %v", entry["msg"])
	}
	// service 是构造时绑定的角色，三个容器的日志合并看时靠它分辨来源。
	if entry["service"] != "worker" {
		t.Errorf("service 字段应为 worker，实际 %v", entry["service"])
	}
	// JSON 里数字应当是数字而不是字符串，否则日志系统没法做数值聚合。
	if entry["jobId"] != float64(7) {
		t.Errorf("jobId 应是数字 7，实际 %#v", entry["jobId"])
	}
	if entry["level"] != "info" {
		t.Errorf("文件里的等级应当是小写，实际 %v", entry["level"])
	}
}

// 这个用例存在的理由：第一版用的是 zapcore.MillisDurationEncoder，
// 而它做整数除法，把亚毫秒的请求全部压成 0 —— 实测 6 行访问日志 6 行都是 "cost":0。
// 谁把这里改回整数毫秒，就会红。
func TestCostKeepsSubMillisecondPrecision(t *testing.T) {
	dir := tempDir(t)
	l, cleanup, err := New("api", Options{Level: "info", Dir: dir})
	if err != nil {
		t.Fatalf("New 返回错误: %v", err)
	}
	defer func() { _ = cleanup() }()

	// 健康检查这类请求实测稳定在 0.4~0.7ms，取 434µs 作代表。
	l.Info("请求", Cost(434*time.Microsecond))

	var entry map[string]any
	if err := json.Unmarshal([]byte(readLines(t, filepath.Join(dir, "api.log"))[0]), &entry); err != nil {
		t.Fatalf("解析日志行失败: %v", err)
	}

	got, ok := entry[CostKey].(float64)
	if !ok {
		t.Fatalf("%s 应当是数字（便于聚合），实际 %#v", CostKey, entry[CostKey])
	}
	if got == 0 {
		t.Fatalf("亚毫秒耗时被压成了 0 —— 整数毫秒编码器丢掉了全部信息")
	}
	if math.Abs(got-0.434) > 1e-9 {
		t.Errorf("434µs 应当编码成 0.434 毫秒，实际 %v", got)
	}
	if CostKey != "costMs" {
		t.Errorf("键名必须自带单位，否则没人知道这个数字是什么量级")
	}
}

// 文件名由 mode 决定，这是「api 与 worker 不能写同一个文件」的落地方式。
func TestFilePerMode(t *testing.T) {
	dir := tempDir(t)
	for _, mode := range []string{"api", "worker", "migrate"} {
		_, cleanup, err := New(mode, Options{Level: "info", Dir: dir})
		if err != nil {
			t.Fatalf("mode %s: New 返回错误: %v", mode, err)
		}
		if err := cleanup(); err != nil {
			t.Fatalf("mode %s: cleanup 返回错误: %v", mode, err)
		}
		if _, err := os.Stat(filepath.Join(dir, mode+".log")); err != nil {
			t.Errorf("mode %s 应生成 %s.log: %v", mode, mode, err)
		}
	}
}

func TestCleanupIsIdempotent(t *testing.T) {
	dir := tempDir(t)
	_, cleanup, err := New("api", Options{Level: "info", Dir: dir})
	if err != nil {
		t.Fatalf("New 返回错误: %v", err)
	}
	// main 里既 defer 了 cleanup，出错分支还会显式再调一次，所以必须能重复调用。
	if err := cleanup(); err != nil {
		t.Errorf("第一次 cleanup 返回错误: %v", err)
	}
	if err := cleanup(); err != nil {
		t.Errorf("第二次 cleanup 也应返回 nil，实际: %v", err)
	}
}

// 目录不可写时必须直接失败，而不是安静地变成「只写 stdout」。
// 一个看起来正常、却永远不产生文件的进程，正是这次要消灭的那类问题。
func TestUnwritableDirFailsFast(t *testing.T) {
	// 拿一个普通文件当父目录：MkdirAll 会因为中间路径不是目录而失败，
	// 两个平台上都成立，不依赖权限位（Windows 上权限位很难可靠地构造）。
	file := filepath.Join(tempDir(t), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("准备测试文件失败: %v", err)
	}

	_, _, err := New("api", Options{Level: "info", Dir: filepath.Join(file, "logs")})
	if err == nil {
		t.Fatal("目录不可写时 New 应当返回错误")
	}
	if !strings.Contains(err.Error(), "创建日志目录") {
		t.Errorf("错误信息应当说明是日志目录的问题，实际: %v", err)
	}
}

func TestConsoleFormatJSON(t *testing.T) {
	// LOG_CONSOLE_FORMAT=json 只影响 stdout；这里只验证它不会让构造失败。
	dir := tempDir(t)
	l, cleanup, err := New("api", Options{Level: "info", Console: "json", Dir: dir})
	if err != nil {
		t.Fatalf("New 返回错误: %v", err)
	}
	defer func() { _ = cleanup() }()

	l.Info("请求", zap.Int("status", 200))
	if lines := readLines(t, filepath.Join(dir, "api.log")); len(lines) != 1 {
		t.Fatalf("期望 1 行日志，实际 %d 行", len(lines))
	}
}

// stdioSyncer 的存在意义就是让 stdout 的 Sync 永远成功。
// 如果它的 Sync 开始返回错误，退出时就会多出 "sync /dev/stdout: invalid argument"。
func TestStdioSyncerNeverFails(t *testing.T) {
	var s stdioSyncer
	if err := s.Sync(); err != nil {
		t.Errorf("stdioSyncer.Sync 必须返回 nil，实际: %v", err)
	}
}
