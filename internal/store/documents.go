package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// ErrNotFound 表示目标记录不存在，由 HTTP 层翻译成 404。
var ErrNotFound = errors.New("记录不存在")

type Store struct{ db *gorm.DB }

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
	// CategoryID 指定分类时，会连同它的所有子分类一起纳入筛选。
	// 树形筛选若只匹配一层，用户点父分类时会看到比子分类更少的结果，与直觉相反。
	CategoryID *int64
	// OnlyUncategorized 为 true 时只看没有归属分类的文件（对应树上的「未分类」）。
	// 与 CategoryID 互斥，CategoryID 优先。
	OnlyUncategorized bool
	// Archived 为 false 时只返回未归档文档，这是默认列表的语义
	Archived bool
	Page     int
	PageSize int
}

// where 拼出 WHERE 子句与对应的参数。
//
// 这里用字符串拼接而不是纯粹的参数化查询，是因为「是否按分类筛选」决定了
// 要不要多出一段子查询，占位符的个数随条件变化，写成一串
// `(? IS NULL OR ...)` 反而更难读、也更容易在新增条件时出错。
// 拼进去的全是本文件里的常量，用户输入一律走占位符。
func (f ListFilter) where() (string, []any) {
	var b strings.Builder
	args := []any{f.Archived}

	b.WriteString(" WHERE d.archived = ?")
	if f.Query != "" {
		b.WriteString(" AND d.name ILIKE '%' || ? || '%'")
		args = append(args, f.Query)
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
		` ORDER BY d.created_at DESC, d.id DESC LIMIT ? OFFSET ?`
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
