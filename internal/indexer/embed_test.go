package indexer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeSidecar 起一个假边车，按请求里的条数回等长向量。
//
// dimension 与 status 可改，用来构造「模型换了维度」与「边车报错」两种情况。
func fakeSidecar(t *testing.T, dimension int, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			_ = json.NewEncoder(w).Encode(map[string]int{"dimension": dimension})
			return
		}
		var req embedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "边车起不来"})
			return
		}
		vectors := make([][]float32, len(req.Texts))
		for i := range vectors {
			vectors[i] = make([]float32, dimension)
		}
		_ = json.NewEncoder(w).Encode(embedResponse{
			Dimension: dimension,
			Count:     len(req.Texts),
			Vectors:   vectors,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// 同一个 Embedder 会被 api 的多个请求处理器并发调用：每个语义检索请求
// 都要把自己的提问转成查询向量。维度是在首次响应时才写进去的，
// 所以这个字段一旦用普通 int，就是一次数据竞争 —— 跑 -race 会直接报出来。
//
// 这条用例的重点不是断言，而是被 -race 跑到。实测过反向验证：
// 把这个字段改回普通 int，`go test -race` 会在这里直接报
// DATA RACE（读在 Embed 的维度校验处，写在同一次调用的结尾）。
//
// 注意 `go test` 不带 -race 时它照样通过 —— 这类问题本来就不会稳定复现，
// 所以改动这里时请带着 -race 一起跑。
func TestEmbedderIsSafeForConcurrentUse(t *testing.T) {
	srv := fakeSidecar(t, 512, http.StatusOK)
	e := NewEmbedder(srv.URL)

	const goroutines = 32
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	wg.Add(goroutines)
	for i := range goroutines {
		go func() {
			defer wg.Done()
			vectors, err := e.Embed(context.Background(), []string{"合并代码之前需要做什么检查"})
			if err == nil && len(vectors) != 1 {
				t.Errorf("goroutine %d 拿到 %d 条向量，期望 1 条", i, len(vectors))
			}
			errs[i] = err
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d 出错: %v", i, err)
		}
	}
	if got := e.Dimension(); got != 512 {
		t.Fatalf("维度为 %d，期望 512", got)
	}
}

func TestHealthReturnsSidecarDimension(t *testing.T) {
	e := NewEmbedder(fakeSidecar(t, 512, http.StatusOK).URL)

	dim, err := e.Health(context.Background())
	if err != nil {
		t.Fatalf("探活失败: %v", err)
	}
	if dim != 512 {
		t.Fatalf("维度为 %d，期望 512", dim)
	}
}

// Health 拿到维度之后，后续响应的维度必须与它一致。
// 这条守的是「边车被换成另一个模型」：不拦的话，不匹配的向量会一路写到
// vector(512) 的列上，最后由数据库抛一个和真实原因毫不相干的错误。
func TestEmbedRejectsDimensionChange(t *testing.T) {
	e := NewEmbedder(fakeSidecar(t, 512, http.StatusOK).URL)
	if _, err := e.Health(context.Background()); err != nil {
		t.Fatalf("探活失败: %v", err)
	}
	// 换一个维度不一样的边车，模拟模型被换掉
	other := NewEmbedder(fakeSidecar(t, 768, http.StatusOK).URL)
	other.setDimension(512)

	_, err := other.Embed(context.Background(), []string{"任意文本"})
	if err == nil {
		t.Fatal("维度从 512 变成 768，却通过了校验")
	}
	if want := "向量维度不符"; !contains(err.Error(), want) {
		t.Fatalf("错误信息为 %q，期望含有 %q", err.Error(), want)
	}
}

// 边车返回非 200（模型还在加载、容器刚重启）必须是错误，
// 而不是「返回 0 条向量」——后者会让索引任务当成成功，把空结果落库。
func TestEmbedFailsOnNonOKResponse(t *testing.T) {
	e := NewEmbedder(fakeSidecar(t, 512, http.StatusServiceUnavailable).URL)

	vectors, err := e.Embed(context.Background(), []string{"任意文本"})
	if err == nil {
		t.Fatalf("边车返回 503，却拿到 %d 条向量", len(vectors))
	}
	if want := "503"; !contains(err.Error(), want) {
		t.Fatalf("错误信息为 %q，期望含有 %q", err.Error(), want)
	}
}

// 向量条数与请求条数不符时必须失败：否则切片与向量会错位，
// 第 N 段文字配上了第 M 段的向量，检索出来的片段看着有道理却对不上原文。
func TestEmbedRejectsVectorCountMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(embedResponse{
			Dimension: 512,
			Vectors:   [][]float32{make([]float32, 512)}, // 请求 2 条，只回 1 条
		})
	}))
	t.Cleanup(srv.Close)

	_, err := NewEmbedder(srv.URL).Embed(context.Background(), []string{"甲", "乙"})
	if err == nil {
		t.Fatal("请求 2 条只回 1 条，却通过了校验")
	}
	if want := "向量条数不符"; !contains(err.Error(), want) {
		t.Fatalf("错误信息为 %q，期望含有 %q", err.Error(), want)
	}
}

// EmbedAll 按 embedBatch 分批，批与批之间不能丢条、不能串序。
func TestEmbedAllKeepsOrderAcrossBatches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		vectors := make([][]float32, len(req.Texts))
		for i, text := range req.Texts {
			// 用文本里带的编号当向量的第 0 维，回读时就能确认顺序没乱
			n, _ := strconv.Atoi(text)
			v := make([]float32, 512)
			v[0] = float32(n)
			vectors[i] = v
		}
		_ = json.NewEncoder(w).Encode(embedResponse{
			Dimension: 512,
			Count:     len(req.Texts),
			Vectors:   vectors,
		})
	}))
	t.Cleanup(srv.Close)

	// 比 embedBatch 多出几条，保证至少跨两批
	texts := make([]string, embedBatch*2+3)
	for i := range texts {
		texts[i] = strconv.Itoa(i)
	}

	vectors, err := NewEmbedder(srv.URL).EmbedAll(context.Background(), texts)
	if err != nil {
		t.Fatalf("批量生成失败: %v", err)
	}
	if len(vectors) != len(texts) {
		t.Fatalf("拿到 %d 条向量，期望 %d 条", len(vectors), len(texts))
	}
	for i, v := range vectors {
		if got := int(v[0]); got != i {
			t.Fatalf("第 %d 条向量来自第 %d 段文本，批次之间串序了", i, got)
		}
	}
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
