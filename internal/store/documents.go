package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
	"gorm.io/gorm"
)

// ErrNotFound 表示目标记录不存在，由 HTTP 层翻译成 404。
var ErrNotFound = errors.New("记录不存在")

// ErrNotTrashed 表示要对一份不在回收站里的文件做彻底删除。
//
// 彻底删除没有后悔药，所以它只允许在回收站里进行 —— 想毁掉一份正在用的文件，
// 必须先「移入回收站」再「彻底删除」两步。多出来的这一步不是仪式：
// 它保证误删永远有一步可以停下来。
var ErrNotTrashed = errors.New("文件不在回收站里")

// ErrAnchorNotFound 表示拖动时指定的落点文件不存在，由 HTTP 层翻译成 400。
//
// 与被拖的文件不存在（ErrNotFound，404）分开：前者是调用方给错了锚点，
// 后者是资源本身没了，两者的修法不一样。
var ErrAnchorNotFound = errors.New("锚点文件不存在")

// Store 是全部数据访问的入口：文档与分类走 GORM，
// 检索、分类树、索引任务队列走原生 SQL（见 docs/DESIGN.md 的关键取舍）。
type Store struct{ db *gorm.DB }

// New 用已有的连接池构造 Store。
func New(db *gorm.DB) *Store { return &Store{db: db} }

// Ping 检查数据库连通性，供健康接口使用。
func (s *Store) Ping(ctx context.Context) error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return fmt.Errorf("获取连接池失败: %w", err)
	}
	return sqlDB.PingContext(ctx)
}

// ListFilter 是列表查询的过滤条件。
type ListFilter struct {
	// Query 为空表示不按文件名过滤；非空时走 pg_trgm 索引的 ILIKE 匹配
	Query string
	// BodyMatches 为 true 时，Query 还要在正文片段里找一遍（关键词检索用）。
	// 默认 false：列表页的搜索框只按文件名过滤，在那里翻正文会让
	// 「搜文件名」的结果里混进一堆只是正文提到过它的文件。
	BodyMatches bool
	// CategoryID 指定分类时，会连同它的所有子分类一起纳入筛选。
	// 树形筛选若只匹配一层，用户点父分类时会看到比子分类更少的结果，与直觉相反。
	CategoryID *int64
	// OnlyUncategorized 为 true 时只看没有归属分类的文件（对应树上的「未分类」）。
	// 与 CategoryID 互斥，CategoryID 优先。
	OnlyUncategorized bool
	// Archived 为 false 时只返回未归档文档，这是默认列表的语义
	Archived bool
	// Trashed 为 true 时只返回回收站里的文件，此时 Archived 不起作用 ——
	// 删除吞掉了归档这层含义，两个条件叠加会出现「我明明删了它却不在回收站里」。
	Trashed  bool
	Page     int
	PageSize int
	// SnippetsPerDoc 是每份文档最多取回的命中片段数，仅检索路径使用。
	SnippetsPerDoc int
}

// where 拼出 WHERE 子句与对应的参数。
//
// 这里用字符串拼接而不是纯粹的参数化查询，是因为「是否按分类筛选」决定了
// 要不要多出一段子查询，占位符的个数随条件变化，写成一串
// `(? IS NULL OR ...)` 反而更难读、也更容易在新增条件时出错。
// 拼进去的全是本文件里的常量，用户输入一律走占位符。
func (f ListFilter) where() (string, []any) {
	var b strings.Builder
	var args []any

	// 默认列表、关键词检索、语义检索都走这一条 where，
	// 所以把 deleted_at 的判断放在这里，三个入口就都不会漏掉回收站里的文件 ——
	// 一处漏掉就是「删掉的文件还能被搜出来」，那比没有删除功能更糟。
	if f.Trashed {
		b.WriteString(" WHERE d.deleted_at IS NOT NULL")
	} else {
		b.WriteString(" WHERE d.deleted_at IS NULL AND d.archived = ?")
		args = append(args, f.Archived)
	}
	if f.Query != "" {
		if f.BodyMatches {
			// EXISTS 而不是 JOIN：一份文件正文里出现十次关键词，
			// 在结果列表里也只该占一行
			b.WriteString(` AND (d.name ILIKE '%' || ? || '%' OR EXISTS (
        SELECT 1 FROM document_chunks ch
        WHERE ch.document_id = d.id AND ch.content ILIKE '%' || ? || '%'))`)
			args = append(args, f.Query, f.Query)
		} else {
			b.WriteString(" AND d.name ILIKE '%' || ? || '%'")
			args = append(args, f.Query)
		}
	}
	switch {
	case f.CategoryID != nil:
		// 递归取该分类的整棵子树，一次查询完成，不依赖应用层遍历
		b.WriteString(` AND d.category_id IN (
        WITH RECURSIVE subtree AS (
            SELECT id FROM categories WHERE id = ?
            UNION ALL
            SELECT c.id FROM categories c JOIN subtree s ON c.parent_id = s.id
        )
        SELECT id FROM subtree)`)
		args = append(args, *f.CategoryID)
	case f.OnlyUncategorized:
		b.WriteString(" AND d.category_id IS NULL")
	}
	return b.String(), args
}

