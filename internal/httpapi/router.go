// Package httpapi 组装 HTTP 路由。
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"KaoHe/internal/config"
	"KaoHe/internal/storage"
	"KaoHe/internal/store"
)

type Server struct {
	cfg     *config.Config
	store   *store.Store
	storage *storage.Store
}

func NewRouter(cfg *config.Config, st *store.Store, files *storage.Store) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)

	s := &Server{cfg: cfg, store: st, storage: files}

	r := gin.New()
	r.Use(gin.Recovery(), accessLog())

	// 供容器 healthcheck 直接访问，不经过 nginx
	r.GET("/healthz", s.health)

	// 对外接口统一挂在 /api/v1 下；nginx 把 /api/ 反代到这里
	api := r.Group("/api/v1")
	{
		api.GET("/healthz", s.health)
		api.GET("/config", s.getConfig)

		api.POST("/documents", s.createDocument)
		api.GET("/documents", s.listDocuments)
		api.GET("/documents/:id", s.getDocument)
		api.GET("/documents/:id/download", s.downloadDocument)
		api.PATCH("/documents/:id", s.updateDocument)
		api.POST("/documents/:id/reindex", s.reindexDocument)

		api.GET("/categories", s.listCategories)
		api.POST("/categories", s.createCategory)
		api.PATCH("/categories/:id", s.updateCategory)
		api.DELETE("/categories/:id", s.deleteCategory)
	}

	return r
}

func accessLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		slog.Info("请求",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"cost", time.Since(start).Round(time.Millisecond).String(),
		)
	}
}

// health 同时用于容器编排的健康判定与前端首页的系统状态展示。
// 数据库不可达时返回 503，让 compose 不要把一个连不上库的 api 标记为健康。
func (s *Server) health(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()

	body := gin.H{
		"status":  "ok",
		"service": "api",
		"time":    time.Now().Format(time.RFC3339),
	}

	if err := s.store.Ping(ctx); err != nil {
		slog.Error("健康检查：数据库不可达", "err", err)
		body["status"] = "degraded"
		body["db"] = "down"
		c.JSON(http.StatusServiceUnavailable, body)
		return
	}

	body["db"] = "ok"
	c.JSON(http.StatusOK, body)
}
