package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

// 分类相关的领域错误。HTTP 层据此选择状态码与可读文案，
// 而不是去解析数据库驱动返回的字符串。
var (
	ErrCategoryNotFound    = errors.New("分类不存在")
	ErrCategoryNameTaken   = errors.New("同级下已存在同名分类")
	ErrCategoryHasChild    = errors.New("分类下还有子分类")
	ErrCategoryCycle       = errors.New("不能把分类移动到它自己或它的子分类之下")
	ErrCategoryNameEmpty   = errors.New("分类名不能为空")
	ErrCategoryNameTooLong = errors.New("分类名过长")
	// ErrCategoryParentNotFound 与 ErrCategoryNotFound 分开，是为了让状态码说得准确：
	// 请求体里指了一个不存在的上级分类，是这次请求写错了（400），
	// 而路径上 /categories/:id 指向的分类不存在，才是「找不到」（404）。
	ErrCategoryParentNotFound = errors.New("指定的上级分类不存在")
)

// MaxCategoryNameRunes 是分类名长度上限。分类名会出现在树的每一层，
// 过长会挤掉右侧内容区，这里按界面的实际可用宽度定。
const MaxCategoryNameRunes = 40

// CategoryNode 是分类树的节点。children 恒为非 nil 切片，
// 前端可以无条件遍历而不用判空。
type CategoryNode struct {
	ID        int64  `json:"id"`
	ParentID  *int64 `json:"parentId"`
	Name      string `json:"name"`
	SortOrder int    `json:"sortOrder"`
	Depth     int    `json:"depth"`
	// DocumentCount 是本分类直属的文件数
	DocumentCount int64 `json:"documentCount"`
	// TotalCount 含所有子分类，用于在树上展示“这个分类下一共有多少文件”
	TotalCount int64           `json:"totalCount"`
	Children   []*CategoryNode `json:"children"`
}

// 分类树用递归 CTE 一次取出：深度由递归层数直接得到，
// 直属文件数用一次聚合左连接带出。
// 带子分类的累计数在 Go 里自底向上汇总，比再写一层递归聚合更好读。
const categoryTreeSelect = `
WITH RECURSIVE tree AS (
    SELECT id, parent_id, name, sort_order, 0 AS depth
    FROM categories
    WHERE parent_id IS NULL
    UNION ALL
    SELECT c.id, c.parent_id, c.name, c.sort_order, t.depth + 1
    FROM categories c
    JOIN tree t ON c.parent_id = t.id
)
SELECT t.id, t.parent_id, t.name, t.sort_order, t.depth,
       COALESCE(d.cnt, 0) AS document_count
FROM tree t
LEFT JOIN (
    SELECT category_id, count(*) AS cnt
    FROM documents
    GROUP BY category_id
) d ON d.category_id = t.id
ORDER BY t.depth, t.sort_order, t.id`

// categoryRow 严格对应 categoryTreeSelect 返回的列。
// 不能直接 Scan 进 CategoryNode：GORM 见到 Children 字段会把它当成关联关系，
// 试图推导外键并报 “define a valid foreign key for relations”。
type categoryRow struct {
	ID            int64  `gorm:"column:id"`
	ParentID      *int64 `gorm:"column:parent_id"`
	Name          string `gorm:"column:name"`
	SortOrder     int    `gorm:"column:sort_order"`
	Depth         int    `gorm:"column:depth"`
	DocumentCount int64  `gorm:"column:document_count"`
}

func (s *Store) ListCategoryTree(ctx context.Context) ([]*CategoryNode, error) {
	var rows []categoryRow
	if err := s.db.WithContext(ctx).Raw(categoryTreeSelect).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("查询分类树失败: %w", err)
	}

	// rows 已按 (深度, 排序, id) 排好，因此父节点必然先于子节点出现，
	// 一趟遍历即可挂载，不需要反复查找。
	byID := make(map[int64]*CategoryNode, len(rows))
	roots := make([]*CategoryNode, 0, len(rows))
	for _, row := range rows {
		node := &CategoryNode{
			ID:            row.ID,
			ParentID:      row.ParentID,
			Name:          row.Name,
			SortOrder:     row.SortOrder,
			Depth:         row.Depth,
			DocumentCount: row.DocumentCount,
			Children:      []*CategoryNode{},
		}
		byID[node.ID] = node
		if node.ParentID == nil {
			roots = append(roots, node)
			continue
		}
		parent, ok := byID[*node.ParentID]
		if !ok {
			// 父节点缺失在正常数据下不会发生（外键保证），
			// 真出现了就把它当根节点展示，总好过整个接口失败
			roots = append(roots, node)
			continue
		}
		parent.Children = append(parent.Children, node)
	}

	// 自底向上汇总：子节点的 total 已算好，父节点直接累加
	var rollup func(node *CategoryNode) int64
	rollup = func(node *CategoryNode) int64 {
		total := node.DocumentCount
		for _, child := range node.Children {
			total += rollup(child)
		}
		node.TotalCount = total
		return total
	}
	for _, root := range roots {
		rollup(root)
	}
	return roots, nil
}

