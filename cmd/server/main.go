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
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"KaoHe/internal/config"
	"KaoHe/internal/db"
	"KaoHe/internal/httpapi"
	"KaoHe/internal/indexer"
	"KaoHe/internal/logging"
	"KaoHe/internal/migrate"
	"KaoHe/internal/storage"
	"KaoHe/internal/store"
)

func main() {
	mode := flag.String("mode", "api", "运行模式：api | worker | migrate")
	flag.Parse()

	// 日志配置先于完整配置读入。logger 必须在 config.Load 之前就绪，
	// 否则「配置读不出来」这件事本身就没有地方可以结构化地报出来，
	// 只能退化成裸的 Fprintf。
	logCfg, err := config.LoadLog()
	if err != nil {
		fmt.Fprintf(os.Stderr, "日志配置无效: %v\n", err)
		os.Exit(1)
	}

	logger, cleanup, err := logging.New(*mode, logging.Options{
		Level:      logCfg.Level,
		Console:    logCfg.Console,
		Dir:        logCfg.Dir,
		MaxSizeMB:  logCfg.MaxSizeMB,
		MaxBackups: logCfg.MaxBackups,
		MaxAgeDays: logCfg.MaxAgeDays,
		Compress:   logCfg.Compress,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "初始化日志失败: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = cleanup() }()

	if err := run(*mode, logger); err != nil {
		logger.Error("进程退出", zap.String("mode", *mode), zap.Error(err))
		// os.Exit 会跳过上面的 defer，所以这里必须显式收尾：
		// 最后一行日志（也正是最需要留下来的那一行）可能还堵在缓冲里。
		_ = logger.Sync()
		_ = cleanup()
		os.Exit(1)
	}
}

func run(mode string, logger *zap.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// 只打这份白名单，**绝不** zap.Any("config", cfg)：
	// cfg.DatabaseDSN 里含口令，整个结构体打出去就等于把口令写进日志。
	logger.Info("配置已加载",
		zap.String("database", cfg.RedactedDSN()),
		zap.String("listenAddr", cfg.ListenAddr),
		zap.String("uploadDir", cfg.UploadDir),
		zap.Int64("maxUploadBytes", cfg.MaxUploadBytes),
		zap.Float32("semanticMinScore", cfg.SemanticMinScore),
		zap.Int("slowRequestMs", cfg.SlowRequestMS),
		zap.Int("slowTaskMs", cfg.SlowTaskMS),
	)

	// 收到 SIGTERM/SIGINT 时取消 ctx，让各角色有机会优雅退出
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch mode {
	case "migrate":
		return migrate.Run(ctx, cfg.DatabaseDSN, logger)
	case "api":
		return runAPI(ctx, cfg, logger)
	case "worker":
		return runWorker(ctx, cfg, logger)
	default:
		return fmt.Errorf("未知运行模式 %q，可选：api | worker | migrate", mode)
	}
}

// runWorker 跑索引任务消费者。
//
// 与 api 共用同一个镜像和同一份数据卷：worker 要读原始文件才能抽取正文，
// 而 api 已经把它写在了 uploads 卷上。
func runWorker(ctx context.Context, cfg *config.Config, logger *zap.Logger) error {
	gdb, err := db.Open(cfg.DatabaseDSN, logger)
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

	// 「worker 已启动」由 Worker.Run 在探活成功之后打，那里能带上向量维度与轮询间隔，
	// 比在这里打一条信息量更足的。这里不重复。

	return indexer.NewWorker(indexer.Deps{
		Store:    store.New(gdb),
		Files:    files,
		Embed:    indexer.NewEmbedder(cfg.EmbedURL),
		Log:      logger,
		SlowTask: time.Duration(cfg.SlowTaskMS) * time.Millisecond,
	}).Run(ctx)
}

func runAPI(ctx context.Context, cfg *config.Config, logger *zap.Logger) error {
	gdb, err := db.Open(cfg.DatabaseDSN, logger)
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
		Handler:           httpapi.NewRouter(cfg, store.New(gdb), files, embed, logger),
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("api 已启动", zap.String("addr", cfg.ListenAddr), zap.String("uploadDir", cfg.UploadDir))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		logger.Info("收到退出信号，正在关闭 api")
		// 这里必须从 context.Background() 起新的超时，不能挂在上面那个 ctx 上：
		// 走到这一支时 ctx 已经被取消，用它派生的 ctx 一出生就是 done，
		// Shutdown 会立刻返回而根本没等在途请求处理完 —— 优雅关闭就成了空话。
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second) //nolint:contextcheck // 见上：此处必须脱离已取消的 ctx
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
