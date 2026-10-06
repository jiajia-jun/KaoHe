package indexer

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"time"

	"go.uber.org/zap"

	"KaoHe/internal/logging"

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
	log         *zap.Logger
	slowTask    time.Duration
	lastRecover time.Time
}

// Deps 是 Worker 的依赖。
//
// 用结构体而不是位置参数：构造函数已经到第五个参数了，再加一个字段时
// 位置参数只会编译器不报错地错位。NewRouter 同理。
type Deps struct {
	Store *store.Store
	Files *storage.Store
	Embed *Embedder
	Log   *zap.Logger
	// SlowTask 是「多久算慢」的门槛。0 表示不标记。
	SlowTask time.Duration
}

// NewWorker 组装一个消费者，但不做任何 IO；连接与探活发生在 Run 里。
func NewWorker(d Deps) *Worker {
	if d.Log == nil {
		// 缺日志器不该让 worker 直接 panic 在第一条日志上。
		d.Log = zap.NewNop()
	}
	return &Worker{
		store:    d.Store,
		files:    d.Files,
		embed:    d.Embed,
		log:      d.Log,
		slowTask: d.SlowTask,
	}
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
	w.log.Info("worker 已启动",
		zap.Int("embedDimension", dim),
		zap.Duration("poll", pollInterval),
	)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.log.Info("收到退出信号，worker 停止")
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
			w.log.Error("领取索引任务失败", zap.Error(err))
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
		w.log.Error("恢复中断任务失败", zap.Error(err))
		return
	}
	if n > 0 {
		w.log.Warn("发现被中断的索引任务，已重新排队", zap.Int64("count", n))
	}
}

// staleJobThreshold 与 store 侧的判定保持一致：超过它仍停在 running 的任务算中断。
// 定义在这里而不是直接用 store 的常量，是为了让「多久算卡住」这件事在这一层可见 ——
// 它是 worker 的运维参数，不是数据访问的细节。
const staleJobThreshold = 10 * time.Minute

// indexResult 是 index 的产出，供 process 打一条说实话的完成日志。
//
// 「完成」这件事有三种不同的结局，混成一句会让日志无法解释：
// 真的写了内容、文档在途中被彻底删除所以没写、以及压根没做索引。
type indexResult struct {
	// Chunks 是本次切出的片段数。
	Chunks int
	// Truncated 表示正文过长，只索引了前 maxChunksPerDocument 片。
	Truncated bool
	// Written 为 false 表示文档在抽取与向量都跑完之后被彻底删除，
	// CompleteIndexJob 的守卫让这次写入变成了空操作。
	Written bool
	// Skipped 非空表示这次根本没做索引，值是原因。
	Skipped string
}

