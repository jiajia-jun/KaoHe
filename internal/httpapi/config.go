package httpapi

import (
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"
)

// GET /api/v1/config
//
// 上传约束（允许的扩展名、体积上限）真正的真相在后端，由环境变量决定。
// 前端要给出「只能传 PDF/TXT/Markdown」「最大 20 MiB」这类提示，
// 如果自己再写一份常量，改了环境变量就会出现界面说一套、服务端做一套。
// 所以把这份约束暴露成只读接口，界面提示与服务端校验同源。
func (s *Server) getConfig(c *gin.Context) {
	exts := make([]string, 0, len(supportedExtensions))
	for ext := range supportedExtensions {
		exts = append(exts, ext)
	}
	sort.Strings(exts)

	c.JSON(http.StatusOK, gin.H{
		"maxUploadBytes":    s.cfg.MaxUploadBytes,
		"allowedExtensions": exts,
	})
}
