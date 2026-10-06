package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
	"go.uber.org/zap"

	"KaoHe/internal/filekind"
	"KaoHe/internal/store"
)

const (
	maxNameRunes = 200
	maxTagCount  = 20
	maxTagRunes  = 32
)

// 允许的格式与各自的能力边界来自 filekind：worker 抽取正文时用的是同一份定义，
// 两边各写一份清单迟早会漏改一处，出现「上传时说会索引、取任务时又不认」。
// 判定以扩展名为准，不做 MIME 嗅探 —— 这一取舍记在 docs/DESIGN.md 的已知限制里。

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
	kind, supported := filekind.Lookup(ext)
	if !supported {
		fail(c, http.StatusBadRequest, codeUnsupported, unsupportedExtMessage())
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
		ContentType:   kind.ContentType,
		SizeBytes:     size,
		CategoryID:    categoryID,
		Tags:          tags,
		StorageStatus: "stored",
		IndexStatus:   store.IndexPending,
	}

	enqueueIndex := filekind.IsExtractable(ext)
	if !enqueueIndex {
		doc.IndexStatus = store.IndexNotSupported
	}

	if err := s.store.CreateDocument(c.Request.Context(), doc, enqueueIndex); err != nil {
		// 写库失败就回滚磁盘，否则会留下一个没有任何记录指向的孤儿文件
		if rmErr := s.storage.RemoveMany(docUID); rmErr != nil {
			LoggerFrom(c).Error("回滚已落盘文件失败",
				zap.String("docUID", docUID), zap.Error(rmErr))
		}
		failInternal(c, err)
		return
	}

	// 入队这条日志是跨文件排查的连接点：worker 处理这份文档时记的是同一个
	// documentId（外加 jobId），两个日志文件靠它串起来。
	// 没有这一行，「上传成功但一直没索引」就只能翻库才知道该去 grep 哪个 id。
	LoggerFrom(c).Info("文档已入库",
		zap.Int64("documentId", doc.ID),
		zap.String("docUID", doc.DocUID),
		zap.Bool("enqueued", enqueueIndex),
	)

	c.JSON(http.StatusCreated, doc)
}

// GET /api/v1/documents
func (s *Server) listDocuments(c *gin.Context) {
	f, ok := parseListFilter(c)
	if !ok {
		return
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
		LoggerFrom(c).Error("原文件缺失",
			zap.String("docUID", doc.DocUID),
			zap.String("storageKey", doc.StorageKey),
			zap.Error(err))
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

// DELETE /api/v1/documents/:id
//
// 移入回收站（软删除）：只改标记位，磁盘上的原文件与已建好的索引都不动。
// 因此恢复之后立刻就能被搜到，不需要重跑一遍索引。
//
// 不做二次确认是刻意的：这一步可以撤销，多一个弹窗只会让用户对着一个
// 随时能反悔的操作犹豫。真正会丢东西的是彻底删除，那一个才需要拦一下。
func (s *Server) deleteDocument(c *gin.Context) {
	doc, err := s.store.SoftDeleteDocument(c.Request.Context(), c.Param("id"))
	if err != nil {
		s.respondLoadError(c, err)
		return
	}
	c.JSON(http.StatusOK, doc)
}

// POST /api/v1/documents/:id/restore
//
// 从回收站恢复。归档状态保持删除前的样子：删除期间没人动过它，
// 恢复就是回到原来的那一边，而不是一律回到「使用中」。
func (s *Server) restoreDocument(c *gin.Context) {
	doc, err := s.store.RestoreFromTrash(c.Request.Context(), c.Param("id"))
	if err != nil {
		s.respondLoadError(c, err)
		return
	}
	c.JSON(http.StatusOK, doc)
}

type moveDocumentRequest struct {
	// 指针就够表达三种写法：不传、传 null、传一个标识 —— 前两者都是置顶。
	AfterID *string `json:"afterId"`
}

// POST /api/v1/documents/:id/position
//
// 把文件挪到 afterId 指向的那份文件之后；afterId 缺省或为 null 表示置顶。
//
// 收相对锚点而不是绝对下标：筛选出来的列表只是全局序列的子序列，
// 「插到 B 之后」在任何子序列里都只有一种解释；下标则要求调用方和我
// 停在同一页、同一筛选条件上，稍有不同步就会插到别的地方去。
func (s *Server) moveDocument(c *gin.Context) {
	docUID := c.Param("id")
	ctx := c.Request.Context()

	// 先确认文档存在：在不存在的文档上「调整成功」没有任何意义
	if _, err := s.store.GetDocument(ctx, docUID); err != nil {
		s.respondLoadError(c, err)
		return
	}

	var req moveDocumentRequest
	// 允许空请求体，等价于置顶；只有真的带了 body 才去解析
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			fail(c, http.StatusBadRequest, codeBadRequest, "请求体不是合法的 JSON")
			return
		}
	}
	if req.AfterID != nil {
		if after := strings.TrimSpace(*req.AfterID); after == "" {
			req.AfterID = nil
		} else {
			req.AfterID = &after
		}
	}

	// 落到自己身上不算错：store 会当成「没有变化」直接放行
	if err := s.store.MoveDocumentAfter(ctx, docUID, req.AfterID); err != nil {
		if errors.Is(err, store.ErrAnchorNotFound) {
			fail(c, http.StatusBadRequest, codeBadRequest, "落点文件不存在")
			return
		}
		s.respondLoadError(c, err)
		return
	}

	doc, err := s.store.GetDocument(ctx, docUID)
	if err != nil {
		s.respondLoadError(c, err)
		return
	}
	c.JSON(http.StatusOK, doc)
}

