// Package db 负责建立数据库连接。
//
// 分工约定（见 docs/DESIGN.md 的“关键取舍”）：
//   - 常规 CRUD 走 GORM；
//   - 检索、分类树、任务队列用原生 SQL，因为它们依赖 pgvector / 递归 CTE /
//     FOR UPDATE SKIP LOCKED 这些 GORM 表达不出来的能力。
package db

import (
	"fmt"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Open 建立连接池。它只负责连上，不负责建表 ——
// 表结构一律由 migrate 用迁移文件建，不用 AutoMigrate（见包注释）。
func Open(dsn string, log *zap.Logger) (*gorm.DB, error) {
	if log == nil {
		log = zap.NewNop()
	}

	gormLogger, err := newGormLogger(log)
	if err != nil {
		return nil, err
	}

	gdb, err := gorm.Open(postgres.New(postgres.Config{DSN: dsn}), &gorm.Config{
		Logger: gormLogger,
	})
	if err != nil {
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}

	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("获取连接池失败: %w", err)
	}
	sqlDB.SetMaxOpenConns(20)
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetConnMaxLifetime(time.Hour)

	if err := sqlDB.Ping(); err != nil {
		return nil, fmt.Errorf("数据库不可达: %w", err)
	}
	return gdb, nil
}

// newGormLogger 把 GORM 的 SQL 日志接到 zap 上。
//
// 不接的话它是一条独立的流：gorm 的 logger.Default 直接往 stderr 写，
// 绕过了 slog，换成 zap 之后也照样绕过 —— SQL 错误就永远进不了日志文件。
func newGormLogger(log *zap.Logger) (logger.Interface, error) {
	// NewStdLogAt 返回的是 *log.Logger；这里必须把整个 logger 传下去，
	// 不能传它上面的 .Writer() —— GORM 要的是 interface{ Printf(...) }，
	// 而 Writer() 给的是 io.Writer，类型对不上，编译期就会拦下来。
	stdLog, err := zap.NewStdLogAt(log.Named("gorm"), zapcore.WarnLevel)
	if err != nil {
		return nil, fmt.Errorf("构造数据库日志器失败: %w", err)
	}

	return logger.New(stdLog, logger.Config{
		SlowThreshold: 200 * time.Millisecond,
		LogLevel:      logger.Warn,
		// RecordNotFound 是正常的业务路径（查一个不存在的 id），
		// 记成 Warn 只会把真正的问题淹掉。
		IgnoreRecordNotFoundError: true,
		// GORM 默认带 ANSI 颜色码，进了 docker logs 或 JSON 文件就是一堆乱码。
		Colorful: false,
		// 这一条是重点，不是加固：默认行为会把参数插值进 SQL 文本，
		// 而 insertChunks 的批量 INSERT 里带的是 document_chunks.content ——
		// 也就是**上传文件的正文**。慢查询或出错时，整批正文会被写进日志。
		// 打开它之后 SQL 里只剩占位符，参数不再出现在日志文本中。
		ParameterizedQueries: true,
	}), nil
}
