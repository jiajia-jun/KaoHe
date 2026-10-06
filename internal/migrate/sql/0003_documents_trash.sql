-- 0003_documents_trash.sql —— 回收站：为「删除」加一个独立的标记位。
--
-- 为什么不复用 archived：
-- 归档是「这份资料暂时不参与日常工作」，删除是「我不要它了」。两者可以同时成立
-- （一份早就归档的文件后来也觉得没必要留着），也可能先后发生。用一个布尔位表达
-- 两种含义，恢复的时候就得猜用户到底想回到哪一边 —— 从「已归档」恢复和从
-- 「使用中」恢复是两个不同的目的地。
--
-- 为什么用时间戳而不是布尔：
-- 「什么时候删的」是用户会问的问题，布尔值回答不了；而 deleted_at IS NOT NULL
-- 照样能当布尔用，不需要为此再多一列。回收站里按时间排序也顺手有了依据。
--
-- 软删除不触碰磁盘上的字节，也不动索引状态：恢复之后立刻就能被搜到，
-- 不必重跑一遍索引。

ALTER TABLE documents
    ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;

-- 默认列表、关键词检索、语义检索现在都多一条 deleted_at IS NULL 的过滤，
-- 原来的 documents_active_idx 不再与查询完全对齐（少了这一列，排序之后
-- 还要回表判断）。换成与查询形状一致的偏索引，避免多养一个用不上的索引。
DROP INDEX IF EXISTS documents_active_idx;

CREATE INDEX IF NOT EXISTS documents_live_idx
    ON documents (archived, created_at DESC) WHERE deleted_at IS NULL;

-- 回收站：按进站时间倒序，这是回收站唯一的一种排序
CREATE INDEX IF NOT EXISTS documents_trashed_idx
    ON documents (created_at DESC) WHERE deleted_at IS NOT NULL;
