package indexer

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strconv"
	"time"

	"KaoHe/internal/filekind"
	"KaoHe/internal/storage"
	"KaoHe/internal/store"
)

const (
	// pollInterval 是队列空的时候的检查间隔。
	// 上传后到界面上状态变化，最坏要多等这么久，取一个用户察觉不到又不会空转的值。
	pollInterval = 700 * time.Millisecond
	// recoverInterval 控制「捞回被中断的任务」的频率。它每次都要扫一遍 running 的任务，
	// 没必要每轮都做；而如果 worker 还活着但某次落库失败，也得有机会自愈。
	recoverInterval = 30 * time.Second
)

// Worker 是索引任务的消费者，是唯一会改动 document_chunks 的角色。
// api 只负责建任务，不碰向量，两边职责不重叠。
type Worker struct {
	store       *store.Store
	files       *storage.Store
	embed       *Embedder
	lastRecover time.Time
}

// NewWorker 组装一个消费者，但不做任何 IO；连接与探活发生在 Run 里。
func NewWorker(st *store.Store, files *storage.Store, embed *Embedder) *Worker {
	return &Worker{store: st, files: files, embed: embed}
}

// Run 是 worker 的主循环，直到 ctx 被取消才返回。
func (w *Worker) Run(ctx context.Context) error {
	// 启动时先确认边车可用。模型没装好、边车没起来，应该在这里就喊出来，
	// 而不是把每一个任务都跑成失败再去猜原因。
	dim, err := w.embed.Health(ctx)
	if err != nil {
		return err
	}
	w.embed.setDimension(dim)
	slog.Info("worker 已启动", "embedDimension", dim, "poll", pollInterval.String())

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("收到退出信号，worker 停止")
			return nil
		case <-ticker.C:
		}

		w.maybeRecoverStale(ctx)
		if err := w.drain(ctx); err != nil {
			return err
		}
	}
}

// drain 把当前能领到的任务全部处理完，队列空了就返回。
// 一次 tick 处理多个任务，积压时不必每个都等一个 pollInterval。
func (w *Worker) drain(ctx context.Context) error {
	for {
		// 这里返回 nil 是刻意的：ctx 被取消意味着容器在正常退出，
		// 不是这一轮出了故障，报成错误会让 Run 把一次正常的 SIGTERM
		// 记成一条失败日志。返回 nil 之后外层 select 会立刻命中 ctx.Done() 并收尾。
		if ctx.Err() != nil {
			return nil //nolint:nilerr // 见上：取消不是失败
		}
		job, err := w.store.ClaimIndexJob(ctx)
		if err != nil {
			// 数据库暂时不可用不该让 worker 退出：下一轮还会再来。
			slog.Error("领取索引任务失败", "err", err)
			return nil
		}
		if job == nil {
			return nil
		}
		w.process(ctx, job)
	}
}

func (w *Worker) maybeRecoverStale(ctx context.Context) {
	if time.Since(w.lastRecover) < recoverInterval {
		return
	}
	w.lastRecover = time.Now()

	n, err := w.store.RecoverStaleJobs(ctx, staleJobThreshold)
	if err != nil {
		slog.Error("恢复中断任务失败", "err", err)
		return
	}
	if n > 0 {
		slog.Warn("发现被中断的索引任务，已重新排队", "count", n)
	}
}

// staleJobThreshold 与 store 侧的判定保持一致：超过它仍停在 running 的任务算中断。
// 定义在这里而不是直接用 store 的常量，是为了让「多久算卡住」这件事在这一层可见 ——
// 它是 worker 的运维参数，不是数据访问的细节。
const staleJobThreshold = 10 * time.Minute

// process 处理一个任务：读原文件 → 抽正文 → 切片 → 生成向量 → 落库。
//
// 任何一步失败都只是这个任务的失败，不会影响别的任务，
// 更不会影响原文件本身 —— 字节早在上传时就写进持久化目录了。
func (w *Worker) process(ctx context.Context, job *store.ClaimedJob) {
	started := time.Now()
	err := w.index(ctx, job)
	if err == nil {
		slog.Info("索引完成", "documentId", job.DocumentID, "attempt", job.Attempts,
			"cost", time.Since(started).Round(time.Millisecond).String())
		return
	}

	// ctx 被取消（容器收到 SIGTERM）不是任务本身的问题，
	// 此时不要去改数据库状态：让任务留在 running，由下次启动时的恢复逻辑放回队列。
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		slog.Warn("索引被中断，任务保持 running 等待恢复", "documentId", job.DocumentID)
		return
	}

	retrying, ferr := w.store.FailIndexJob(ctx, job, err.Error())
	if ferr != nil {
		slog.Error("记录索引失败时又出错了", "documentId", job.DocumentID, "err", ferr)
		return
	}
	if retrying {
		slog.Warn("索引失败，稍后重试", "documentId", job.DocumentID,
			"attempt", job.Attempts, "max", job.MaxAttempts, "err", err)
	} else {
		slog.Error("索引失败，重试次数已用尽", "documentId", job.DocumentID, "err", err)
	}
}

func (w *Worker) index(ctx context.Context, job *store.ClaimedJob) error {
	doc, err := w.store.GetDocumentByID(ctx, job.DocumentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// 文档在排队期间被删除，是正常情况，不值得重试
			return nil
		}
		return err
	}

	// 格式按存储键判断，不按用户可见的文件名：
	// 文件可以被重命名成别的后缀，而磁盘上的存储键自上传起就不再变化。
	ext := filepath.Ext(doc.StorageKey)
	if !filekind.IsExtractable(ext) {
		// 不可抽取正文的格式（PDF）本就不该有任务。真出现了就把它标记清楚并结束，
		// 而不是反复重试一个永远不会成功的事。
		slog.Warn("该格式不支持正文索引，跳过", "documentId", doc.ID, "ext", ext)
		return nil
	}

	file, _, err := w.files.Open(doc.StorageKey)
	if err != nil {
		return err
	}
	defer file.Close()

	text, err := ExtractText(file)
	if err != nil {
		return err
	}
	chunks, truncated := Chunk(text)
	if len(chunks) == 0 {
		return errors.New("文件中没有可索引的正文")
	}

	vectors, err := w.embed.EmbedAll(ctx, chunks)
	if err != nil {
		return err
	}
	if len(vectors) != len(chunks) {
		return errors.New("向量条数与片段数不一致")
	}

	inputs := make([]store.ChunkInput, len(chunks))
	for i, content := range chunks {
		inputs[i] = store.ChunkInput{
			Ordinal:   i,
			Content:   content,
			CharCount: len([]rune(content)),
			Embedding: vectors[i],
		}
	}
	if err := w.store.CompleteIndexJob(ctx, job, inputs); err != nil {
		return err
	}

	if truncated {
		// 索引成功但不完整。状态保持 ready，把这件事写进 index_error 让界面能提示，
		// 而不是让用户以为整份文件都搜得到。
		note := "正文过长，只索引了前 " + strconv.Itoa(maxChunksPerDocument) + " 个片段"
		if err := w.store.MarkIndexPartial(ctx, doc.ID, note); err != nil {
			slog.Warn("写入索引截断提示失败", "documentId", doc.ID, "err", err)
		}
		slog.Warn("正文过长，索引被截断", "documentId", doc.ID, "chunks", len(chunks))
	}
	return nil
}