// GetCategoryNode 取单个分类，用于写操作后回传最新状态。
func (s *Store) GetCategoryNode(ctx context.Context, id int64) (*CategoryNode, error) {
	tree, err := s.ListCategoryTree(ctx)
	if err != nil {
		return nil, err
	}
	var found *CategoryNode
	var walk func(nodes []*CategoryNode)
	walk = func(nodes []*CategoryNode) {
		for _, node := range nodes {
			if node.ID == id {
				found = node
				return
			}
			walk(node.Children)
			if found != nil {
				return
			}
		}
	}
	walk(tree)
	if found == nil {
		return nil, ErrCategoryNotFound
	}
	return found, nil
}

// validateCategoryName 统一分类名的校验，创建与重命名共用一套规则。
func validateCategoryName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", ErrCategoryNameEmpty
	}
	if len([]rune(name)) > MaxCategoryNameRunes {
		return "", ErrCategoryNameTooLong
	}
	return name, nil
}

// isUniqueViolation 判断是否为唯一索引冲突。
// categories 上的 (parent_id, name) 唯一索引是防止同级重名的最后一道防线，
// 应用层先查一次只是为了给出更友好的提示，真正的判定交给数据库 ——
// 否则两个并发请求会同时通过检查。
//
// 注意错误类型：驱动是 pgx（gorm.io/driver/postgres），不是 lib/pq。
// lib/pq 只用于 pq.StringArray 这个数组类型，断言 *pq.Error 永远匹配不上，
// 唯一冲突会被当成未知错误返回 500。
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func (s *Store) CreateCategory(ctx context.Context, parentID *int64, rawName string) (*CategoryNode, error) {
	name, err := validateCategoryName(rawName)
	if err != nil {
		return nil, err
	}
	if parentID != nil {
		if err := s.ensureParentExists(ctx, *parentID); err != nil {
			return nil, err
		}
	}

	var id int64
	err = s.db.WithContext(ctx).Raw(
		`INSERT INTO categories (parent_id, name) VALUES (?, ?) RETURNING id`,
		parentID, name,
	).Scan(&id).Error
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrCategoryNameTaken
		}
		return nil, fmt.Errorf("创建分类失败: %w", err)
	}
	return s.GetCategoryNode(ctx, id)
}

// CategoryUpdate 描述一次分类修改。
// 改名与调整层级可以在同一次请求里完成，所以两者一起放在事务里执行，
// 否则「改名成功、移动失败」会留下一个用户没要求过的中间状态。
type CategoryUpdate struct {
	// Name 为 nil 表示不改名
	Name *string
	// MoveTo 是新的上级分类，nil 表示移到顶层
	MoveTo *int64
	// MoveToSet 为 true 时才会应用 MoveTo。
	// 需要这个标志才能区分「不改层级」与「移到顶层」——两者的 MoveTo 都是 nil。
	MoveToSet bool
}

func (s *Store) UpdateCategory(ctx context.Context, id int64, upd CategoryUpdate) (*CategoryNode, error) {
	if err := s.ensureCategoryExists(ctx, id); err != nil {
		return nil, err
	}

	name := ""
	if upd.Name != nil {
		validated, err := validateCategoryName(*upd.Name)
		if err != nil {
			return nil, err
		}
		name = validated
	}
	if upd.MoveToSet && upd.MoveTo != nil {
		if err := s.ensureParentExists(ctx, *upd.MoveTo); err != nil {
			return nil, err
		}
		if err := s.assertNoCycle(ctx, id, *upd.MoveTo); err != nil {
			return nil, err
		}
	}

	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if upd.Name != nil {
			if err := tx.Exec(
				`UPDATE categories SET name = ?, updated_at = now() WHERE id = ?`, name, id,
			).Error; err != nil {
				return err
			}
		}
		if upd.MoveToSet {
			if err := tx.Exec(
				`UPDATE categories SET parent_id = ?, updated_at = now() WHERE id = ?`, upd.MoveTo, id,
			).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrCategoryNameTaken
		}
		return nil, fmt.Errorf("更新分类失败: %w", err)
	}
	return s.GetCategoryNode(ctx, id)
}

