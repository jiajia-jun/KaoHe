package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"KaoHe/internal/store"
)

// GET /api/v1/categories
// 一次返回整棵树。分类数量是人工维护的量级，分页反而让前端拼树更麻烦。
func (s *Server) listCategories(c *gin.Context) {
	tree, err := s.store.ListCategoryTree(c.Request.Context())
	if err != nil {
		failInternal(c, err)
		return
	}
	if tree == nil {
		tree = []*store.CategoryNode{}
	}
	c.JSON(http.StatusOK, gin.H{"items": tree})
}

type createCategoryRequest struct {
	Name string `json:"name"`
	// ParentID 为 nil 表示建为顶层分类
	ParentID *int64 `json:"parentId"`
}

// POST /api/v1/categories
func (s *Server) createCategory(c *gin.Context) {
	var req createCategoryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, codeBadRequest, "请求体不是合法的 JSON")
		return
	}
	node, err := s.store.CreateCategory(c.Request.Context(), req.ParentID, req.Name)
	if err != nil {
		respondCategoryError(c, err)
		return
	}
	c.JSON(http.StatusCreated, node)
}

type updateCategoryRequest struct {
	Name *string `json:"name"`
	// 与文档的分类字段同理：用 RawMessage 才能区分「未提供」与「显式传 null」，
	// 后者表示把这个分类移到顶层。
	ParentID json.RawMessage `json:"parentId"`
}

// PATCH /api/v1/categories/:id
func (s *Server) updateCategory(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	var req updateCategoryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, codeBadRequest, "请求体不是合法的 JSON")
		return
	}

	upd := store.CategoryUpdate{Name: req.Name}
	if req.ParentID != nil {
		upd.MoveToSet = true
		if string(req.ParentID) != "null" {
			var parentID int64
			if err := json.Unmarshal(req.ParentID, &parentID); err != nil || parentID <= 0 {
				fail(c, http.StatusBadRequest, codeBadRequest, "上级分类标识无效")
				return
			}
			upd.MoveTo = &parentID
		}
	}

	if upd.Name == nil && !upd.MoveToSet {
		fail(c, http.StatusBadRequest, codeBadRequest, "请求里没有任何要修改的字段")
		return
	}

	node, err := s.store.UpdateCategory(c.Request.Context(), id, upd)
	if err != nil {
		respondCategoryError(c, err)
		return
	}
	c.JSON(http.StatusOK, node)
}

// DELETE /api/v1/categories/:id
//
// 只删除分类本身，不动任何文件：该分类下的文件会变为「未分类」，字节与索引都不受影响。
// 返回值带出被移出分类的文件数，界面上据此告诉用户刚才发生了什么。
// 有子分类时拒绝删除 —— 那会连带影响整棵子树，是用户没有明确表达的操作。
func (s *Server) deleteCategory(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	moved, err := s.store.DeleteCategory(c.Request.Context(), id)
	if err != nil {
		respondCategoryError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"movedDocuments": moved,
		"message":        fmt.Sprintf("分类已删除，%d 个文件已转为未分类", moved),
	})
}

// respondCategoryError 把领域错误翻译成状态码与用户能看懂的话。
// 重名与「有子分类」用 409：请求本身没错，是当前状态不允许。
func respondCategoryError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrCategoryNotFound):
		fail(c, http.StatusNotFound, codeNotFound, "分类不存在")
	case errors.Is(err, store.ErrCategoryParentNotFound):
		fail(c, http.StatusBadRequest, codeBadRequest, "指定的上级分类不存在")
	case errors.Is(err, store.ErrCategoryNameTaken):
		fail(c, http.StatusConflict, codeConflict, "同一层级下已存在同名分类")
	case errors.Is(err, store.ErrCategoryHasChild):
		fail(c, http.StatusConflict, codeConflict, "该分类下还有子分类，请先删除或移走子分类")
	case errors.Is(err, store.ErrCategoryCycle):
		fail(c, http.StatusBadRequest, codeBadRequest, "不能把分类移动到它自己或它的子分类之下")
	case errors.Is(err, store.ErrCategoryNameEmpty):
		fail(c, http.StatusBadRequest, codeBadRequest, "分类名不能为空")
	case errors.Is(err, store.ErrCategoryNameTooLong):
		fail(c, http.StatusBadRequest, codeBadRequest,
			fmt.Sprintf("分类名不能超过 %d 个字符", store.MaxCategoryNameRunes))
	default:
		failInternal(c, err)
	}
}
