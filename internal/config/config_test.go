package config

import (
	"strings"
	"testing"
)

// 口令绝不能出现在日志里，这是本次加日志系统时要守住的一条线。
// 用真实形态的串测，而不是随手编一个 —— 编出来的串往往恰好绕开了真正的解析路径。
const secret = "please-change-this-password" //nolint:gosec // G101：这是测试用的假口令，它正是要被断言脱敏掉的那个串

func TestRedactedDSNHidesPassword(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
		// 脱敏之后必须还认得出连的是哪个库、哪个用户，
		// 否则这条日志就只剩下「出错了」，没有排查价值。
		wantKept []string
	}{
		{
			name:     "URL 形式（compose 里用的就是这种）",
			dsn:      "postgres://kaohe:" + secret + "@db:5432/kaohe?sslmode=disable",
			wantKept: []string{"kaohe", "db:5432", "sslmode=disable"},
		},
		{
			name:     "key/value 形式",
			dsn:      "host=db port=5432 user=kaohe password=" + secret + " dbname=kaohe sslmode=disable",
			wantKept: []string{"host=db", "user=kaohe", "dbname=kaohe", "sslmode=disable"},
		},
		{
			name:     "key/value 形式且口令带引号",
			dsn:      "host=db user=kaohe password='" + secret + "' dbname=kaohe",
			wantKept: []string{"host=db", "user=kaohe", "dbname=kaohe"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := (&Config{DatabaseDSN: tc.dsn}).RedactedDSN()

			if strings.Contains(got, secret) {
				t.Fatalf("脱敏后仍然含口令: %s", got)
			}
			if !strings.Contains(got, Redacted) {
				t.Errorf("脱敏后应当能看到 %s 占位: %s", Redacted, got)
			}
			// 占位串本身不该被百分号转义，否则日志里读起来像乱码。
			if strings.Contains(got, "%2A") || strings.Contains(got, "%25") {
				t.Errorf("占位串被转义了: %s", got)
			}
			for _, keep := range tc.wantKept {
				if !strings.Contains(got, keep) {
					t.Errorf("脱敏不该把 %q 也抹掉，实际: %s", keep, got)
				}
			}
		})
	}
}

func TestRedactedDSNWithoutPasswordIsUnchanged(t *testing.T) {
	// 本来就没有口令的串不该被改动，否则日志里的连接信息会变得不可信。
	dsn := "postgres://kaohe@db:5432/kaohe?sslmode=disable"
	if got := (&Config{DatabaseDSN: dsn}).RedactedDSN(); got != dsn {
		t.Errorf("无口令的 DSN 不该被改动\n原始: %s\n实际: %s", dsn, got)
	}
	if got := (&Config{}).RedactedDSN(); got != "" {
		t.Errorf("空 DSN 应返回空串，实际: %q", got)
	}
}

func TestLoadLogDefaults(t *testing.T) {
	// 清空所有相关变量，验证「什么都不配」也能起来。
	for _, k := range []string{
		"LOG_LEVEL", "LOG_CONSOLE_FORMAT", "LOG_DIR",
		"LOG_MAX_SIZE_MB", "LOG_MAX_BACKUPS", "LOG_MAX_AGE_DAYS", "LOG_COMPRESS",
	} {
		t.Setenv(k, "")
	}

	l, err := LoadLog()
	if err != nil {
		t.Fatalf("LoadLog 返回错误: %v", err)
	}
	if l.Level != "info" {
		t.Errorf("默认等级应是 info，实际 %q", l.Level)
	}
	if l.Console != "console" {
		t.Errorf("默认 console 格式应是 console，实际 %q", l.Console)
	}
	// 默认为空 = 只写 stdout。落盘由 compose 显式设置打开，
	// 这样本机 go run / go test 不会在盘符根目录下凭空建出 /data/logs。
	if l.Dir != "" {
		t.Errorf("默认 Dir 应为空（只写 stdout），实际 %q", l.Dir)
	}
	if l.MaxSizeMB <= 0 || l.MaxBackups <= 0 || l.MaxAgeDays <= 0 {
		t.Errorf("轮转参数的默认值都该是正数，实际 %+v", l)
	}
	if l.Compress {
		t.Error("压缩默认应当是关闭的：轮转出来的 .gz 用 grep 看不了")
	}
}

func TestLoadLogRejectsBadValues(t *testing.T) {
	cases := []struct{ key, value string }{
		{"LOG_MAX_SIZE_MB", "big"},
		// 0 表示永不轮转 = 文件无限长，不是我们想支持的用法
		{"LOG_MAX_SIZE_MB", "0"},
		// 0 表示不限制合法，但负数不合法
		{"LOG_MAX_BACKUPS", "-1"},
		{"LOG_MAX_AGE_DAYS", "-1"},
		{"LOG_MAX_AGE_DAYS", "0.5"},
		{"LOG_COMPRESS", "yes-please"},
		{"LOG_CONSOLE_FORMAT", "xml"},
	}

	for _, tc := range cases {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if _, err := LoadLog(); err == nil {
				t.Errorf("%s=%q 应当被拒绝", tc.key, tc.value)
			} else if !strings.Contains(err.Error(), tc.key) {
				// 错误信息里必须带上变量名，否则运维不知道该改哪一个。
				t.Errorf("错误信息应当提到 %s，实际: %v", tc.key, err)
			}
		})
	}
}

func TestLoadIncludesLogConfigAndSlowThresholds(t *testing.T) {
	t.Setenv("DATABASE_DSN", "postgres://kaohe:pw@db:5432/kaohe?sslmode=disable")
	t.Setenv("LOG_DIR", "/data/logs")
	t.Setenv("SLOW_REQUEST_MS", "250")
	t.Setenv("SLOW_TASK_MS", "7500")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load 返回错误: %v", err)
	}
	// Load 必须把 LoadLog 的结果带出来，否则 main 里那处赋值漏掉也不会有人发现。
	if cfg.Log.Dir != "/data/logs" {
		t.Errorf("Log.Dir 应当被 Load 带出来，实际 %q", cfg.Log.Dir)
	}
	if cfg.SlowRequestMS != 250 {
		t.Errorf("SlowRequestMS 应为 250，实际 %d", cfg.SlowRequestMS)
	}
	if cfg.SlowTaskMS != 7500 {
		t.Errorf("SlowTaskMS 应为 7500，实际 %d", cfg.SlowTaskMS)
	}
}

func TestSlowThresholdDefaultsAreCalibrated(t *testing.T) {
	t.Setenv("DATABASE_DSN", "postgres://kaohe:pw@db:5432/kaohe?sslmode=disable")
	t.Setenv("SLOW_REQUEST_MS", "")
	t.Setenv("SLOW_TASK_MS", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load 返回错误: %v", err)
	}
	if cfg.SlowRequestMS != DefaultSlowRequestMS || cfg.SlowTaskMS != DefaultSlowTaskMS {
		t.Errorf("慢阈值应取默认值，实际 %d / %d", cfg.SlowRequestMS, cfg.SlowTaskMS)
	}
}

func TestLoadRejectsInvalidSlowThreshold(t *testing.T) {
	t.Setenv("DATABASE_DSN", "postgres://kaohe:pw@db:5432/kaohe?sslmode=disable")
	t.Setenv("SLOW_REQUEST_MS", "0")
	if _, err := Load(); err == nil {
		t.Error("SLOW_REQUEST_MS=0 应当被拒绝：那会让每个请求都被标成慢")
	}
}
