-- 0002_category_no_self_parent.sql —— 禁止分类把自己设为自己的上级。
--
-- 应用层在移动分类时已经会拦截成环（包括移动到自身），这里再加一道库级约束：
-- 分类树是用递归 CTE 查出来的，一旦数据里出现自环，
-- 递归查询不会报错也不会结束，而是把数据库连接一直占住。
-- 这种故障从接口上看只是「请求一直不返回」，排查成本远高于约束本身的成本。
--
-- 深层成环（A→B→A）仍由应用层的祖先回溯拦截，SQL 层面无法用简单 CHECK 表达。

ALTER TABLE categories
    DROP CONSTRAINT IF EXISTS categories_no_self_parent;

ALTER TABLE categories
    ADD CONSTRAINT categories_no_self_parent
    CHECK (parent_id IS NULL OR parent_id <> id);
