package rag

import (
	"context"
	"testing"
)

// 用真实知识库语料对比 BM25 与哈希向量化器的检索质量。
func TestBM25RetrievalQuality(t *testing.T) {
	chunks := []Chunk{
		{ID: "k1", DocID: "d1", Title: "接口鉴权失败排查",
			Content: "接口返回 401 表示鉴权令牌过期或签名不正确。请检查请求头 Authorization 是否携带有效令牌。"},
		{ID: "k2", DocID: "d1", Title: "接口超时排查",
			Content: "接口超时常见原因是下游依赖响应缓慢或连接池耗尽。"},
		{ID: "k3", DocID: "d2", Title: "账号被锁定处理",
			Content: "连续多次输入错误密码会触发账号锁定，通常锁定十五分钟。管理员可重置密码并解锁。"},
		{ID: "k4", DocID: "d3", Title: "私有化部署硬件要求",
			Content: "私有化部署最低需要四核八 GB 内存，建议十六 GB 以上。"},
		{ID: "k5", DocID: "d3", Title: "私有化部署网络配置",
			Content: "私有化部署需要开放应用端口与数据库端口，使用外部模型服务需开放出网。"},
	}

	docs := make([]string, 0, len(chunks))
	for _, c := range chunks {
		docs = append(docs, c.titleAndContent())
	}
	bm25 := NewBM25Embedder(docs)
	options := DefaultOptions()

	// 期望：提问 → 应命中的片段
	cases := []struct {
		query string
		want  string
	}{
		{"接口返回 401 鉴权失败怎么办", "k1"},
		{"账号被锁定了怎么解锁", "k3"},
		{"私有化部署需要什么网络配置", "k5"},
		{"接口老是超时是什么原因", "k2"},
		{"私有化部署的硬件要求是什么", "k4"},
	}

	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			r := NewWithScorer(chunks, bm25, bm25, options)
			result := r.Retrieve(context.Background(), tc.query)

			if len(result.Hits) == 0 {
				t.Fatalf("无命中，门控=%s", result.Gate.Message)
			}
			if result.Hits[0].Chunk.ID != tc.want {
				t.Errorf("期望首位命中 %s，实际 %s（分数 %.3f，门控=%s）",
					tc.want, result.Hits[0].Chunk.ID, result.Hits[0].Score, result.Gate.Message)
			}
			if !result.Gate.Sufficient {
				t.Errorf("应判定为证据充分，实际 %s（%s）", result.Gate.Reason, result.Gate.Message)
			}
			t.Logf("首位=%s 分数=%.3f 门控=%s 覆盖=%.2f",
				result.Hits[0].Chunk.ID, result.Hits[0].Score, result.Gate.Reason, result.Gate.Coverage)
		})
	}
}

// 无关提问必须被判定为知识库未覆盖，而不是给出低分命中。
func TestBM25RejectsUnrelatedQuery(t *testing.T) {
	chunks := []Chunk{
		{ID: "k1", DocID: "d1", Title: "接口鉴权失败排查", Content: "接口返回 401 表示鉴权令牌过期。"},
		{ID: "k2", DocID: "d2", Title: "账号被锁定处理", Content: "连续错误密码触发账号锁定。"},
	}
	docs := []string{chunks[0].titleAndContent(), chunks[1].titleAndContent()}
	bm25 := NewBM25Embedder(docs)

	r := NewWithScorer(chunks, bm25, bm25, DefaultOptions())
	result := r.Retrieve(context.Background(), "员工报销流程需要哪些材料")

	if result.Gate.Sufficient {
		t.Fatalf("无关提问不应判定为充分")
	}
	if result.Gate.Reason != ReasonNoHit {
		t.Errorf("应归因为 no_hit，实际 %s（%s）", result.Gate.Reason, result.Gate.Message)
	}
}
