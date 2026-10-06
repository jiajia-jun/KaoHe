package indexer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// embedBatch 是每次请求边车的片段数。
// 不是越大越好：一批里最长的片段决定整批的填充长度，长短混在一起会白算很多 padding。
const embedBatch = 16

// Embedder 是向量边车的客户端。
//
// api 与 worker 共用一个 Embedder 实例：worker 用它给片段建索引，
// api 的语义检索接口用它把用户的提问转成查询向量。
type Embedder struct {
	baseURL string
	client  *http.Client
	// dimension 由边车自报，用于校验返回值与数据库里 vector(512) 的定义是否一致。
	//
	// 用原子量而不是普通 int：api 的多个请求处理器会并发调用 Embed
	// （每个语义检索请求都要转一次查询向量），而这个字段是在首次响应时才写入的。
	// 普通 int 在这里就是一次数据竞争 —— 平时看不出症状，跑 -race 立刻报。
	dimension atomic.Int64
}

// Dimension 返回边车自报的向量维度；还没和边车成功通信过时返回 0。
func (e *Embedder) Dimension() int { return int(e.dimension.Load()) }

// setDimension 记住边车自报的维度。首次写入之后，后续响应的维度都要与它一致。
func (e *Embedder) setDimension(d int) { e.dimension.Store(int64(d)) }

// NewEmbedder 构造边车客户端。维度初始未知：worker 会在 Run 开始时用
// Health 把它问出来，api 则等到第一次语义检索时从响应里学到。
// 未知期间不做维度校验，拿到之后才开始校验。
func NewEmbedder(baseURL string) *Embedder {
	return &Embedder{
		baseURL: baseURL,
		// 单批 16 个片段在 CPU 上通常几十毫秒，但容器刚起来、模型还在换页时可能慢得多。
		// 给足超时，让「慢」和「挂」区分开：慢的等它算完，挂的靠超时报错触发重试。
		client: &http.Client{Timeout: 60 * time.Second},
	}
}

type embedRequest struct {
	Texts []string `json:"texts"`
}

type embedResponse struct {
	Dimension int         `json:"dimension"`
	Count     int         `json:"count"`
	Vectors   [][]float32 `json:"vectors"`
}

// Health 探一次边车，返回它自报的维度。
// worker 启动时调用：模型没装好就该在这里失败，而不是等第一个任务进来才发现。
func (e *Embedder) Health(ctx context.Context) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.baseURL+"/healthz", nil)
	if err != nil {
		return 0, err
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("连接向量服务失败: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("向量服务健康检查返回 %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	var payload struct {
		Dimension int `json:"dimension"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, fmt.Errorf("解析向量服务健康检查响应失败: %w", err)
	}
	return payload.Dimension, nil
}

// Embed 生成一批文本的向量。调用方负责分批。
//
// 任何非 200 的响应都当作可重试的错误：边车重启、模型还在加载都属这一类，
// 而真正的输入问题（文本为空）在调用方就已经被排除掉了。
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	payload, err := json.Marshal(embedRequest{Texts: texts})
	if err != nil {
		return nil, fmt.Errorf("序列化向量请求失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/embed", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("调用向量服务失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("读取向量服务响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var apiErr struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &apiErr)
		if apiErr.Message == "" {
			apiErr.Message = string(bytes.TrimSpace(body))
		}
		return nil, fmt.Errorf("向量服务返回 %d: %s", resp.StatusCode, apiErr.Message)
	}

	var parsed embedResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("解析向量服务响应失败: %w", err)
	}
	if len(parsed.Vectors) != len(texts) {
		return nil, fmt.Errorf("向量条数不符：请求 %d 条，返回 %d 条", len(texts), len(parsed.Vectors))
	}
	// 维度对不上就必须失败：写进 vector(512) 的列会被数据库拒绝，
	// 在那里报错远不如在这里说清楚是模型换了。
	if known := e.Dimension(); known != 0 && parsed.Dimension != known {
		return nil, fmt.Errorf("向量维度不符：期望 %d，实际 %d", known, parsed.Dimension)
	}
	for i, v := range parsed.Vectors {
		if len(v) != parsed.Dimension {
			return nil, fmt.Errorf("第 %d 条向量维度为 %d，与声明的 %d 不符", i+1, len(v), parsed.Dimension)
		}
	}
	e.setDimension(parsed.Dimension)
	return parsed.Vectors, nil
}

// EmbedAll 按 embedBatch 分批把所有片段跑完。
func (e *Embedder) EmbedAll(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += embedBatch {
		end := min(start+embedBatch, len(texts))
		vectors, err := e.Embed(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, vectors...)
	}
	return out, nil
}
