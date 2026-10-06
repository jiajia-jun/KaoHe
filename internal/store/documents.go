package store

import (
	"context"
	"errors"
	"fmt"
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
	// CategoryID 是精确分类筛选，nil 表示不筛选
	CategoryID *int64
	// Archived 为 false 时只返回未归档文档，这是默认列表的语义
	Archived bool
	Page     int
	PageSize int
}

const listSelect = `
SELECT d.*, c.name AS category_name
FROM documents d
LEFT JOIN categories c ON c.id = d.category_id
WHERE d.archived = ?
  AND (? = '' OR d.name ILIKE '%' || ? || '%')
  AND (?::bigint IS NULL OR d.category_id = ?)
ORDER BY d.created_at DESC, d.id DESC
LIMIT ? OFFSET ?`

const countSelect = `
SELECT count(*)
FROM documents d
WHERE d.archived = ?
  AND (? = '' OR d.name ILIKE '%' || ? || '%')
  AND (?::bigint IS NULL OR d.category_id = ?)`

// ListDocuments 返回一页文档与命中总数。
// 用原生 SQL 而非 GORM 链式调用：ILIKE 与 pg_trgm 索引的配合、
// 以及可空的分类筛选，写成 SQL 比拼 GORM 条件更清楚也更可控。
func (s *Store) ListDocuments(ctx context.Context, f ListFilter) ([]Document, int64, error) {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize < 1 || f.PageSize > 100 {
		f.PageSize = 20
	}
	offset := (f.Page - 1) * f.PageSize

	var items []Document
	err := s.db.WithContext(ctx).Raw(listSelect,
		f.Archived, f.Query, f.Query, f.CategoryID, f.CategoryID, f.PageSize, offset,
	).Scan(&items).Error
	if err != nil {
		return nil, 0, fmt.Errorf("查询文档列表失败: %w", err)
	}

	var total int64
	err = s.db.WithContext(ctx).Raw(countSelect,
		f.Archived, f.Query, f.Query, f.CategoryID, f.CategoryID,
	).Scan(&total).Error
	if err != nil {
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
	var doc Document
	err := s.db.WithContext(ctx).Raw(getSelect, docUID).Scan(&doc).Error
	if err != nil {
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
