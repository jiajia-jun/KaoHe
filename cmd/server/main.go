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
	"KaoHe/internal/indexer"
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
		return runWorker(ctx, cfg)
	default:
		return fmt.Errorf("未知运行模式 %q，可选：api | worker | migrate", mode)
	}
}

// runWorker 跑索引任务消费者。
//
// 与 api 共用同一个镜像和同一份数据卷：worker 要读原始文件才能抽取正文，
// 而 api 已经把它写在了 uploads 卷上。
func runWorker(ctx context.Context, cfg *config.Config) error {
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

	return indexer.NewWorker(store.New(gdb), files, indexer.NewEmbedder(cfg.EmbedURL)).Run(ctx)
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

	// 边车这里不探活：文件管理、关键词检索都不依赖它，
	// 为了一个可选能力把整个 API 拦在启动之外不划算。
	// 语义检索会在真正调用失败时返回「服务暂不可用」。
	embed := indexer.NewEmbedder(cfg.EmbedURL)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           httpapi.NewRouter(cfg, store.New(gdb), files, embed),
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
		// 这里必须从 context.Background() 起新的超时，不能挂在上面那个 ctx 上：
		// 走到这一支时 ctx 已经被取消，用它派生的 ctx 一出生就是 done，
		// Shutdown 会立刻返回而根本没等在途请求处理完 —— 优雅关闭就成了空话。
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second) //nolint:contextcheck // 见上：此处必须脱离已取消的 ctx
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
