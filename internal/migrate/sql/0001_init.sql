-- 0001_init.sql —— 扩展、表与索引的初始定义。
--
-- 全部语句幂等（IF NOT EXISTS），可安全重复执行。
-- 迁移文件一旦提交就不再修改；后续结构变更一律追加新文件。
--
-- 注意：这里不用 GORM AutoMigrate。向量列、HNSW 索引、部分唯一索引、
-- 表达式约束都是 AutoMigrate 表达不出来的，混用会导致 schema 漂移。

CREATE EXTENSION IF NOT EXISTS vector;    -- 向量类型与 HNSW 索引
CREATE EXTENSION IF NOT EXISTS pg_trgm;   -- 中文按字符切三元组，不依赖分词器


-- ---------------------------------------------------------------- 分类树
-- 自引用树。UI 按两层设计，但结构上不限深度。
CREATE TABLE IF NOT EXISTS categories (
    id         BIGSERIAL   PRIMARY KEY,
    parent_id  BIGINT      REFERENCES categories (id) ON DELETE RESTRICT,
    name       TEXT        NOT NULL,
    sort_order INTEGER     NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 同一父节点下不允许重名。NULLS NOT DISTINCT 让“顶层分类”也参与唯一性判断，
-- 否则 parent_id IS NULL 的行在唯一索引里互不冲突，顶层会出现重名。
CREATE UNIQUE INDEX IF NOT EXISTS categories_parent_name_key
    ON categories (parent_id, name) NULLS NOT DISTINCT;

CREATE INDEX IF NOT EXISTS categories_parent_idx ON categories (parent_id);


-- ---------------------------------------------------------------- 文档
CREATE TABLE IF NOT EXISTS documents (
    id             BIGSERIAL   PRIMARY KEY,
    doc_uid        TEXT        NOT NULL UNIQUE,   -- 对外标识 documentId
    name           TEXT        NOT NULL,          -- 用户可读的文件名
    storage_key    TEXT        NOT NULL UNIQUE,   -- 磁盘存储键，与显示名分离，防同名覆盖
    content_type   TEXT        NOT NULL,
    size_bytes     BIGINT      NOT NULL CHECK (size_bytes >= 0),
    category_id    BIGINT      REFERENCES categories (id) ON DELETE SET NULL,
    tags           TEXT[]      NOT NULL DEFAULT '{}',
    archived       BOOLEAN     NOT NULL DEFAULT FALSE,
    storage_status TEXT        NOT NULL DEFAULT 'stored',
    index_status   TEXT        NOT NULL DEFAULT 'pending',
    index_error    TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT documents_index_status_check
        CHECK (index_status IN ('pending','processing','ready','failed','not_supported'))
);

-- 文件名模糊检索：pg_trgm 让 ILIKE '%词%' 能走索引（中文无空格，tsvector 不可用）
CREATE INDEX IF NOT EXISTS documents_name_trgm_idx ON documents USING gin (name gin_trgm_ops);
CREATE INDEX IF NOT EXISTS documents_tags_idx      ON documents USING gin (tags);
-- 默认列表与归档区的区分，兼作时间倒序
CREATE INDEX IF NOT EXISTS documents_active_idx    ON documents (archived, created_at DESC);
CREATE INDEX IF NOT EXISTS documents_category_idx  ON documents (category_id);
-- 后台任务按状态筛选待处理文档
CREATE INDEX IF NOT EXISTS documents_index_status_idx ON documents (index_status);


-- ---------------------------------------------------------------- 文本块与向量
CREATE TABLE IF NOT EXISTS document_chunks (
    id          BIGSERIAL   PRIMARY KEY,
    document_id BIGINT      NOT NULL REFERENCES documents (id) ON DELETE CASCADE,
    ordinal     INTEGER     NOT NULL,
    content     TEXT        NOT NULL,
    embedding   VECTOR(512),           -- bge-small-zh-v1.5 输出维度，实测确认
    char_count  INTEGER     NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- 索引重试按 (document_id, ordinal) 覆盖写入：
    -- 同一任务重放不会产生重复片段，这正是故障复盘里要求的行为。
    CONSTRAINT document_chunks_document_ordinal_key UNIQUE (document_id, ordinal)
);

-- 语义检索：余弦距离 + HNSW 近似最近邻
CREATE INDEX IF NOT EXISTS document_chunks_embedding_idx
    ON document_chunks USING hnsw (embedding vector_cosine_ops);

-- 关键词正文检索：命中片段直接取自本表，与语义检索共用一份切分
CREATE INDEX IF NOT EXISTS document_chunks_content_trgm_idx
    ON document_chunks USING gin (content gin_trgm_ops);


-- ---------------------------------------------------------------- 索引任务队列
-- 用 PostgreSQL 表当队列，而不是引入 Redis：
-- 文件落盘、documents 行、index_jobs 行可以在同一个事务里写入，
-- 不存在“文件已存但任务丢失”的窗口。
CREATE TABLE IF NOT EXISTS index_jobs (
    id           BIGSERIAL   PRIMARY KEY,
    document_id  BIGINT      NOT NULL REFERENCES documents (id) ON DELETE CASCADE,
    status       TEXT        NOT NULL DEFAULT 'queued',
    attempts     INTEGER     NOT NULL DEFAULT 0,
    max_attempts INTEGER     NOT NULL DEFAULT 3,
    last_error   TEXT,
    run_after    TIMESTAMPTZ NOT NULL DEFAULT now(),  -- 退避重试的下次可执行时间
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT index_jobs_status_check
        CHECK (status IN ('queued','running','done','failed'))
);

-- 同一文档同时只允许有一个未完成任务，重试接口重复调用不会堆积任务
CREATE UNIQUE INDEX IF NOT EXISTS index_jobs_active_key
    ON index_jobs (document_id) WHERE status IN ('queued','running');

-- worker 领取任务时按 (status, run_after) 过滤
CREATE INDEX IF NOT EXISTS index_jobs_pick_idx ON index_jobs (status, run_after);

-- 失败原因统计与告警用
CREATE INDEX IF NOT EXISTS index_jobs_status_idx ON index_jobs (status);
