// Package migrate 执行数据库结构迁移。
//
// 迁移文件通过 go:embed 编进二进制，因此 migrate 容器不需要挂载任何目录，
// compose 里也不必为它准备卷。已应用的版本记录在 schema_migrations 表中，
// 重复执行只跳过、不报错。
package migrate

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"sort"

	_ "github.com/jackc/pgx/v5/stdlib" // 注册 "pgx" driver
)

//go:embed sql/*.sql
var migrationFiles embed.FS

// 固定值，仅用于让并发的 migrate 串行执行。
const advisoryLockID int64 = 0x4B616F48 // "KaoH"

const createMigrationsTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    TEXT        PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`

func Run(ctx context.Context, dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("打开数据库连接失败: %w", err)
	}
	defer db.Close()

	// 迁移期间独占一条连接：advisory lock 是会话级的，必须落在同一条连接上。
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("获取数据库连接失败: %w", err)
	}
	defer conn.Close()

	if err := conn.PingContext(ctx); err != nil {
		return fmt.Errorf("数据库不可达: %w", err)
	}

	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockID); err != nil {
		return fmt.Errorf("获取迁移锁失败: %w", err)
	}
	defer func() {
		// 即使 ctx 已被取消也要释放锁，避免阻塞下一次启动。
		_, _ = conn.ExecContext(context.WithoutCancel(ctx),
			`SELECT pg_advisory_unlock($1)`, advisoryLockID)
	}()

	if _, err := conn.ExecContext(ctx, createMigrationsTable); err != nil {
		return fmt.Errorf("创建迁移记录表失败: %w", err)
	}

	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return err
	}

	versions, err := availableVersions()
	if err != nil {
		return err
	}

	pending := 0
	for _, version := range versions {
		if applied[version] {
			continue
		}
		if err := applyOne(ctx, conn, version); err != nil {
			return err
		}
		slog.Info("迁移已应用", "version", version)
		pending++
	}

	if pending == 0 {
		slog.Info("数据库结构已是最新，无需迁移", "versions", len(versions))
	} else {
		slog.Info("迁移完成", "applied", pending)
	}
	return nil
}

func availableVersions() ([]string, error) {
	entries, err := fs.Glob(migrationFiles, "sql/*.sql")
	if err != nil {
		return nil, fmt.Errorf("枚举迁移文件失败: %w", err)
	}
	versions := make([]string, 0, len(entries))
	for _, entry := range entries {
		versions = append(versions, path.Base(entry))
	}
	// 文件名前缀即版本号，字典序等于执行序
	sort.Strings(versions)
	return versions, nil
}

func appliedVersions(ctx context.Context, conn *sql.Conn) (map[string]bool, error) {
	rows, err := conn.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("读取迁移记录失败: %w", err)
	}
	defer rows.Close()

	applied := map[string]bool{}
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("解析迁移记录失败: %w", err)
		}
		applied[version] = true
	}
	return applied, rows.Err()
}

// applyOne 在单个事务里执行一个迁移文件并登记版本，保证“要么全做、要么全不做”。
func applyOne(ctx context.Context, conn *sql.Conn, version string) error {
	body, err := migrationFiles.ReadFile(path.Join("sql", version))
	if err != nil {
		return fmt.Errorf("读取迁移文件 %s 失败: %w", version, err)
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // 提交成功后回滚是空操作

	if _, err := tx.ExecContext(ctx, string(body)); err != nil {
		return fmt.Errorf("执行迁移 %s 失败: %w", version, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
		return fmt.Errorf("登记迁移版本 %s 失败: %w", version, err)
	}
	return tx.Commit()
}
