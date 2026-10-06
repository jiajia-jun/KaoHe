// Package config 集中读取运行期配置。
// 所有可变项一律来自环境变量，仓库内只保留 .env.example。
package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
)

// Config 是进程启动时一次性读入的配置，之后不再变化。
type Config struct {
	// DatabaseDSN 形如 postgres://user:pass@host:5432/db?sslmode=disable
	DatabaseDSN string
	// ListenAddr 是 api 的监听地址，容器内固定 :8080，不对宿主机暴露
	ListenAddr string
	// UploadDir 是原始文件的持久化目录，映射到具名卷 uploads
	UploadDir string
	// MaxUploadBytes 是单文件上传上限，超限返回 413
	MaxUploadBytes int64
	// EmbedURL 是 embedding 边车的地址
	EmbedURL string
	// SemanticMinScore 是语义检索的余弦相似度下限。
	// 做成配置项是因为它取决于所用模型：换一个向量模型，合适的阈值就变了，
	// 硬编码在代码里会让「换了模型之后结果全空」变成一个要读源码才能解释的问题。
	SemanticMinScore float32
	// Log 是日志子系统的配置。它与 SlowRequestMS / SlowTaskMS 分开读，
	// 因为 logger 必须在 Load 之前就绪 —— 见 LoadLog 的说明。
	Log Log
	// SlowRequestMS 超过它的 HTTP 请求会被标记为慢（access log 里 slow=true）。
	SlowRequestMS int
	// SlowTaskMS 超过它的索引任务会被标记为慢。
	SlowTaskMS int
}

// Log 是日志子系统的配置。
//
// 它与 internal/logging.Options 字段一一对应。两边刻意不共享类型：
// logging 不依赖 config（logger 要在 config 之前就绪），config 也不依赖 zap
// （等级字符串的合法性由 logging.New 用 zapcore 校验，不在这一层判）。
// 代价是 cmd/server 里有一处逐字段的转换。
type Log struct {
	Level      string
	Console    string
	Dir        string
	MaxSizeMB  int
	MaxBackups int
	MaxAgeDays int
	Compress   bool
}

// DefaultSemanticMinScore 是余弦相似度下限的默认值。
//
// 实测依据（bge-small-zh-v1.5 + 验收语料，见 docs/DESIGN.md 的标定表）：
// 与提问确实相关的片段，文档级相似度都在 0.60 以上；而语料里根本没有对应内容时，
// 最像的几条落在 0.44~0.47。分界落在两者之间的空档里，0.5 是它的中点。
// 取 0.45 等于几乎不过滤（任何输入都会返回满屏弱相关结果），
// 取 0.6 又会把「相关但措辞差得远」的结果一起挡掉。
const DefaultSemanticMinScore = 0.5

// 慢请求与慢任务的默认阈值，实测依据（本机单用户、验收语料 10 份）：
//
//	HTTP 请求（healthz / 列表 / 关键词检索 / 语义检索各 3 次）绝大多数在 45ms 以内，
//	冷启动那一次是 189ms —— 所以 500ms 大约是热态最大值的十倍，
//	正常波动不会误报，真出问题一定够醒目。
//	索引任务（worker 日志里的 cost）语料文件在 16ms~585ms 之间，
//	唯一一次 15.41s 出自一个 440KB 的 Markdown —— 所以 5s 能把这一个异常挑出来。
//
// 两个值都可能因为大文件而正常触发（20MiB 的文本要切很多片、调很多次边车），
// 那不算误报：它只是被标成 slow，不是失败。
const (
	DefaultSlowRequestMS = 500
	DefaultSlowTaskMS    = 5000
)

// Load 从环境变量组装配置，缺少必填项时返回错误让进程直接起不来 ——
// 一个连不上数据库的容器，早退比带病运行更好排查。
func Load() (*Config, error) {
	cfg := &Config{
		DatabaseDSN: os.Getenv("DATABASE_DSN"),
		ListenAddr:  envOr("LISTEN_ADDR", ":8080"),
		UploadDir:   envOr("UPLOAD_DIR", "/data/uploads"),
		EmbedURL:    envOr("EMBED_URL", "http://embed:8000"),
	}

	if cfg.DatabaseDSN == "" {
		return nil, fmt.Errorf("DATABASE_DSN 未设置")
	}

	maxBytes, err := strconv.ParseInt(envOr("MAX_UPLOAD_BYTES", "20971520"), 10, 64)
	if err != nil || maxBytes <= 0 {
		return nil, fmt.Errorf("MAX_UPLOAD_BYTES 必须是正整数，当前为 %q", os.Getenv("MAX_UPLOAD_BYTES"))
	}
	cfg.MaxUploadBytes = maxBytes

	minScore, err := strconv.ParseFloat(
		envOr("SEMANTIC_MIN_SCORE", strconv.FormatFloat(DefaultSemanticMinScore, 'f', -1, 32)), 32)
	if err != nil || minScore <= 0 || minScore > 1 {
		return nil, fmt.Errorf("SEMANTIC_MIN_SCORE 必须是 0 到 1 之间的小数，当前为 %q",
			os.Getenv("SEMANTIC_MIN_SCORE"))
	}
	cfg.SemanticMinScore = float32(minScore)

	logCfg, err := LoadLog()
	if err != nil {
		return nil, err
	}
	cfg.Log = logCfg

	slowReq, err := envIntOr("SLOW_REQUEST_MS", DefaultSlowRequestMS)
	if err != nil || slowReq <= 0 {
		return nil, fmt.Errorf("SLOW_REQUEST_MS 必须是正整数，当前为 %q", os.Getenv("SLOW_REQUEST_MS"))
	}
	cfg.SlowRequestMS = slowReq

	slowTask, err := envIntOr("SLOW_TASK_MS", DefaultSlowTaskMS)
	if err != nil || slowTask <= 0 {
		return nil, fmt.Errorf("SLOW_TASK_MS 必须是正整数，当前为 %q", os.Getenv("SLOW_TASK_MS"))
	}
	cfg.SlowTaskMS = slowTask

	return cfg, nil
}

