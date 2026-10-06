// Package config 集中读取运行期配置。
// 所有可变项一律来自环境变量，仓库内只保留 .env.example。
package config

import (
	"fmt"
	"os"
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
}

// DefaultSemanticMinScore 是余弦相似度下限的默认值。
//
// 实测依据（bge-small-zh-v1.5 + 验收语料，见 docs/DESIGN.md 的标定表）：
// 与提问确实相关的片段，文档级相似度都在 0.60 以上；而语料里根本没有对应内容时，
// 最像的几条落在 0.44~0.47。分界落在两者之间的空档里，0.5 是它的中点。
// 取 0.45 等于几乎不过滤（任何输入都会返回满屏弱相关结果），
// 取 0.6 又会把「相关但措辞差得远」的结果一起挡掉。
const DefaultSemanticMinScore = 0.5

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

	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