// process 处理一个任务：读原文件 → 抽正文 → 切片 → 生成向量 → 落库。
//
// 任何一步失败都只是这个任务的失败，不会影响别的任务，
// 更不会影响原文件本身 —— 字节早在上传时就写进持久化目录了。
func (w *Worker) process(ctx context.Context, job *store.ClaimedJob) {
	started := time.Now()

	// 一个任务派生一个子 logger：这四个字段描述的是「这个任务是谁」，
	// 而不是「这一条日志说了什么」，所以在入口绑一次，
	// 让 index 内部那几条也自动带上，不必各自重抄一遍。
	log := w.log.With(
		zap.Int64("jobId", job.JobID),
		zap.Int64("documentId", job.DocumentID),
		zap.Int("attempt", job.Attempts),
	)

	res, err := w.index(ctx, job, log)
	cost := time.Since(started)

	if err == nil {
		switch {
		case res.Skipped != "":
			// 跳过的原因必须写出来，否则「没有索引完成日志」和「任务丢了」分不清。
			log.Info("索引跳过", zap.String("reason", res.Skipped), logging.Cost(cost))
		case !res.Written:
			// 抽取与向量都跑完了，真正要写库时才发现文档已经不在。
			// 既不是失败，也不该说成「完成」—— 一个字都没写进去。
			log.Info("文档已被彻底删除，索引未写入",
				zap.Int("chunks", res.Chunks), logging.Cost(cost))
		default:
			fields := []zap.Field{
				zap.Int("chunks", res.Chunks),
				zap.Bool("truncated", res.Truncated),
				logging.Cost(cost),
			}
			// 消息文本与 Info 那次一样，只降级到 Warn 并加 slow 标记 ——
			// 换个说法的代价是「grep 索引完成」再也捞不全这些行。
			if w.slowTask > 0 && cost >= w.slowTask {
				log.Warn("索引完成", append(fields, zap.Bool("slow", true))...)
				return
			}
			log.Info("索引完成", fields...)
		}
		return
	}

	// ctx 被取消（容器收到 SIGTERM）不是任务本身的问题，
	// 此时不要去改数据库状态：让任务留在 running，由下次启动时的恢复逻辑放回队列。
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		log.Warn("索引被中断，任务保持 running 等待恢复")
		return
	}

	retrying, ferr := w.store.FailIndexJob(ctx, job, err.Error())
	if ferr != nil {
		log.Error("记录索引失败时又出错了", zap.Error(ferr))
		return
	}
	if retrying {
		log.Warn("索引失败，稍后重试",
			zap.Int("max", job.MaxAttempts), zap.Error(err))
	} else {
		log.Error("索引失败，重试次数已用尽", zap.Error(err))
	}
}

// index 做一次完整的索引。log 由 process 传入，已经带好了 jobId / documentId / attempt，
// 这样这一层里的几条日志不必自己再拼一遍这三个字段。
func (w *Worker) index(ctx context.Context, job *store.ClaimedJob, log *zap.Logger) (indexResult, error) {
	doc, err := w.store.GetDocumentByID(ctx, job.DocumentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// 文档在排队期间被删除，是正常情况，不值得重试
			return indexResult{Skipped: "文档在排队期间已被删除"}, nil
		}
		return indexResult{}, err
	}

	// 格式按存储键判断，不按用户可见的文件名：
	// 文件可以被重命名成别的后缀，而磁盘上的存储键自上传起就不再变化。
	ext := filepath.Ext(doc.StorageKey)
	if !filekind.IsExtractable(ext) {
		// 不可抽取正文的格式（PDF）本就不该有任务。真出现了就把它标记清楚并结束，
		// 而不是反复重试一个永远不会成功的事。
		log.Warn("该格式不支持正文索引，跳过", zap.String("ext", ext))
		return indexResult{Skipped: "格式 " + ext + " 不支持正文索引"}, nil
	}

	file, _, err := w.files.Open(doc.StorageKey)
	if err != nil {
		return indexResult{}, err
	}
	defer file.Close()

	text, err := ExtractText(file)
	if err != nil {
		return indexResult{}, err
	}
	chunks, truncated := Chunk(text)
	if len(chunks) == 0 {
		return indexResult{}, errors.New("文件中没有可索引的正文")
	}

	vectors, err := w.embed.EmbedAll(ctx, chunks)
	if err != nil {
		return indexResult{}, err
	}
	if len(vectors) != len(chunks) {
		return indexResult{}, errors.New("向量条数与片段数不一致")
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

	written, err := w.store.CompleteIndexJob(ctx, job, inputs)
	if err != nil {
		return indexResult{}, err
	}

	res := indexResult{Chunks: len(chunks), Truncated: truncated, Written: written}

	// 文档已经没了就别再往它身上写提示，那一次更新同样会落在空处。
	if truncated && written {
		// 索引成功但不完整。状态保持 ready，把这件事写进 index_error 让界面能提示，
		// 而不是让用户以为整份文件都搜得到。
		note := "正文过长，只索引了前 " + strconv.Itoa(maxChunksPerDocument) + " 个片段"
		if err := w.store.MarkIndexPartial(ctx, doc.ID, note); err != nil {
			log.Warn("写入索引截断提示失败", zap.Error(err))
		}
		log.Warn("正文过长，索引被截断", zap.Int("chunks", len(chunks)))
	}
	return res, nil
}