// ListDocuments 返回一页文档与命中总数。
//
// 用原生 SQL 而非 GORM 链式调用：ILIKE 与 pg_trgm 索引的配合、
// 分类子树的递归查询，写成 SQL 比拼 GORM 条件更清楚也更可控。
func (s *Store) ListDocuments(ctx context.Context, f ListFilter) ([]Document, int64, error) {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize < 1 || f.PageSize > 100 {
		f.PageSize = 20
	}
	offset := (f.Page - 1) * f.PageSize

	where, args := f.where()

	var items []Document
	listSQL := `SELECT d.*, c.name AS category_name
FROM documents d
LEFT JOIN categories c ON c.id = d.category_id` + where +
		` ORDER BY d.sort_order, d.id DESC LIMIT ? OFFSET ?`
	if err := s.db.WithContext(ctx).Raw(listSQL,
		append(append([]any{}, args...), f.PageSize, offset)...,
	).Scan(&items).Error; err != nil {
		return nil, 0, fmt.Errorf("查询文档列表失败: %w", err)
	}

	var total int64
	countSQL := `SELECT count(*) FROM documents d` + where
	if err := s.db.WithContext(ctx).Raw(countSQL, args...).Scan(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("统计文档总数失败: %w", err)
	}
	return items, total, nil
}

const getSelect = `
SELECT d.*, c.name AS category_name
FROM documents d
LEFT JOIN categories c ON c.id = d.category_id
WHERE d.doc_uid = ?`

// GetDocument 按对外标识取文档，取不到时返回 ErrNotFound。
func (s *Store) GetDocument(ctx context.Context, docUID string) (*Document, error) {
	return s.scanDocument(ctx, getSelect, docUID)
}

// GetDocumentByID 按自增主键取文档，供 worker 使用：
// 索引任务里存的是 document_id，而 doc_uid 是给外部用的编号，两者不要混。
func (s *Store) GetDocumentByID(ctx context.Context, id int64) (*Document, error) {
	const query = `
SELECT d.*, c.name AS category_name
FROM documents d
LEFT JOIN categories c ON c.id = d.category_id
WHERE d.id = ?`
	return s.scanDocument(ctx, query, id)
}

func (s *Store) scanDocument(ctx context.Context, query string, arg any) (*Document, error) {
	var doc Document
	if err := s.db.WithContext(ctx).Raw(query, arg).Scan(&doc).Error; err != nil {
		return nil, fmt.Errorf("查询文档失败: %w", err)
	}
	if doc.ID == 0 {
		return nil, ErrNotFound
	}
	return &doc, nil
}

// CreateDocument 在一个事务里写入文档记录与索引任务。
//
// 这两条记录必须同时成功或同时失败：如果只写入了文档而没有任务，
// 文件就会永远停在 pending 且没有任何东西会去处理它。
func (s *Store) CreateDocument(ctx context.Context, doc *Document, enqueueIndex bool) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 新文件排在最前，保住「最新的在上面」这个直觉。
		//
		// 这里刻意不加锁：并发两次上传会读到同一个 MIN，于是两个文件并列最前，
		// 由 (sort_order, id DESC) 分出先后，下一次上传再把 MIN 继续往下推。
		// 并列是无害的 —— 这正是 sort_order 上不加唯一约束的原因（0004 迁移里有说明）。
		var next int64
		if err := tx.Raw(
			`SELECT COALESCE(MIN(sort_order), 1) - 1 FROM documents`).Row().Scan(&next); err != nil {
			return fmt.Errorf("计算文档排序位置失败: %w", err)
		}
		doc.SortOrder = next

		if err := tx.Create(doc).Error; err != nil {
			return fmt.Errorf("写入文档记录失败: %w", err)
		}
		if !enqueueIndex {
			return nil
		}
		job := IndexJob{
			DocumentID:  doc.ID,
			Status:      JobQueued,
			MaxAttempts: 3,
			RunAfter:    time.Now(),
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
		}
		if err := tx.Create(&job).Error; err != nil {
			return fmt.Errorf("创建索引任务失败: %w", err)
		}
		return nil
	})
}

