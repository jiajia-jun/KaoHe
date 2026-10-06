// Package store 定义数据模型与查询。
package store

import (
	"time"

	"github.com/lib/pq"
)

// 索引状态取值。与 documents 表上的 CHECK 约束保持一致。
const (
	// IndexPending 已入队，等待 worker 领取
	IndexPending = "pending"
	// IndexProcessing worker 正在处理
	IndexProcessing = "processing"
	// IndexReady 切片与向量均已写入，可被正文与语义检索命中
	IndexReady = "ready"
	// IndexFailed 重试次数耗尽，等待人工触发重试
	IndexFailed = "failed"
	// IndexNotSupported 该格式本设计不抽取正文（PDF），不算失败、不重试
	IndexNotSupported = "not_supported"
)

// 任务状态取值。与 index_jobs 表上的 CHECK 约束保持一致。
const (
	JobQueued  = "queued"
	JobRunning = "running"
	JobDone    = "done"
	JobFailed  = "failed"
)

// Category 是分类树上的一个节点。父子关系只靠 ParentID，
// 没有物化路径列：树最多两层，递归 CTE 足够快，多维护一列反而多一处会写错的地方。
type Category struct {
	ID        int64     `gorm:"primaryKey" json:"-"`
	ParentID  *int64    `gorm:"column:parent_id" json:"parentId"`
	Name      string    `gorm:"column:name" json:"name"`
	SortOrder int       `gorm:"column:sort_order" json:"sortOrder"`
	CreatedAt time.Time `gorm:"column:created_at" json:"createdAt"`
	UpdatedAt time.Time `gorm:"column:updated_at" json:"updatedAt"`
}

// Document 是一份上传的文件。
//
// 注意「文件在不在」与「索引到哪一步了」是两个独立字段：
// 归档只是把 Archived 置位、字节不动，索引失败也不会动原文件。
type Document struct {
	ID int64 `gorm:"primaryKey" json:"-"`
	// DocUID 是对外标识，接口路径里用它，不暴露自增主键
	DocUID string `gorm:"column:doc_uid" json:"id"`
	// Name 是用户可读的文件名，可与磁盘上的存储键完全不同
	Name string `gorm:"column:name" json:"name"`
	// StorageKey 是磁盘上的相对路径，与显示名分离，避免同名文件互相覆盖
	StorageKey  string         `gorm:"column:storage_key" json:"-"`
	ContentType string         `gorm:"column:content_type" json:"contentType"`
	SizeBytes   int64          `gorm:"column:size_bytes" json:"sizeBytes"`
	CategoryID  *int64         `gorm:"column:category_id" json:"categoryId"`
	Tags        pq.StringArray `gorm:"column:tags;type:text[]" json:"tags"`

	// Archived 是软状态：文件字节永远不动，只是默认不出现在列表与检索里
	Archived      bool    `gorm:"column:archived" json:"archived"`
	StorageStatus string  `gorm:"column:storage_status" json:"storageStatus"`
	IndexStatus   string  `gorm:"column:index_status" json:"indexStatus"`
	IndexError    *string `gorm:"column:index_error" json:"indexError"`

	CreatedAt time.Time `gorm:"column:created_at" json:"createdAt"`
	UpdatedAt time.Time `gorm:"column:updated_at" json:"updatedAt"`

	// 由列表查询 JOIN 带出，不落库
	CategoryName *string `gorm:"->;column:category_name" json:"categoryName"`
}

// TableName 固定表名，不让 GORM 按结构体名推断。
func (Document) TableName() string { return "documents" }

// DocumentChunk 是文档切出的一段正文及其向量，关键词检索与语义检索共用这一份。
// 重建索引时整篇的片段会被一起替换，不做增量合并。
type DocumentChunk struct {
	ID         int64   `gorm:"primaryKey"`
	DocumentID int64   `gorm:"column:document_id"`
	Ordinal    int     `gorm:"column:ordinal"`
	Content    string  `gorm:"column:content"`
	CharCount  int     `gorm:"column:char_count"`
	Embedding  *string `gorm:"column:embedding"` // 由原生 SQL 读写，GORM 不直接映射 vector
}

// TableName 固定表名，不让 GORM 按结构体名推断。
func (DocumentChunk) TableName() string { return "document_chunks" }

// IndexJob 是索引任务队列里的一行。
// 队列就是这个表本身，没有额外的消息中间件；worker 用 FOR UPDATE SKIP LOCKED 领取。
// 同一文档同时只允许一条未完成的任务，由部分唯一索引在数据库层保证。
type IndexJob struct {
	ID          int64     `gorm:"primaryKey"`
	DocumentID  int64     `gorm:"column:document_id"`
	Status      string    `gorm:"column:status"`
	Attempts    int       `gorm:"column:attempts"`
	MaxAttempts int       `gorm:"column:max_attempts"`
	LastError   *string   `gorm:"column:last_error"`
	RunAfter    time.Time `gorm:"column:run_after"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
}

// TableName 固定表名，不让 GORM 按结构体名推断。
func (IndexJob) TableName() string { return "index_jobs" }
