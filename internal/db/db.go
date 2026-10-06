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

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Open 建立连接池。它只负责连上，不负责建表 ——
// 表结构一律由 migrate 用迁移文件建，不用 AutoMigrate（见包注释）。
func Open(dsn string) (*gorm.DB, error) {
	gdb, err := gorm.Open(postgres.New(postgres.Config{DSN: dsn}), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
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