// DELETE /api/v1/documents/:id/purge
//
// 彻底删除：从库里删掉记录（片段与索引任务随外键一起消失），再清理磁盘目录。
//
// 顺序是先删库、后删盘，且删盘失败不回滚、不报错。盘上留下的孤儿目录没有任何
// 记录指向它，最坏只是占地方；而一旦反过来，删盘成功但删库失败，库里那条记录
// 会变成点下载必报「原文件已丢失」的坏数据 —— 那是用户看得见的故障。
func (s *Server) purgeDocument(c *gin.Context) {
	doc, err := s.store.PurgeDocument(c.Request.Context(), c.Param("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotTrashed) {
			fail(c, http.StatusConflict, codeConflict,
				"该文件不在回收站里；彻底删除不可恢复，请先把它移入回收站")
			return
		}
		s.respondLoadError(c, err)
		return
	}
	s.purgeFiles(LoggerFrom(c), []store.Document{*doc})

	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("「%s」已彻底删除", doc.Name),
	})
}

// DELETE /api/v1/trash
//
// 清空回收站。库里一次删完，磁盘逐个目录清理 —— 同样允许个别目录清理失败，
// 理由与 purgeDocument 相同：记录已经没了，对用户而言文件确实已经删掉。
func (s *Server) emptyTrash(c *gin.Context) {
	docs, err := s.store.PurgeAllTrashed(c.Request.Context())
	if err != nil {
		failInternal(c, err)
		return
	}
	s.purgeFiles(LoggerFrom(c), docs)

	c.JSON(http.StatusOK, gin.H{
		"purged":  len(docs),
		"message": fmt.Sprintf("已清空回收站，%d 个文件被彻底删除", len(docs)),
	})
}

// purgeFiles 尽力清理这些记录对应的磁盘目录。
//
// 失败只记日志、不往外报：库里的记录已经删掉了，这一步失败留下的是一个谁也不引用
// 的目录。把一次已经完成的删除报成失败，会让用户反复重试一个无事可做的动作，
// 而他重试时看到的仍然是「文件不在了」——两次提示互相矛盾，比一个孤儿目录更糟。
// log 由调用方传入：这里没有 *gin.Context，而日志必须带上 requestId
// 才能和触发这次清理的那个请求对上。两个调用点都拿得到 c，穿透来即可。
func (s *Server) purgeFiles(log *zap.Logger, docs []store.Document) {
	for _, doc := range docs {
		if err := s.storage.RemoveMany(doc.DocUID); err != nil {
			log.Error("彻底删除后清理磁盘目录失败，留下孤儿目录",
				zap.String("docUID", doc.DocUID), zap.Error(err))
		}
	}
}

// ---------------------------------------------------------------- 辅助函数

// unsupportedExtMessage 拼出「不支持该格式」的 400 文案。
//
// 清单从 filekind 推导，而不是在这里写死一份 —— 写死的那份漂过：
// .markdown 早在 registry 里注册了（filekind.go），文案却一直只写 .md，
// 用户按提示把扩展名改成 .markdown 反而被拒，提示成了误导。
//
// 单独抽成函数是为了能被用例钉住（documents_test.go）：
// 只要文案里的清单和 registry 对不上，用例就红，不必等到线上收到一个坏提示。
//
// 排序放在这里而不是 SupportedExtensions 里：那个函数遍历 map，本身顺序不稳定，
// /config 也是在调用处排的（见 config.go），两处保持同一套做法。
func unsupportedExtMessage() string {
	exts := filekind.SupportedExtensions()
	sort.Strings(exts)
	return fmt.Sprintf("暂不支持该文件格式，目前支持 %s", strings.Join(exts, " / "))
}

// parseListFilter 解析列表与检索共用的筛选参数，失败时已写好 400 响应。
func parseListFilter(c *gin.Context) (store.ListFilter, bool) {
	f := store.ListFilter{
		Query:    strings.TrimSpace(c.Query("q")),
		Archived: c.Query("archived") == "true",
		Trashed:  c.Query("trashed") == "true",
		Page:     atoiDefault(c.Query("page"), 1),
		PageSize: atoiDefault(c.Query("pageSize"), 20),
	}
	raw := strings.TrimSpace(c.Query("categoryId"))
	if raw == "" {
		return f, true
	}
	// categoryId=none 表示「未分类」，这是树上的一类节点，
	// 但它对应的是 category_id IS NULL，没法用分类标识表达
	if raw == "none" {
		f.OnlyUncategorized = true
		return f, true
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		fail(c, http.StatusBadRequest, codeBadRequest, "分类筛选参数无效")
		return f, false
	}
	f.CategoryID = &id
	return f, true
}

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
