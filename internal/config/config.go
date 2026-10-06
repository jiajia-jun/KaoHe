// Package config 集中读取运行期配置。
// 所有可变项一律来自环境变量，仓库内只保留 .env.example。
package config

import (
	"fmt"
	"os"
	"strconv"
)

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
}

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

	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
