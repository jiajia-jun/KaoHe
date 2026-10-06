package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
)

// DefaultMaxAttempts 是单个索引任务的总尝试次数（首次 + 重试）。
const DefaultMaxAttempts = 3

// ClaimedJob 是一个已被本进程独占的索引任务。
type ClaimedJob struct {
	JobID       int64
	DocumentID  int64
	Attempts    int
	MaxAttempts int
}

// ChunkInput 是要写入 document_chunks 的一个片段。
type ChunkInput struct {
	Ordinal   int
	Content   string
	CharCount int
	Embedding []float32
}

// ClaimIndexJob 领取一个待处理任务。
//
// FOR UPDATE SKIP LOCKED 是这套队列的核心：多个 worker 同时来取，
// 各自锁住不同的行，谁都不会阻塞在别人的锁上，同一任务也不会被两个人拿到。
// 用 PostgreSQL 自己当队列，就不必为了「文件已存但任务丢了」这种窗口去引入 Redis。
//
// 领取与状态流转放在同一条语句里：一旦这个 UPDATE 提交，任务就是 running，
// 中间没有「已锁定但还没标成 running」的窗口。
func (s *Store) ClaimIndexJob(ctx context.Context) (*ClaimedJob, error) {
	const query = `
WITH picked AS (
    SELECT id, document_id
    FROM index_jobs
    WHERE status = 'queued' AND run_after <= now()
    ORDER BY run_after, id
    FOR UPDATE SKIP LOCKED
    LIMIT 1
), claimed AS (
    UPDATE index_jobs j
    SET status = 'running', attempts = j.attempts + 1, updated_at = now()
    FROM picked p
    WHERE j.id = p.id
    RETURNING j.id, j.document_id, j.attempts, j.max_attempts
)
UPDATE documents d
SET index_status = 'processing', index_error = NULL, updated_at = now()
FROM claimed c
WHERE d.id = c.document_id
RETURNING c.id, c.document_id, c.attempts, c.max_attempts`

	var row struct {
		ID          int64
		DocumentID  int64
		Attempts    int
		MaxAttempts int
	}
	res := s.db.WithContext(ctx).Raw(query).Scan(&row)
	if res.Error != nil {
		return nil, fmt.Errorf("领取索引任务失败: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return nil, nil // 队列为空，不是错误
	}
	return &ClaimedJob{
		JobID:       row.ID,
		DocumentID:  row.DocumentID,
		Attempts:    row.Attempts,
		MaxAttempts: row.MaxAttempts,
	}, nil
}

// CompleteIndexJob 写入片段、把文档标成 ready、任务标成 done。
//
// 三件事必须在同一个事务里：如果片段写了一半而任务已标成 done，
// 这份文档就会永远停在「索引完成但只搜得到一半内容」的状态，且不会被重试。
func (s *Store) CompleteIndexJob(ctx context.Context, job *ClaimedJob, chunks []ChunkInput) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 先更新文档状态，并借这次更新确认文档还在。
		//
		// 文件可能在索引进行到一半时被「彻底删除」（回收站里清掉）：documents 行没了，
		// document_chunks 与 index_jobs 也随外键一起消失。这时后面的写入会撞外键，
		// 整个事务失败、任务被记成一次失败并重试 —— 而重试永远不可能成功，
		// 白白烧掉三次机会，日志里留下一串看不出原因的报错。
		//
		// 放在最前面是因为它同时解决了顺序问题：没有可写的对象就直接结束，
		// 此时还没有任何片段被删改，事务提交一个空变更即可。
		res := tx.Exec(`
UPDATE documents SET index_status = 'ready', index_error = NULL, updated_at = now()
WHERE id = ?`, job.DocumentID)
		if res.Error != nil {
			return fmt.Errorf("更新文档索引状态失败: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return nil
		}

		// 先清后写：重试时上一次可能已经写进去一部分片段，
		// 不清的话 (document_id, ordinal) 唯一约束会直接把这一次也顶掉。
		if err := tx.Exec(`DELETE FROM document_chunks WHERE document_id = ?`, job.DocumentID).Error; err != nil {
			return fmt.Errorf("清理旧片段失败: %w", err)
		}
		if err := insertChunks(tx, job.DocumentID, chunks); err != nil {
			return err
		}
		return tx.Exec(`
UPDATE index_jobs SET status = 'done', last_error = NULL, updated_at = now()
WHERE id = ?`, job.JobID).Error
	})
}

func insertChunks(tx *gorm.DB, documentID int64, chunks []ChunkInput) error {
	if len(chunks) == 0 {
		return nil
	}
	var (
		placeholders []string
		args         []any
	)
	for _, chunk := range chunks {
		placeholders = append(placeholders, "(?, ?, ?, ?, ?::vector)")
		args = append(args, documentID, chunk.Ordinal, chunk.Content, chunk.CharCount, vectorLiteral(chunk.Embedding))
	}
	// 一次多值 INSERT：按片段逐条发，一份文档就是几百次往返，
	// 而这里的数据量完全在一条语句能承受的范围内。
	sql := `INSERT INTO document_chunks (document_id, ordinal, content, char_count, embedding) VALUES ` +
		strings.Join(placeholders, ", ")
	if err := tx.Exec(sql, args...).Error; err != nil {
		return fmt.Errorf("写入片段失败: %w", err)
	}
	return nil
}

