// Package logging 构造进程唯一的 zap logger，并定义全项目共用的日志字段词表。
//
// # 字段词表
//
// 跨文件关联日志靠的是下面这几个键，改名前先想清楚有没有别处依赖它：
//
//	requestId   一次 HTTP 请求，只存在于 api 侧
//	documentId  文档的数字主键（zap.Int64）
//	docUID      文档对外暴露的 doc_xxxx 标识（zap.String）
//	jobId       一次索引任务（zap.Int64）
//	service     进程角色，构造时就绑好（api / worker / migrate）
//
// worker 手里只有 documentId，api 侧两个都有，**不要为了统一而互相强转**。
//
// # 两个刻意不做的选择
//
//   - 不用 zap.NewProduction / NewDevelopment：它们硬编码自己的 encoder、sink 与采样，
//     而 Development 还会把 DPanic 真的变成 panic，对常驻服务是错的行为。
//   - 不加 zap.Sampling：它会丢弃重复的相同条目，但在一个三容器系统里，
//     第一万条一模一样的数据库错误**就是**信号本身。
//
// # 一个约束
//
// 日志文件名由 mode 决定（api.log / worker.log / migrate.log），
// 这个命名在「一个 api + 一个 worker + 一次 migrate」下是安全的。
// 它**不**支持 docker compose --scale api=3：三个进程会往同一个文件里追加，
// 而 lumberjack 的轮转是「重命名 + 新建」，不做多进程同步，
// 结果是交替丢日志、轮转互相踩。跨容器的文件锁本身也不可靠，所以不加锁，
// 而是把单副本约束写在这里和 docs/DESIGN.md 里。
package logging

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

// Options 是日志子系统需要的全部配置。
//
// 它刻意不从 internal/config 取：logger 必须在 config.Load 之前就绪，
// 否则「配置读取失败」这件事本身就没有地方可以结构化地报出来。
// 代价是这个结构体与 config.Log 的字段要保持一致，由调用点一处完成转换。
type Options struct {
	Level      string // debug | info | warn | error；空串等价于 info
	Console    string // console | json；只作用于 stdout，文件恒为 JSON
	Dir        string // 日志目录；为空则只写 stdout（本机 go run / go test 的默认）
	MaxSizeMB  int
	MaxBackups int
	MaxAgeDays int
	Compress   bool
}

// New 按 mode（api | worker | migrate）命名文件，构造一个同时写 stdout 与文件的 logger。
//
// 返回的 cleanup 用于进程退出前收尾（Sync 再 Close），重复调用是安全的。
//
// Dir 不可写时直接返回 error 而**不是**降级成「只写 stdout」：一个不可写的目录
// 会安静地变成「文件日志根本不存在」，进程照跑、看起来一切正常，
// 直到有人去找日志才发现 —— 那正是本次要消灭的那类问题。
func New(mode string, opt Options) (*zap.Logger, func() error, error) {
	var lvl zapcore.Level
	// 空串在 zapcore 里被解析成 info，所以「没配」和「配成 info」走的是同一条路径。
	if err := lvl.UnmarshalText([]byte(opt.Level)); err != nil {
		return nil, nil, fmt.Errorf("LOG_LEVEL 取值无效: %w", err)
	}

	// stdout 给人看，文件给机器读，所以两份 encoder 配置分开设。
	consoleCfg := zap.NewProductionEncoderConfig()
	consoleCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	// 不带颜色：ANSI 转义码进了 docker logs 只会变成乱码。
	consoleCfg.EncodeLevel = zapcore.CapitalLevelEncoder
	consoleCfg.EncodeDuration = floatMillisEncoder

	fileCfg := zap.NewProductionEncoderConfig()
	fileCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	fileCfg.EncodeLevel = zapcore.LowercaseLevelEncoder
	fileCfg.EncodeDuration = floatMillisEncoder

	consoleEnc := zapcore.Encoder(zapcore.NewConsoleEncoder(consoleCfg))
	if opt.Console == "json" {
		consoleEnc = zapcore.NewJSONEncoder(fileCfg)
	}

	// 每个 sink 一个 core：encoder 是 core 的属性，不是 sink 的属性。
	// 用 NewMultiWriteSyncer 做不到「stdout 文本、文件 JSON」——
	// 它在编码*之后*才分叉字节，会把 console 文本也写进文件。
	//
	// Lock 不是可选的：stdout 有多个 goroutine 在写（gin 的 worker、serve goroutine、
	// 信号处理路径），而 zapcore.NewCore 自身不做同步。
	cores := []zapcore.Core{
		zapcore.NewCore(consoleEnc, zapcore.Lock(stdioSyncer{os.Stdout}), lvl),
	}

	var closer io.Closer
	if opt.Dir != "" {
		// lumberjack 是「第一次写」才 MkdirAll 的，而 zapcore 会把 core 的 Write
		// 错误整个吞掉 —— 所以必须在这里提前探一次，让配置错误在启动时就暴露。
		if err := os.MkdirAll(opt.Dir, 0o750); err != nil {
			return nil, nil, fmt.Errorf("创建日志目录 %s 失败: %w", opt.Dir, err)
		}

		rotator := &lumberjack.Logger{
			Filename:   filepath.Join(opt.Dir, mode+".log"),
			MaxSize:    opt.MaxSizeMB,
			MaxBackups: opt.MaxBackups,
			MaxAge:     opt.MaxAgeDays,
			Compress:   opt.Compress,
		}

		probe, err := os.OpenFile(rotator.Filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, nil, fmt.Errorf("日志文件 %s 不可写: %w", rotator.Filename, err)
		}
		_ = probe.Close()

		cores = append(cores,
			zapcore.NewCore(zapcore.NewJSONEncoder(fileCfg), zapcore.Lock(zapcore.AddSync(rotator)), lvl))
		closer = rotator
	}

	logger := zap.New(
		zapcore.NewTee(cores...),
		zap.AddCaller(),
		// 只给 Error 以上带栈。栈指向 failInternal 而不是根因，不完美，
		// 但仍是最便宜的面包屑；真正重要的栈由 recovery 中间件单独带。
		zap.AddStacktrace(zapcore.ErrorLevel),
		// 三个容器的文件合并着看时能分清来源。
		zap.Fields(zap.String("service", mode)),
		// zap 自身的编码错误走这里，别让它落回 stderr 的默认格式。
		zap.ErrorOutput(zapcore.Lock(stdioSyncer{os.Stderr})),
	)

	cleanup := func() error {
		// stdioSyncer 之后这里不会再有 EINVAL，同步错误可以如实上报。
		_ = logger.Sync()
		if closer != nil {
			return closer.Close()
		}
		return nil
	}
	return logger, cleanup, nil
}

