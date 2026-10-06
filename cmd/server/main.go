// Command server 是本项目唯一的可执行文件。
//
// 同一个镜像通过 --mode 扮演不同角色：
//
//	--mode=migrate  一次性任务，建扩展与表结构
//	--mode=api      HTTP 服务
//	--mode=worker   索引任务消费者
//
// 这样 docker-compose 只需要构建一次镜像，各角色的依赖与版本天然一致。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"KaoHe/internal/config"
	"KaoHe/internal/db"
	"KaoHe/internal/httpapi"
	"KaoHe/internal/migrate"
	"KaoHe/internal/storage"
	"KaoHe/internal/store"
)

func main() {
	mode := flag.String("mode", "api", "运行模式：api | worker | migrate")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	if err := run(*mode); err != nil {
		slog.Error("进程退出", "mode", *mode, "err", err)
		os.Exit(1)
	}
}

func run(mode string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// 收到 SIGTERM/SIGINT 时取消 ctx，让各角色有机会优雅退出
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch mode {
	case "migrate":
		return migrate.Run(ctx, cfg.DatabaseDSN)
	case "api":
		return runAPI(ctx, cfg)
	case "worker":
		// M4 实现：从 index_jobs 领取任务、调用 embed 边车、写入 document_chunks
		return errors.New("worker 模式尚未实现（计划在 M4 落地）")
	default:
		return fmt.Errorf("未知运行模式 %q，可选：api | worker | migrate", mode)
	}
}

func runAPI(ctx context.Context, cfg *config.Config) error {
	gdb, err := db.Open(cfg.DatabaseDSN)
	if err != nil {
		return err
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		return fmt.Errorf("获取连接池失败: %w", err)
	}
	defer sqlDB.Close()

	files, err := storage.New(cfg.UploadDir)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           httpapi.NewRouter(cfg, store.New(gdb), files),
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		slog.Info("api 已启动", "addr", cfg.ListenAddr, "uploadDir", cfg.UploadDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		slog.Info("收到退出信号，正在关闭 api")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
