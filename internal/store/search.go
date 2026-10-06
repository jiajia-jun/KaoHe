package store

import (
	"context"
	"fmt"
	"strings"
)

// 检索取数的上界。放在 store 里而不是 HTTP 层，是因为它们直接决定 SQL 取多少行。
const (
	// DefaultSnippetsPerDoc 是每份文档默认展示的命中片段数
	DefaultSnippetsPerDoc = 3
	maxSnippetsPerDoc     = 10
	// semanticChunkLimit 是语义检索在片段层先取回的候选数。
	//
	// 向量检索必须在片段这一层排序（HNSW 索引建在 document_chunks 上），
	// 所以先按距离取一批候选，再按文档归并，比在 SQL 里对全部片段算完距离再分组快得多，
	// 也让索引真的被用上。取 200 是因为一篇文档最多也就几段能挤进前列。
	semanticChunkLimit = 200
)

// SearchHit 是一份文档在关键词检索里的命中情况。
type SearchHit struct {
	Document Document `json:"document"`
	// NameHit 表示文件名本身命中了关键词
	NameHit bool `json:"nameHit"`
	// BodyHits 是正文命中的片段总数，可能大于 Matches 的长度
	BodyHits int `json:"bodyHits"`
	// Matches 是最靠前的若干段命中内容，由上层截成摘要后再展示
	Matches []MatchedChunk `json:"matches"`
}

// MatchedChunk 是一段命中了关键词的正文。
type MatchedChunk struct {
	Ordinal int    `json:"ordinal"`
	Content string `json:"content"`
}

// SearchDocuments 按关键词检索：文件名命中或正文片段命中都算。
//
// 与 ListDocuments 分开写而不是加个开关：检索要对结果排序
// （文件名命中的排前面），还要再查一次正文把命中片段捞出来，
// 塞进列表接口会让那条本来就简单的查询变成两个用途的混合体。
func (s *Store) SearchDocuments(ctx context.Context, f ListFilter) ([]SearchHit, int64, error) {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize < 1 || f.PageSize > 100 {
		f.PageSize = 20
	}
	f.BodyMatches = true
	f.SnippetsPerDoc = clampSnippets(f.SnippetsPerDoc)
	offset := (f.Page - 1) * f.PageSize

	where, args := f.where()

	var docs []Document
	listSQL := `SELECT d.*, c.name AS category_name
FROM documents d
LEFT JOIN categories c ON c.id = d.category_id` + where +
		// 文件名命中的排在前面：用户敲进去的多半是想找某个文件，
		// 正文里顺带提到过这个词只是补充。同组内按上传时间倒序。
		` ORDER BY (d.name ILIKE '%' || ? || '%') DESC, d.created_at DESC, d.id DESC
LIMIT ? OFFSET ?`
	if err := s.db.WithContext(ctx).Raw(listSQL,
		append(append([]any{}, args...), f.Query, f.PageSize, offset)...,
	).Scan(&docs).Error; err != nil {
		return nil, 0, fmt.Errorf("检索文档失败: %w", err)
	}

	var total int64
	countSQL := `SELECT count(*) FROM documents d` + where
	if err := s.db.WithContext(ctx).Raw(countSQL, args...).Scan(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("统计检索结果失败: %w", err)
	}

	hits := make([]SearchHit, len(docs))
	ids := make([]int64, 0, len(docs))
	pos := make(map[int64]int, len(docs))
	for i, doc := range docs {
		hits[i] = SearchHit{
			Document: doc,
			NameHit:  containsFold(doc.Name, f.Query),
			Matches:  []MatchedChunk{},
		}
		ids = append(ids, doc.ID)
		pos[doc.ID] = i
	}

	if len(ids) > 0 {
		if err := s.fillBodyMatches(ctx, hits, pos, ids, f); err != nil {
			return nil, 0, err
		}
	}
	return hits, total, nil
}

// fillBodyMatches 把这一页文档的正文命中片段补进结果里。
//
// 单独一次查询而不是在列表 SQL 里做聚合：聚合出来的会是 json 或数组，
// 在 Go 里还要再解一遍，不如直接取回行、在内存里归类。
func (s *Store) fillBodyMatches(
	ctx context.Context, hits []SearchHit, pos map[int64]int, ids []int64, f ListFilter,
) error {
	// total 用窗口函数在过滤前算：取回的片段有上限，而「还有多少段命中」
	// 是用户判断要不要点进详情看的依据，不能跟着上限一起被截断。
	const query = `
WITH matched AS (
    SELECT ch.document_id, ch.ordinal, ch.content,
           count(*) OVER (PARTITION BY ch.document_id) AS total,
           row_number() OVER (PARTITION BY ch.document_id ORDER BY ch.ordinal) AS rn
    FROM document_chunks ch
    WHERE ch.document_id IN (?) AND ch.content ILIKE '%' || ? || '%'
)
SELECT matched.document_id, matched.ordinal, matched.content, matched.total
FROM matched
WHERE matched.rn <= ?
ORDER BY matched.document_id, matched.ordinal`

	var rows []struct {
		DocumentID int64
		Ordinal    int
		Content    string
		Total      int
	}
	if err := s.db.WithContext(ctx).Raw(query, ids, f.Query, f.SnippetsPerDoc).
		Scan(&rows).Error; err != nil {
		return fmt.Errorf("查询正文命中片段失败: %w", err)
	}

	for _, row := range rows {
		i, ok := pos[row.DocumentID]
		if !ok {
			continue
		}
		hits[i].BodyHits = row.Total
		hits[i].Matches = append(hits[i].Matches, MatchedChunk{
			Ordinal: row.Ordinal,
			Content: row.Content,
		})
	}
	return nil
}

