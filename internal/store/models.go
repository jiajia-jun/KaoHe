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

type Category struct {
	ID        int64     `gorm:"primaryKey" json:"-"`
	ParentID  *int64    `gorm:"column:parent_id" json:"parentId"`
	Name      string    `gorm:"column:name" json:"name"`
	SortOrder int       `gorm:"column:sort_order" json:"sortOrder"`
	CreatedAt time.Time `gorm:"column:created_at" json:"createdAt"`
	UpdatedAt time.Time `gorm:"column:updated_at" json:"updatedAt"`
}

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

func (Document) TableName() string { return "documents" }

type DocumentChunk struct {
	ID         int64   `gorm:"primaryKey"`
	DocumentID int64   `gorm:"column:document_id"`
	Ordinal    int     `gorm:"column:ordinal"`
	Content    string  `gorm:"column:content"`
	CharCount  int     `gorm:"column:char_count"`
	Embedding  *string `gorm:"column:embedding"` // 由原生 SQL 读写，GORM 不直接映射 vector
}

func (DocumentChunk) TableName() string { return "document_chunks" }

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

func (IndexJob) TableName() string { return "index_jobs" }
