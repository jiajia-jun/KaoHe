package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/lib/pq"

	"KaoHe/internal/store"
)

const (
	maxNameRunes = 200
	maxTagCount  = 20
	maxTagRunes  = 32
)

// supportedExtensions 是允许上传的格式。target 要求至少覆盖 PDF、TXT、Markdown。
// 判定以扩展名为准，不做 MIME 嗅探 —— 这一取舍记在 docs/DESIGN.md 的已知限制里。
var supportedExtensions = map[string]string{
	".pdf":      "application/pdf",
	".txt":      "text/plain; charset=utf-8",
	".md":       "text/markdown; charset=utf-8",
	".markdown": "text/markdown; charset=utf-8",
}

// extractableExtensions 是会被抽取正文并建立索引的格式。
// PDF 本设计只做保存、下载与在线预览，因此标记为 not_supported 而非 failed。
var extractableExtensions = map[string]bool{
	".txt":      true,
	".md":       true,
	".markdown": true,
}

// POST /api/v1/documents
func (s *Server) createDocument(c *gin.Context) {
	// 请求体上限比文件上限多留 1 MiB 给 multipart 边界与其余字段，
	// 精确的超限判断随后按 fileHeader.Size 做。
	bodyLimit := s.cfg.MaxUploadBytes + (1 << 20)
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, bodyLimit)

	fileHeader, err := c.FormFile("file")
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) || strings.Contains(err.Error(), "request body too large") {
			fail(c, http.StatusRequestEntityTooLarge, codeTooLarge,
				fmt.Sprintf("文件超过上传上限 %s", humanSize(s.cfg.MaxUploadBytes)))
			return
		}
		fail(c, http.StatusBadRequest, codeBadRequest, "请求中缺少 file 字段，或表单格式不正确")
		return
	}

	if fileHeader.Size <= 0 {
		fail(c, http.StatusBadRequest, codeBadRequest, "文件内容为空")
		return
	}
	if fileHeader.Size > s.cfg.MaxUploadBytes {
		fail(c, http.StatusRequestEntityTooLarge, codeTooLarge,
			fmt.Sprintf("文件大小 %s，超过上传上限 %s",
				humanSize(fileHeader.Size), humanSize(s.cfg.MaxUploadBytes)))
		return
	}

	ext := strings.ToLower(filepath.Ext(fileHeader.Filename))
	contentType, supported := supportedExtensions[ext]
	if !supported {
		fail(c, http.StatusBadRequest, codeUnsupported,
			"暂不支持该文件格式，目前支持 PDF、TXT、Markdown（.pdf / .txt / .md）")
		return
	}

	// 分类校验：给出可读的 400，而不是让外键约束抛出 500
	categoryID, ok := s.resolveCategoryInput(c, c.PostForm("categoryId"))
	if !ok {
		return
	}

	tags, err := normalizeTags(c.PostForm("tags"))
	if err != nil {
		fail(c, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}

	docUID := newDocUID()

	src, err := fileHeader.Open()
	if err != nil {
		failInternal(c, fmt.Errorf("打开上传内容失败: %w", err))
		return
	}
	defer src.Close()

	// 先落盘再写库。反过来的话，数据库里可能出现指向不存在文件的记录。
	storageKey, size, err := s.storage.Save(docUID, ext, src)
	if err != nil {
		failInternal(c, err)
		return
	}

	doc := &store.Document{
		DocUID:        docUID,
		Name:          sanitizeFileName(fileHeader.Filename),
		StorageKey:    storageKey,
		ContentType:   contentType,
		SizeBytes:     size,
		CategoryID:    categoryID,
		Tags:          tags,
		StorageStatus: "stored",
		IndexStatus:   store.IndexPending,
	}

	enqueueIndex := extractableExtensions[ext]
	if !enqueueIndex {
		doc.IndexStatus = store.IndexNotSupported
	}

	if err := s.store.CreateDocument(c.Request.Context(), doc, enqueueIndex); err != nil {
		// 写库失败就回滚磁盘，否则会留下一个没有任何记录指向的孤儿文件
		if rmErr := s.storage.RemoveMany(docUID); rmErr != nil {
			slog.Error("回滚已落盘文件失败", "docUID", docUID, "err", rmErr)
		}
		failInternal(c, err)
		return
	}

	c.JSON(http.StatusCreated, doc)
}