// UpdateDocument 做局部更新。fields 的键是数据库列名，由 HTTP 层白名单构造。
func (s *Store) UpdateDocument(ctx context.Context, docUID string, fields map[string]any) (*Document, error) {
	if len(fields) == 0 {
		return s.GetDocument(ctx, docUID)
	}
	res := s.db.WithContext(ctx).
		Model(&Document{}).
		Where("doc_uid = ?", docUID).
		Updates(fields)
	if res.Error != nil {
		return nil, fmt.Errorf("更新文档失败: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		// 可能是文档不存在，也可能只是字段值没有变化；再查一次以区分
		if _, err := s.GetDocument(ctx, docUID); err != nil {
			return nil, err
		}
	}
	return s.GetDocument(ctx, docUID)
}

// MoveDocumentAfter 把一份文件挪到另一份之后，afterUID 为 nil 表示置顶。
//
// 读的是全表而不是界面上那一页：顺序是整张表一条序列，筛选视图只是它的子序列，
// 「插到 B 之后」只有放在全局序列里才有唯一定义。接口因此收的是相对锚点而非绝对下标。
//
// 整表重排，而不是留间隔、按中点插入：中点方案在反复插到同一处（比如一直往最前面拖）
// 会耗尽间隔，需要一条「间隔不够了就重排一遍」的分支 —— 那条分支平时跑不到，
// 真跑起来恰恰是最需要正确的时候。这里每次走的都是同一条路径，代价是 O(n) 行写入，
// 在几百到几千份文档这个量级上可以忽略。
func (s *Store) MoveDocumentAfter(ctx context.Context, docUID string, afterUID *string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		type ordered struct {
			ID     int64  `gorm:"column:id"`
			DocUID string `gorm:"column:doc_uid"`
		}

		// 行级 FOR UPDATE，而不是 LOCK TABLE：这个仓库里的加锁惯例是前者。
		// 整表按同一个 ORDER BY 取行，于是并发拖动既会串行、加锁顺序又一致，不会互相死锁 ——
		// 若各读一份顺序再各自写回，后写的会把先写的整个盖掉。
		var rows []ordered
		if err := tx.Raw(
			`SELECT id, doc_uid FROM documents ORDER BY sort_order, id DESC FOR UPDATE`,
		).Scan(&rows).Error; err != nil {
			return fmt.Errorf("读取文档顺序失败: %w", err)
		}

		from := -1
		for i, row := range rows {
			if row.DocUID == docUID {
				from = i
				break
			}
		}
		if from < 0 {
			return ErrNotFound
		}

		// 锚点就是自己：没得可挪。必须在摘除之前判断 —— 摘除之后自己已经不在
		// 待查的行里，会当成「锚点不存在」误报成 400。
		if afterUID != nil && *afterUID == docUID {
			return nil
		}

		// 先把被拖的行摘出来，再在剩下的行里找锚点：
		// 否则「拖到紧挨着自己前面/后面」会算出一个差一格的位置。
		rest := make([]ordered, 0, len(rows)-1)
		rest = append(rest, rows[:from]...)
		rest = append(rest, rows[from+1:]...)

		at := 0 // 没有锚点就是置顶
		if afterUID != nil {
			anchor := -1
			for i, row := range rest {
				if row.DocUID == *afterUID {
					anchor = i
					break
				}
			}
			if anchor < 0 {
				return ErrAnchorNotFound
			}
			at = anchor + 1
		}

		next := make([]ordered, 0, len(rows))
		next = append(next, rest[:at]...)
		next = append(next, rows[from])
		next = append(next, rest[at:]...)

		// 落点即原位：不去写库。拖动落回原处、或客户端重发一次同样的请求，都不该动数据。
		moved := false
		for i, row := range next {
			if row.ID != rows[i].ID {
				moved = true
				break
			}
		}
		if !moved {
			return nil
		}

		ids := make([]int64, len(next))
		for i, row := range next {
			ids[i] = row.ID
		}

		// 一条语句写回整条序列：WITH ORDINALITY 自带 1..n 的序号，
		// 于是只需要传一个新序的 id 数组，序号天然稠密。
		//
		// 只写 sort_order，不碰 updated_at —— 挪位置不是内容变更，
		// 顺手把「更新时间」也改掉属于撒谎。
		if err := tx.Exec(`
UPDATE documents AS d
SET sort_order = t.ord
FROM unnest(?::bigint[]) WITH ORDINALITY AS t(id, ord)
WHERE d.id = t.id`, pq.Int64Array(ids)).Error; err != nil {
			return fmt.Errorf("写入文档顺序失败: %w", err)
		}
		return nil
	})
}

