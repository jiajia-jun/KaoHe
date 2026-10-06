// Package httpapi 组装 HTTP 路由。
package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"KaoHe/internal/config"
	"KaoHe/internal/indexer"
	"KaoHe/internal/storage"
	"KaoHe/internal/store"
)

// Server 持有各处理器需要的依赖。
// 处理器都是它的方法，因此取依赖不需要全局变量，测试时也便于替换。
type Server struct {
	cfg     *config.Config
	store   *store.Store
	storage *storage.Store
	// embed 只用于语义检索：把用户的自然语言描述转成查询向量。
	// 边车不可用时它返回错误，API 本身照常提供文件管理与关键词检索。
	embed *indexer.Embedder
	// log 是基础 logger。请求内的日志用 LoggerFrom(c) 取带 requestId 的子 logger，
	// 这个只在没有请求上下文的地方用（如 health 里的启动期判断）。
	log *zap.Logger
}

// NewRouter 装配全部路由。所有接口都挂在 /api/v1 下，
// 只有 /healthz 例外 —— 容器的 healthcheck 直接打 api，不经过 nginx。
func NewRouter(cfg *config.Config, st *store.Store, files *storage.Store,
	embed *indexer.Embedder, log *zap.Logger) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)

	if log == nil {
		log = zap.NewNop()
	}
	s := &Server{cfg: cfg, store: st, storage: files, embed: embed, log: log}

	r := gin.New()
	// 顺序不能改：RequestID 最外、AccessLog 居中、Recovery 贴着 handler。
	//
	// 写成 Use(Recovery(), AccessLog()) 的话 Recovery 在外层，
	// panic 会直接把 AccessLog 的栈帧掀掉 —— 于是出错最严重的那次请求
	// 恰恰没有访问日志。现在这个顺序里 Recovery 先恢复并把响应写完，
	// AccessLog 的收尾代码照常执行，能看到那条 500。
	r.Use(RequestID(log), AccessLog(log, time.Duration(cfg.SlowRequestMS)*time.Millisecond), Recovery(log))

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

		// 删除分两步：DELETE 移入回收站（可撤销），/purge 才是真的抹掉。
		// 彻底删除只对回收站里的文件生效，所以想毁掉一份正在用的文件必须走两步 ——
		// 多出来的这一步就是误删可以停下来的地方。
		api.DELETE("/documents/:id", s.deleteDocument)
		api.POST("/documents/:id/restore", s.restoreDocument)
		api.DELETE("/documents/:id/purge", s.purgeDocument)
		api.DELETE("/trash", s.emptyTrash)

		api.GET("/search", s.searchDocuments)
		api.POST("/search/semantic", s.searchSemantic)

		api.GET("/categories", s.listCategories)
		api.POST("/categories", s.createCategory)
		api.PATCH("/categories/:id", s.updateCategory)
		api.DELETE("/categories/:id", s.deleteCategory)
	}

	return r
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
		// 走请求 logger，这样一条健康检查失败也能顺着 requestId 找到它的访问日志。
		LoggerFrom(c).Error("健康检查：数据库不可达", zap.Error(err))
		body["status"] = "degraded"
		body["db"] = "down"
		c.JSON(http.StatusServiceUnavailable, body)
		return
	}

	body["db"] = "ok"
	c.JSON(http.StatusOK, body)
}