// GET /api/v1/documents
func (s *Server) listDocuments(c *gin.Context) {
	f := store.ListFilter{
		Query:    strings.TrimSpace(c.Query("q")),
		Archived: c.Query("archived") == "true",
		Page:     atoiDefault(c.Query("page"), 1),
		PageSize: atoiDefault(c.Query("pageSize"), 20),
	}
	if raw := strings.TrimSpace(c.Query("categoryId")); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			fail(c, http.StatusBadRequest, codeBadRequest, "分类筛选参数无效")
			return
		}
		f.CategoryID = &id
	}

	items, total, err := s.store.ListDocuments(c.Request.Context(), f)
	if err != nil {
		failInternal(c, err)
		return
	}
	// 保证空结果是 [] 而不是 null，前端可以无条件遍历
	if items == nil {
		items = []store.Document{}
	}

	c.JSON(http.StatusOK, gin.H{
		"items":    items,
		"total":    total,
		"page":     f.Page,
		"pageSize": f.PageSize,
	})
}

// GET /api/v1/documents/:id
func (s *Server) getDocument(c *gin.Context) {
	doc, err := s.loadDocument(c)
	if err != nil {
		return
	}
	c.JSON(http.StatusOK, doc)
}

// GET /api/v1/documents/:id/download
// 默认以附件形式返回（下载内容与原文件逐字节一致）；
// 带 ?inline=true 时改为内联返回，供 PDF 在线预览使用。
func (s *Server) downloadDocument(c *gin.Context) {
	doc, err := s.loadDocument(c)
	if err != nil {
		return
	}

	f, info, err := s.storage.Open(doc.StorageKey)
	if err != nil {
		// 记录在库但盘上没有文件，这是“原文件丢失”，与索引失败是两回事，
		// 必须给出不同的原因，否则排障时会指错方向。
		slog.Error("原文件缺失", "docUID", doc.DocUID, "storageKey", doc.StorageKey, "err", err)
		fail(c, http.StatusNotFound, codeNotFound, "原文件已丢失，无法下载")
		return
	}
	defer f.Close()

	disposition := "attachment"
	if c.Query("inline") == "true" {
		disposition = "inline"
	}

	c.Header("Content-Disposition", contentDisposition(disposition, doc.Name))
	c.Header("Content-Type", doc.ContentType)
	// ServeContent 支持 Range 请求，PDF 预览器可以按需取分片
	http.ServeContent(c.Writer, c.Request, doc.Name, info.ModTime(), f)
}

type updateDocumentRequest struct {
	Name *string `json:"name"`
	// 用 RawMessage 才能区分「字段未出现」与「显式传 null」：
	// 前者表示不改归属，后者表示把文件移出所有分类。
	CategoryID json.RawMessage `json:"categoryId"`
	Tags       *[]string       `json:"tags"`
	Archived   *bool           `json:"archived"`
}

// PATCH /api/v1/documents/:id
func (s *Server) updateDocument(c *gin.Context) {
	docUID := c.Param("id")
	ctx := c.Request.Context()

	// 先确认文档存在，避免在不存在的文档上“更新成功”
	if _, err := s.store.GetDocument(ctx, docUID); err != nil {
		s.respondLoadError(c, err)
		return
	}

	var req updateDocumentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, codeBadRequest, "请求体不是合法的 JSON")
		return
	}

	fields := map[string]any{}

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			fail(c, http.StatusBadRequest, codeBadRequest, "文件名不能为空")
			return
		}
		if len([]rune(name)) > maxNameRunes {
			fail(c, http.StatusBadRequest, codeBadRequest,
				fmt.Sprintf("文件名不能超过 %d 个字符", maxNameRunes))
			return
		}
		fields["name"] = name
	}

	if req.CategoryID != nil {
		if string(req.CategoryID) == "null" {
			fields["category_id"] = nil
		} else {
			var id int64
			if err := json.Unmarshal(req.CategoryID, &id); err != nil || id <= 0 {
				fail(c, http.StatusBadRequest, codeBadRequest, "分类标识无效")
				return
			}
			exists, err := s.store.CategoryExists(ctx, id)
			if err != nil {
				failInternal(c, err)
				return
			}
			if !exists {
				fail(c, http.StatusBadRequest, codeBadRequest, "指定的分类不存在")
				return
			}
			fields["category_id"] = id
		}
	}

	if req.Tags != nil {
		normalized, err := normalizeTagList(*req.Tags)
		if err != nil {
			fail(c, http.StatusBadRequest, codeBadRequest, err.Error())
			return
		}
		fields["tags"] = normalized
	}

	if req.Archived != nil {
		fields["archived"] = *req.Archived
	}

	doc, err := s.store.UpdateDocument(ctx, docUID, fields)
	if err != nil {
		s.respondLoadError(c, err)
		return
	}
	c.JSON(http.StatusOK, doc)
}