// vectorLiteral 把向量转成 pgvector 的字面量，形如 [0.1,-0.2,...]。
func vectorLiteral(v []float32) string {
	var b strings.Builder
	b.Grow(len(v) * 9)
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		// 用 -1 精度：float32 的最短往返表示，再多写小数位只是白占空间
		b.WriteString(strconv.FormatFloat(float64(f), 'f', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// FailIndexJob 记录一次失败，并按剩余次数决定是重新排队还是就此终止。
//
// 返回是否还会重试。退避按 attempts 指数增长：边车刚重启时是它最忙的时候，
// 立刻重放只会连着失败三次，把重试次数耗尽。
func (s *Store) FailIndexJob(ctx context.Context, job *ClaimedJob, reason string) (retrying bool, err error) {
	if len(reason) > 500 {
		reason = reason[:500]
	}
	retrying = job.Attempts < job.MaxAttempts

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if retrying {
			backoff := time.Duration(1<<uint(job.Attempts-1)) * 15 * time.Second
			if err := tx.Exec(`
UPDATE index_jobs SET status = 'queued', last_error = ?, run_after = now() + ?::interval, updated_at = now()
WHERE id = ?`, reason, backoff.String(), job.JobID).Error; err != nil {
				return err
			}
			// 文档退回 pending：界面上「处理中」的转圈要停下来。
			// 错误文案照旧留着，用户能看到上一次为什么失败。
			return tx.Exec(`
UPDATE documents SET index_status = 'pending', index_error = ?, updated_at = now()
WHERE id = ?`, reason, job.DocumentID).Error
		}
		if err := tx.Exec(`
UPDATE index_jobs SET status = 'failed', last_error = ?, updated_at = now()
WHERE id = ?`, reason, job.JobID).Error; err != nil {
			return err
		}
		return tx.Exec(`
UPDATE documents SET index_status = 'failed', index_error = ?, updated_at = now()
WHERE id = ?`, reason, job.DocumentID).Error
	})
	if err != nil {
		return false, fmt.Errorf("记录索引失败状态时出错: %w", err)
	}
	return retrying, nil
}

// MarkIndexPartial 在索引成功但有保留时，把提示写进 index_error。
// 状态仍是 ready —— 内容确实可检索，只是不完整，两者要分开表达。
func (s *Store) MarkIndexPartial(ctx context.Context, documentID int64, note string) error {
	return s.db.WithContext(ctx).Exec(`
UPDATE documents SET index_error = ?, updated_at = now() WHERE id = ?`, note, documentID).Error
}

// EnqueueIndexJob 为文档排一个索引任务，供「重新索引」使用。
// 返回本次是否真的新建了任务。
//
// 已经有排队或执行中的任务时不再新建：index_jobs 上的部分唯一索引也会拦，
// 但先查一次能让这个接口天然幂等 —— 用户连点两下不该产生两个任务。
func (s *Store) EnqueueIndexJob(ctx context.Context, documentID int64) (bool, error) {
	created := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var active int64
		if err := tx.Raw(`
SELECT count(*) FROM index_jobs WHERE document_id = ? AND status IN ('queued','running')`,
			documentID).Scan(&active).Error; err != nil {
			return fmt.Errorf("检查已有索引任务失败: %w", err)
		}
		if active > 0 {
			// 正在跑的任务不要动它的状态：把 processing 改回 pending 会让界面
			// 显示成「等待中」，而实际上它正在被处理
			return nil
		}
		if err := tx.Exec(`
INSERT INTO index_jobs (document_id, status, attempts, max_attempts, run_after, created_at, updated_at)
VALUES (?, 'queued', 0, ?, now(), now(), now())`, documentID, DefaultMaxAttempts).Error; err != nil {
			return fmt.Errorf("创建索引任务失败: %w", err)
		}
		// 新任务会重新算一遍，上一次的失败原因要清掉，
		// 否则状态已是 pending 却还挂着上一轮的报错
		if err := tx.Exec(`
UPDATE documents SET index_status = 'pending', index_error = NULL, updated_at = now()
WHERE id = ?`, documentID).Error; err != nil {
			return err
		}
		created = true
		return nil
	})
	return created, err
}

// RecoverStaleJobs 把停在 running 太久的任务放回队列。
//
// worker 被 kill -9、容器被重启时，它正在处理的任务会永远停在 running，
// 而部分唯一索引又让同一文档无法再建新任务 —— 那份文件就再也不会被索引了。
// worker 每次启动都会先跑一遍这里。
func (s *Store) RecoverStaleJobs(ctx context.Context, olderThan time.Duration) (int64, error) {
	const query = `
WITH stale AS (
    UPDATE index_jobs
    SET status = 'queued', run_after = now(), last_error = '任务被中断，已自动重新排队', updated_at = now()
    WHERE status = 'running' AND updated_at < now() - ?::interval
    RETURNING document_id
)
UPDATE documents d
SET index_status = 'pending', updated_at = now()
FROM stale s
WHERE d.id = s.document_id`

	res := s.db.WithContext(ctx).Exec(query, olderThan.String())
	if res.Error != nil {
		return 0, fmt.Errorf("恢复中断的索引任务失败: %w", res.Error)
	}
	return res.RowsAffected, nil
}