// assertNoCycle 检查把 id 挂到 newParentID 之下是否会成环。
// 从 newParentID 向上回溯祖先，若能走回 id，说明目标落在自己的子树里。
func (s *Store) assertNoCycle(ctx context.Context, id, newParentID int64) error {
	var cycle bool
	err := s.db.WithContext(ctx).Raw(`
WITH RECURSIVE ancestors AS (
    SELECT id, parent_id FROM categories WHERE id = ?
    UNION ALL
    SELECT c.id, c.parent_id
    FROM categories c
    JOIN ancestors a ON c.id = a.parent_id
)
SELECT EXISTS (SELECT 1 FROM ancestors WHERE id = ?)`,
		newParentID, id,
	).Scan(&cycle).Error
	if err != nil {
		return fmt.Errorf("检查分类层级失败: %w", err)
	}
	if cycle {
		return ErrCategoryCycle
	}
	return nil
}

// DeleteCategory 删除分类，返回被移出该分类（转为未分类）的文件数。
//
// 不删除任何文件：documents.category_id 是 ON DELETE SET NULL，
// 分类消失后文件落到「未分类」，字节与索引都不受影响。
// 有子分类时拒绝删除 —— 那会连带影响整棵子树，属于用户没有明确表达的操作。
func (s *Store) DeleteCategory(ctx context.Context, id int64) (int64, error) {
	if err := s.ensureCategoryExists(ctx, id); err != nil {
		return 0, err
	}

	var childCount int64
	if err := s.db.WithContext(ctx).Raw(
		`SELECT count(*) FROM categories WHERE parent_id = ?`, id,
	).Scan(&childCount).Error; err != nil {
		return 0, fmt.Errorf("统计子分类失败: %w", err)
	}
	if childCount > 0 {
		return 0, ErrCategoryHasChild
	}

	var docCount int64
	if err := s.db.WithContext(ctx).Raw(
		`SELECT count(*) FROM documents WHERE category_id = ?`, id,
	).Scan(&docCount).Error; err != nil {
		return 0, fmt.Errorf("统计分类下文件失败: %w", err)
	}

	if err := s.db.WithContext(ctx).Exec(
		`DELETE FROM categories WHERE id = ?`, id,
	).Error; err != nil {
		return 0, fmt.Errorf("删除分类失败: %w", err)
	}
	return docCount, nil
}

// CountDocumentsInCategory 统计分类及其所有子分类下的文件数，
// 供删除前的提示使用。
func (s *Store) CountDocumentsInCategory(ctx context.Context, id int64) (int64, error) {
	var count int64
	err := s.db.WithContext(ctx).Raw(`
WITH RECURSIVE sub AS (
    SELECT id FROM categories WHERE id = ?
    UNION ALL
    SELECT c.id FROM categories c JOIN sub s ON c.parent_id = s.id
)
SELECT count(*) FROM documents WHERE category_id IN (SELECT id FROM sub)`, id).Scan(&count).Error
	if err != nil {
		return 0, fmt.Errorf("统计分类下文件失败: %w", err)
	}
	return count, nil
}

func (s *Store) ensureCategoryExists(ctx context.Context, id int64) error {
	if ok, err := s.categoryExists(ctx, id); err != nil {
		return err
	} else if !ok {
		return ErrCategoryNotFound
	}
	return nil
}

// ensureParentExists 校验请求体里引用的上级分类。
// 与 ensureCategoryExists 的区别只在错误类型，见 ErrCategoryParentNotFound 的说明。
func (s *Store) ensureParentExists(ctx context.Context, id int64) error {
	if ok, err := s.categoryExists(ctx, id); err != nil {
		return err
	} else if !ok {
		return ErrCategoryParentNotFound
	}
	return nil
}

func (s *Store) categoryExists(ctx context.Context, id int64) (bool, error) {
	var count int64
	if err := s.db.WithContext(ctx).Raw(
		`SELECT count(*) FROM categories WHERE id = ?`, id,
	).Scan(&count).Error; err != nil {
		return false, fmt.Errorf("校验分类失败: %w", err)
	}
	return count > 0, nil
}