// LoadLog 只读 LOG_* 开头的环境变量，不碰 DATABASE_DSN 那些必填项。
//
// 单独拆出来是因为 logger 必须在 Load 之前就绪：否则「配置读取失败」这件事本身
// 就没有地方可以结构化地报出来，只能退化成裸的 Fprintf。Load 内部复用它。
func LoadLog() (Log, error) {
	l := Log{
		Level:   envOr("LOG_LEVEL", "info"),
		Console: envOr("LOG_CONSOLE_FORMAT", "console"),
		// 默认空 = 只写 stdout。落盘由 compose 显式设成 /data/logs 打开，
		// 这样本机 go run / go test 不会凭空在盘符根目录下建出 /data/logs。
		Dir: os.Getenv("LOG_DIR"),
	}

	if l.Console != "console" && l.Console != "json" {
		return Log{}, fmt.Errorf("LOG_CONSOLE_FORMAT 只能是 console 或 json，当前为 %q",
			os.Getenv("LOG_CONSOLE_FORMAT"))
	}

	// 轮转参数的取值范围跟着 lumberjack 的实际语义定：
	// MaxSize 为 0 表示永不轮转（文件无限长），这不是我们想支持的用法，所以要求 > 0；
	// MaxBackups / MaxAge 为 0 表示不限制，是合法配置，所以只拒绝负数。
	var err error
	if l.MaxSizeMB, err = envIntOr("LOG_MAX_SIZE_MB", 32); err != nil || l.MaxSizeMB <= 0 {
		return Log{}, fmt.Errorf("LOG_MAX_SIZE_MB 必须是正整数，当前为 %q", os.Getenv("LOG_MAX_SIZE_MB"))
	}
	if l.MaxBackups, err = envIntOr("LOG_MAX_BACKUPS", 5); err != nil || l.MaxBackups < 0 {
		return Log{}, fmt.Errorf("LOG_MAX_BACKUPS 必须是非负整数（0 表示不限制），当前为 %q", os.Getenv("LOG_MAX_BACKUPS"))
	}
	if l.MaxAgeDays, err = envIntOr("LOG_MAX_AGE_DAYS", 30); err != nil || l.MaxAgeDays < 0 {
		return Log{}, fmt.Errorf("LOG_MAX_AGE_DAYS 必须是非负整数（0 表示不限制），当前为 %q", os.Getenv("LOG_MAX_AGE_DAYS"))
	}
	// 压缩默认关闭：开了之后轮转出来的文件是 .gz，得用 zcat 而不是 grep 看，
	// 这对一个顺手去翻日志的人来说是个没必要的意外。
	if l.Compress, err = envBoolOr("LOG_COMPRESS", false); err != nil {
		return Log{}, fmt.Errorf("LOG_COMPRESS 只能是 true 或 false，当前为 %q", os.Getenv("LOG_COMPRESS"))
	}

	return l, nil
}

// RedactedDSN 返回把口令换掉的连接串，供日志使用。
//
// 这里唯一的敏感值就是数据库口令（postgres://user:pass@...）。
// 注意 pgx 自己的 ParseConfigError 已经会做脱敏，但那只是它报错时的尽力而为 ——
// 我们的日志里任何时候都不该出现明文口令，所以自己再兜一层。
func (c *Config) RedactedDSN() string {
	return redactDSN(c.DatabaseDSN)
}

// Redacted 是替换口令用的占位串。
//
// 不用 *** —— url.UserPassword 会把 * 百分号转义成 %2A%2A%2A，
// 在日志里既难认又像是乱码。字母不会被转义，读起来也一眼就懂。
const Redacted = "REDACTED"

// dsnPasswordKV 匹配 key/value 形式的 DSN 里的 password=...，
// 值可能带单引号（libpq 允许 password='a b'）。
var dsnPasswordKV = regexp.MustCompile(`(?i)(password\s*=\s*)('(?:[^'\\]|\\.)*'|\S+)`)

func redactDSN(dsn string) string {
	if dsn == "" {
		return ""
	}

	// URL 形式：postgres://user:pass@host:5432/db
	if u, err := url.Parse(dsn); err == nil && u.Scheme != "" {
		if _, hasPassword := u.User.Password(); hasPassword {
			u.User = url.UserPassword(u.User.Username(), Redacted)
		}
		return u.String()
	}

	// key/value 形式：host=... user=... password=...
	return dsnPasswordKV.ReplaceAllString(dsn, "${1}"+Redacted)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIntOr(key string, fallback int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	return strconv.Atoi(raw)
}

func envBoolOr(key string, fallback bool) (bool, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	return strconv.ParseBool(raw)
}
