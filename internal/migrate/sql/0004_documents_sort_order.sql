-- 0004_documents_sort_order.sql —— 列表顺序交给用户。
--
-- 在此之前列表固定按 created_at DESC 排，用户想按自己的工作流摆一摆做不到。
-- 这里加一列 sort_order 存全局顺序：整张表只有一条序列，筛选出来的列表
-- （某个分类、归档区、回收站）都是它的子序列。这样「把文件甲放到文件乙之后」
-- 在任何筛选视图里都成立，不需要为每个视图各维护一套顺序。
--
-- 为什么是稠密整数 + 拖动时整表重排，而不是留间隔、按中点插入：
-- 中点方案在反复插到同一处（比如一直往最前面拖）时会耗尽间隔，需要一条
-- 「间隔不够了就重排一遍」的分支 —— 那条分支平时跑不到，真跑起来恰恰是最需要
-- 正确的时候。稠密重排每次走的都是同一条路径，代价是 O(n) 行写入，
-- 在内网单机、文档数以千计的量级上可以忽略。
--
-- 为什么不加 UNIQUE：
-- 两次并发上传会各自读到同一个 MIN(sort_order) 而写入同一个值；重排的整表
-- UPDATE 也是逐行校验的，唯一约束会让拖动偶发失败。并列的两行由
-- (sort_order, id DESC) 决定先后 —— 与旧列表的 id DESC 完全一致，
-- 所以「并列」这种无害情形不该让请求直接失败。
--
-- 回填保持现有可见顺序不变：窗口函数的 ORDER BY 与旧列表逐字一致
-- （created_at DESC, id DESC），升级前后用户看到的顺序一模一样。

ALTER TABLE documents
    ADD COLUMN IF NOT EXISTS sort_order BIGINT;

UPDATE documents d
SET sort_order = t.rn
FROM (
    SELECT id, row_number() OVER (ORDER BY created_at DESC, id DESC) AS rn
    FROM documents
) t
WHERE d.id = t.id AND d.sort_order IS NULL;

-- 回填完再收紧约束：先可空、后补值、最后 NOT NULL，比一开始就
-- NOT NULL DEFAULT 0 更干净地幂等 —— 重跑时 WHERE sort_order IS NULL
-- 匹配不到任何行，不会覆盖用户已经手工排好的顺序。
ALTER TABLE documents ALTER COLUMN sort_order SET DEFAULT 0;
ALTER TABLE documents ALTER COLUMN sort_order SET NOT NULL;

-- 列表与归档区的取数形状变成 ORDER BY sort_order, id，
-- 原来的索引少了排序列，排序之后还要回表。换成与查询完全对齐的偏索引
-- （与 0003 换掉 documents_active_idx 是同一个理由）。
DROP INDEX IF EXISTS documents_live_idx;

CREATE INDEX IF NOT EXISTS documents_live_idx
    ON documents (archived, sort_order, id) WHERE deleted_at IS NULL;

-- 回收站不再单独按上传时间排：位置是与归档/删除并列的第三条正交状态，
-- 文件进回收站不改位置，恢复出来也回到原处，所以回收站同样是这条全局序列的
-- 子序列。0003 里那句「按进站时间倒序」的注释随这条索引一并作废。
DROP INDEX IF EXISTS documents_trashed_idx;

CREATE INDEX IF NOT EXISTS documents_trashed_idx
    ON documents (sort_order, id) WHERE deleted_at IS NOT NULL;