// SoftDeleteDocument 把文件移进回收站。
//
// 只改标记位：磁盘上的字节与已有的正文片段都不动。这不是偷懒 ——
// 恢复之后必须立刻能被搜到，如果删除时把片段清掉，恢复就得重跑一遍索引，
// 而用户眼里「我刚恢复的文件搜不到」和「恢复失败」是分不清的。
//
// 对已在回收站里的文件重复调用不会重置时间：进站时间应当是第一次删的时刻，
// 被重复请求推后会让这个信息失去意义。结果与第一次相同，按成功返回。
func (s *Store) SoftDeleteDocument(ctx context.Context, docUID string) (*Document, error) {
	if err := s.db.WithContext(ctx).Exec(`
UPDATE documents SET deleted_at = now(), updated_at = now()
WHERE doc_uid = ? AND deleted_at IS NULL`, docUID).Error; err != nil {
		return nil, fmt.Errorf("移入回收站失败: %w", err)
	}
	// 没改到行有两种可能：文件不存在，或者它本来就在回收站里。
	// 前者由 GetDocument 返回 ErrNotFound，后者原样返回当前记录，两者都能区分开。
	return s.GetDocument(ctx, docUID)
}

// RestoreFromTrash 把文件移出回收站。
//
// Archived 保持原样：归档状态在删除期间没有被改动过，恢复就是回到删除前的那一边。
// 同样对重复调用幂等。
func (s *Store) RestoreFromTrash(ctx context.Context, docUID string) (*Document, error) {
	if err := s.db.WithContext(ctx).Exec(`
UPDATE documents SET deleted_at = NULL, updated_at = now()
WHERE doc_uid = ? AND deleted_at IS NOT NULL`, docUID).Error; err != nil {
		return nil, fmt.Errorf("从回收站恢复失败: %w", err)
	}
	return s.GetDocument(ctx, docUID)
}

// PurgeDocument 彻底删除一份文件，返回被删掉的那条记录，
// 调用方据此去清理磁盘上的目录。
//
// 只做库里的删除，不碰磁盘：删除顺序与文件清理分开，是为了让「删除」这个动作
// 在库里是原子的。盘上残留的目录没有任何记录指向它，最坏只是浪费空间；
// 反过来（先删盘）一旦库里的删除失败，就会留下一条点下载必报「原文件已丢失」的记录。
//
// document_chunks 与 index_jobs 上的外键是 ON DELETE CASCADE，随这一行一起消失。
func (s *Store) PurgeDocument(ctx context.Context, docUID string) (*Document, error) {
	doc, err := s.GetDocument(ctx, docUID)
	if err != nil {
		return nil, err
	}
	if doc.DeletedAt == nil {
		return nil, ErrNotTrashed
	}
	if err := s.db.WithContext(ctx).Exec(
		`DELETE FROM documents WHERE doc_uid = ?`, docUID).Error; err != nil {
		return nil, fmt.Errorf("彻底删除文件失败: %w", err)
	}
	return doc, nil
}

// PurgeAllTrashed 清空回收站，返回被删掉的全部记录，供调用方清理磁盘目录。
//
// 用一条 DELETE ... RETURNING 而不是先查后删：两条语句之间插进来一次软删除，
// 那份文件就会被漏掉磁盘清理，留下一份孤儿目录而没有任何提示。
func (s *Store) PurgeAllTrashed(ctx context.Context) ([]Document, error) {
	var docs []Document
	if err := s.db.WithContext(ctx).Raw(
		`DELETE FROM documents WHERE deleted_at IS NOT NULL RETURNING *`).Scan(&docs).Error; err != nil {
		return nil, fmt.Errorf("清空回收站失败: %w", err)
	}
	if docs == nil {
		docs = []Document{}
	}
	return docs, nil
}

// CategoryExists 校验分类是否有效。上传与移动归属时都要用它，
// 避免引用一个不存在的分类导致外键报错而不是给出可读的 400。
func (s *Store) CategoryExists(ctx context.Context, id int64) (bool, error) {
	var count int64
	if err := s.db.WithContext(ctx).Raw(
		`SELECT count(*) FROM categories WHERE id = ?`, id).Scan(&count).Error; err != nil {
		return false, fmt.Errorf("校验分类失败: %w", err)
	}
	return count > 0, nil
}