// stdioSyncer 让 stdout/stderr 的 Sync 恒成功。
//
// 对管道做 fsync 会返回 EINVAL，而 Docker 里 stdout 正是管道；zap 会把每个
// WriteSyncer 的 Sync 错误原样上报，于是每次退出都会多一行
// "sync /dev/stdout: invalid argument"。这条「错误」没有任何可操作性，
// 只会把真正的问题挤得看不见。
//
// 不用 errors.Is(err, syscall.EINVAL) 去打补丁：那是 Linux 专有，
// 漏掉别的平台，而且是抄来不理解它的典型写法。
type stdioSyncer struct{ io.Writer }

// Sync 刻意返回 nil —— 见 stdioSyncer 的说明。
func (stdioSyncer) Sync() error { return nil }

// floatMillisEncoder 把时长编码成「毫秒」的浮点数。
//
// 不用 zapcore.MillisDurationEncoder（它做整数除法）：这个系统的请求经常在
// 1 毫秒以下 —— 实测健康检查稳定在 0.4~0.7ms —— 整数毫秒会把它们全部压成 0，
// 落盘日志里就再也看不出快慢了。这不是假设：第一版就是那么写的，
// 切出 6 行访问日志，6 行全是 "cost":0。
//
// 也不用 StringDurationEncoder：它给出 "434.182µs" 这种好读但没法聚合的字符串，
// 而且 µ 在 Windows 的 GBK 终端里会变成乱码。
//
// 浮点毫秒两头都占：可读、可聚合、纯 ASCII。代价是单位不在值里，
// 所以用它的字段一律带 Ms 后缀，让键名自己说清楚。
func floatMillisEncoder(d time.Duration, enc zapcore.PrimitiveArrayEncoder) {
	enc.AppendFloat64(millis(d))
}

// CostKey 是耗时字段的键名。
//
// 带 Ms 后缀是必须的：值是个纯数字，单位不在里面，键名不说就没人知道
// 15 是 15 毫秒还是 15 秒。
const CostKey = "costMs"

// Cost 返回一个以毫秒为单位的耗时字段。
//
// 放在这里而不是让各处自己 zap.Float64(...)：换算方式与上面那个 encoder 是
// 同一件事的两半 —— 用 zap.Duration 配 floatMillisEncoder 也能得到同样的输出，
// 但那样每处调用点都要自己知道该用什么类型。统一从这里出去就不会走岔。
func Cost(d time.Duration) zap.Field {
	return zap.Float64(CostKey, millis(d))
}

func millis(d time.Duration) float64 {
	return float64(d.Nanoseconds()) / 1e6
}