// SemanticFilter 是语义检索的条件。
type SemanticFilter struct {
	ListFilter
	// Vector 是查询文本的向量，维度必须与 embedding 列一致
	Vector []float32
	// MinScore 是余弦相似度下限，低于它的片段不返回。
	// 向量检索永远会返回「最像的几条」，哪怕它们其实都不相关，
	// 靠这个下限把它变成可以返回空结果。
	MinScore float32
	// MaxDocuments 是返回的文档数上限
	MaxDocuments int
}

// SemanticHit 是一份文档在语义检索里的命中。
type SemanticHit struct {
	Document Document `json:"document"`
	// Score 是该文档最相关片段的相似度，用于排序与展示
	Score   float32       `json:"score"`
	Matches []ScoredChunk `json:"matches"`
}

// ScoredChunk 是一段被语义检索命中的正文。
type ScoredChunk struct {
	Ordinal int     `json:"ordinal"`
	Content string  `json:"content"`
	Score   float32 `json:"score"`
}

// SearchChunksByVector 用余弦距离找最相关的正文片段，再按文档归并。
//
// 距离算子 <=> 与建在 embedding 上的 HNSW 索引（vector_cosine_ops）配套，
// 顺序不能换成 <->（L2）：索引算子类与查询算子不匹配时索引不会被使用。
func (s *Store) SearchChunksByVector(ctx context.Context, f SemanticFilter) ([]SemanticHit, error) {
	if len(f.Vector) == 0 {
		return nil, fmt.Errorf("查询向量为空")
	}
	if f.MaxDocuments < 1 {
		f.MaxDocuments = 10
	}
	f.SnippetsPerDoc = clampSnippets(f.SnippetsPerDoc)
	f.Query = "" // 语义检索不看文件名，ListFilter 里的关键词条件必须清掉

	where, args := f.where()
	literal := vectorLiteral(f.Vector)

	query := `
WITH scored AS (
    SELECT ch.document_id, ch.ordinal, ch.content,
           1 - (ch.embedding <=> ?::vector) AS score
    FROM document_chunks ch
    JOIN documents d ON d.id = ch.document_id` + where + `
    ORDER BY ch.embedding <=> ?::vector
    LIMIT ?
), ranked AS (
    SELECT scored.document_id, scored.ordinal, scored.content, scored.score,
           max(scored.score) OVER (PARTITION BY scored.document_id) AS best,
           row_number() OVER (PARTITION BY scored.document_id
                              ORDER BY scored.score DESC, scored.ordinal) AS rn
    FROM scored
    WHERE scored.score >= ?
)
SELECT d.*, c.name AS category_name,
       ranked.ordinal, ranked.content, ranked.score, ranked.best
FROM ranked
JOIN documents d ON d.id = ranked.document_id
LEFT JOIN categories c ON c.id = d.category_id
WHERE ranked.rn <= ?
ORDER BY ranked.best DESC, ranked.document_id, ranked.ordinal`

	// 向量出现两次（投影里算分数、ORDER BY 里给它用索引），
	// 其余参数按 SQL 中出现的先后排列
	full := append([]any{literal}, args...)
	full = append(full, literal, semanticChunkLimit, f.MinScore, f.SnippetsPerDoc)

	var rows []struct {
		Document
		Ordinal int
		Content string
		Score   float32
		Best    float32
	}
	if err := s.db.WithContext(ctx).Raw(query, full...).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("语义检索失败: %w", err)
	}

	hits := make([]SemanticHit, 0, f.MaxDocuments)
	for _, row := range rows {
		// 结果按 best 降序、文档分组排列，同一份文档的行必然相邻
		if len(hits) == 0 || hits[len(hits)-1].Document.ID != row.ID {
			if len(hits) >= f.MaxDocuments {
				break
			}
			hits = append(hits, SemanticHit{
				Document: row.Document,
				Score:    row.Best,
				Matches:  []ScoredChunk{},
			})
		}
		last := &hits[len(hits)-1]
		last.Matches = append(last.Matches, ScoredChunk{
			Ordinal: row.Ordinal,
			Content: row.Content,
			Score:   row.Score,
		})
	}
	return hits, nil
}

func clampSnippets(n int) int {
	if n < 1 {
		return DefaultSnippetsPerDoc
	}
	return min(n, maxSnippetsPerDoc)
}

// containsFold 做大小写不敏感的子串判断，用于标记「文件名命中」。
// 与库里的 ILIKE 行为对齐：数据库说命中了，这里也得说命中。
func containsFold(haystack, needle string) bool {
	if needle == "" {
		return false
	}
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}
