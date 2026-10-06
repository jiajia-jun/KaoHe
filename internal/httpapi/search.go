package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"KaoHe/internal/snippet"
	"KaoHe/internal/store"
)

// 检索的两条路径共用同一套分组结构，只在「怎么算命中」上不同。
const (
	// maxQueryRunes 是查询串长度上限。自然语言描述需求也就是一两句话，
	// 再长的输入对检索没有帮助，只会让向量化白算。
	maxQueryRunes = 200
	// semanticTopDocuments 是语义检索返回的文档数上限
	semanticTopDocuments = 10
)

type snippetView struct {
	Ordinal int      `json:"ordinal"`
	Text    string   `json:"text"`
	Score   *float32 `json:"score,omitempty"`
}

// GET /api/v1/search
// 关键词检索：文件名与正文一起找，返回命中片段。
func (s *Server) searchDocuments(c *gin.Context) {
	f, ok := parseListFilter(c)
	if !ok {
		return
	}
	if f.Query == "" {
		fail(c, http.StatusBadRequest, codeBadRequest, "请输入要检索的关键词")
		return
	}
	if len([]rune(f.Query)) > maxQueryRunes {
		fail(c, http.StatusBadRequest, codeBadRequest,
			"检索词过长，请控制在 "+strconv.Itoa(maxQueryRunes)+" 个字符以内")
		return
	}

	hits, total, err := s.store.SearchDocuments(c.Request.Context(), f)
	if err != nil {
		failInternal(c, err)
		return
	}

	items := make([]gin.H, 0, len(hits))
	for _, hit := range hits {
		matches := make([]snippetView, 0, len(hit.Matches))
		for _, m := range hit.Matches {
			// 片段里找不到关键词时退回开头一段：库里的 ILIKE 与这里的
			// 大小写折叠规则不一定完全一致，宁可给一段上下文，也不要空着
			text, found := snippet.Around(m.Content, f.Query, snippet.DefaultContext)
			if !found {
				text = head(m.Content, snippet.DefaultContext*2)
			}
			matches = append(matches, snippetView{Ordinal: m.Ordinal, Text: text})
		}
		items = append(items, gin.H{
			"document": hit.Document,
			"nameHit":  hit.NameHit,
			"bodyHits": hit.BodyHits,
			"matches":  matches,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"query":    f.Query,
		"items":    items,
		"total":    total,
		"page":     f.Page,
		"pageSize": f.PageSize,
	})
}

type semanticRequest struct {
	Query string `json:"query"`
	// CategoryID 与 Archived 与列表页的筛选参数同义
	CategoryID json.Number `json:"categoryId"`
	Archived   bool        `json:"archived"`
	TopK       int         `json:"topK"`
	// MinScore 为 0 时用服务端默认值，而不是「相似度必须大于 0」——
	// 后者等于没有下限，任何输入都会返回满屏结果
	MinScore float32 `json:"minScore"`
}

// POST /api/v1/search/semantic
// 语义检索：把自然语言描述转成向量，在片段上做余弦相似度检索。
func (s *Server) searchSemantic(c *gin.Context) {
	var req semanticRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, codeBadRequest, "请求体不是合法的 JSON")
		return
	}
	query := strings.TrimSpace(req.Query)
	if query == "" {
		fail(c, http.StatusBadRequest, codeBadRequest, "请输入要检索的内容描述")
		return
	}
	if len([]rune(query)) > maxQueryRunes {
		fail(c, http.StatusBadRequest, codeBadRequest,
			"检索描述过长，请控制在 "+strconv.Itoa(maxQueryRunes)+" 个字符以内")
		return
	}

	filter := store.ListFilter{Archived: req.Archived}
	if req.CategoryID != "" {
		id, err := req.CategoryID.Int64()
		if err != nil || id <= 0 {
			fail(c, http.StatusBadRequest, codeBadRequest, "分类筛选参数无效")
			return
		}
		filter.CategoryID = &id
	}

	// 向量化失败最常见的原因是边车没起来。这不该是 500：
	// 服务本身是好的，只是这一条检索能力暂时不可用，界面要能把这件事说清楚。
	vectors, err := s.embed.Embed(c.Request.Context(), []string{query})
	if err != nil {
		slog.Error("语义检索：查询向量化失败", "err", err)
		fail(c, http.StatusServiceUnavailable, codeUnavailable,
			"向量服务暂不可用，语义检索无法进行；关键词检索和文件管理不受影响")
		return
	}
	if len(vectors) != 1 {
		failInternal(c, errors.New("向量服务返回的条数与请求不符"))
		return
	}

	minScore := req.MinScore
	if minScore <= 0 {
		minScore = s.cfg.SemanticMinScore
	}
	topK := req.TopK
	if topK < 1 || topK > semanticTopDocuments {
		topK = semanticTopDocuments
	}

	hits, err := s.store.SearchChunksByVector(c.Request.Context(), store.SemanticFilter{
		ListFilter:   filter,
		Vector:       vectors[0],
		MinScore:     minScore,
		MaxDocuments: topK,
	})
	if err != nil {
		failInternal(c, err)
		return
	}

	items := make([]gin.H, 0, len(hits))
	for _, hit := range hits {
		matches := make([]snippetView, 0, len(hit.Matches))
		for _, m := range hit.Matches {
			score := m.Score
			matches = append(matches, snippetView{
				Ordinal: m.Ordinal,
				// 语义检索的片段本身就是「相关片段」，不做二次截取：
				// 它是模型看过的那段原文，截短反而丢掉了判断相关性的依据
				Text:  m.Content,
				Score: &score,
			})
		}
		items = append(items, gin.H{
			"document": hit.Document,
			"score":    hit.Score,
			"matches":  matches,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"query": query,
		"items": items,
		"total": len(items),
		// 回传阈值，界面才能说明「低于这条线的片段没有展示」，
		// 而不是让用户以为库里就只有这么多东西
		"minScore": minScore,
	})
}

// head 取一段文本的开头 n 个字符，超出部分用省略号收尾。
func head(text string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	return string(runes[:n]) + "…"
}
