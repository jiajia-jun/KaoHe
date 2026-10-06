package httpapi

import (
	"errors"
	"net/http"
	"path/filepath"

	"github.com/gin-gonic/gin"

	"KaoHe/internal/filekind"
	"KaoHe/internal/store"
)

// POST /api/v1/documents/:id/reindex
//
// 需求 4 要求「失败后支持重试」，这个接口就是那个重试入口。
// 它可以被反复调用而在结果上等价：正在排队的任务不会被顶掉，
// 已完成的文档再点一次就是重新索引一遍。
func (s *Server) reindexDocument(c *gin.Context) {
	ctx := c.Request.Context()

	doc, err := s.store.GetDocument(ctx, c.Param("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			fail(c, http.StatusNotFound, codeNotFound, "文件不存在")
			return
		}
		failInternal(c, err)
		return
	}

	// 判断格式用存储键而不是显示名：文件可以被重命名成 .txt，
	// 但磁盘上那份 PDF 并不会因此变得可以抽取正文。
	if !filekind.IsExtractable(filepath.Ext(doc.StorageKey)) {
		fail(c, http.StatusBadRequest, codeBadRequest,
			"该格式不支持正文索引，只能下载与预览")
		return
	}

	if _, err := s.store.EnqueueIndexJob(ctx, doc.ID); err != nil {
		failInternal(c, err)
		return
	}

	// 回传最新的文档而不是让前端自己猜：状态可能是 pending（新建了任务）
	// 也可能是 processing（已有任务正在跑），由数据说了算。
	fresh, err := s.store.GetDocument(ctx, doc.DocUID)
	if err != nil {
		failInternal(c, err)
		return
	}
	c.JSON(http.StatusOK, fresh)
}