// ---------------------------------------------------------------- 辅助函数

// loadDocument 读取路径参数指向的文档，失败时已写好响应。
func (s *Server) loadDocument(c *gin.Context) (*store.Document, error) {
	doc, err := s.store.GetDocument(c.Request.Context(), c.Param("id"))
	if err != nil {
		s.respondLoadError(c, err)
		return nil, err
	}
	return doc, nil
}

func (s *Server) respondLoadError(c *gin.Context, err error) {
	if errors.Is(err, store.ErrNotFound) {
		fail(c, http.StatusNotFound, codeNotFound, "文档不存在")
		return
	}
	failInternal(c, err)
}

// resolveCategoryInput 校验调用方传来的分类标识。
// 返回 (nil, true) 表示未指定分类，这是合法状态。
func (s *Server) resolveCategoryInput(c *gin.Context, raw string) (*int64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, true
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		fail(c, http.StatusBadRequest, codeBadRequest, "分类标识无效")
		return nil, false
	}
	exists, err := s.store.CategoryExists(c.Request.Context(), id)
	if err != nil {
		failInternal(c, err)
		return nil, false
	}
	if !exists {
		fail(c, http.StatusBadRequest, codeBadRequest, "指定的分类不存在")
		return nil, false
	}
	return &id, true
}

// normalizeTags 解析上传表单里的逗号分隔标签。
func normalizeTags(raw string) (pq.StringArray, error) {
	if strings.TrimSpace(raw) == "" {
		return pq.StringArray{}, nil
	}
	return normalizeTagList(strings.Split(raw, ","))
}

func normalizeTagList(in []string) (pq.StringArray, error) {
	out := make(pq.StringArray, 0, len(in))
	seen := map[string]bool{}
	for _, raw := range in {
		tag := strings.TrimSpace(raw)
		if tag == "" {
			continue
		}
		if len([]rune(tag)) > maxTagRunes {
			return nil, fmt.Errorf("单个标签不能超过 %d 个字符", maxTagRunes)
		}
		// 上传表单用逗号分隔标签，标签本身含逗号就无法无歧义地还原。
		// 两条写入路径（表单 / JSON）在这里统一拒绝，避免同一个标签在两边解析出不同结果。
		if strings.ContainsRune(tag, ',') {
			return nil, errors.New("标签内不能包含逗号")
		}
		if seen[tag] {
			continue
		}
		seen[tag] = true
		out = append(out, tag)
	}
	if len(out) > maxTagCount {
		return nil, fmt.Errorf("标签数量不能超过 %d 个", maxTagCount)
	}
	return out, nil
}

// sanitizeFileName 只保留最后一个路径分量并去掉控制字符。
// 浏览器一般只发文件名，但不能假设所有客户端都守规矩。
func sanitizeFileName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" {
		return "未命名文件"
	}
	if len([]rune(name)) > maxNameRunes {
		runes := []rune(name)
		name = string(runes[:maxNameRunes])
	}
	return name
}

// contentDisposition 用 RFC 5987 的 filename* 编码中文文件名，
// 避免浏览器把中文显示成乱码或直接丢弃文件名。
func contentDisposition(disposition, filename string) string {
	return fmt.Sprintf("%s; filename*=UTF-8''%s", disposition, url.PathEscape(filename))
}

func newDocUID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 在支持的平台上不会失败；真失败了也没有合理的降级方案
		panic("无法生成文档标识: " + err.Error())
	}
	return "doc_" + hex.EncodeToString(buf)
}

func atoiDefault(raw string, fallback int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
		return v
	}
	return fallback
}
