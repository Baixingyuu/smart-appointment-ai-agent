package rag

import (
	"context"
	"strings"
	"testing"
)

// testChunks 构造小规模知识库，覆盖三个不同主题。
// 三个主题刻意用词差异明显，便于用离线向量化器区分。
func testChunks() []Chunk {
	return []Chunk{
		{
			ID: "c1", DocID: "d1", Title: "接口鉴权失败排查",
			Content:  "接口返回 401 通常表示鉴权令牌过期或签名不正确。请先检查 Authorization 头是否携带有效令牌，再确认系统时间是否准确。",
			Keywords: []string{"接口", "401", "鉴权", "令牌"},
		},
		{
			ID: "c2", DocID: "d1", Title: "接口超时排查",
			Content:  "接口超时常见原因是下游依赖响应缓慢或连接池耗尽。建议先查看调用链耗时分布，再确认数据库慢查询与连接池配置。",
			Keywords: []string{"接口", "超时", "连接池"},
		},
		{
			ID: "c3", DocID: "d2", Title: "账号被锁定处理",
			Content:  "连续多次输入错误密码会触发账号锁定，通常锁定十五分钟。管理员可在成员管理中重置密码并解锁账号。",
			Keywords: []string{"账号", "锁定", "密码", "解锁"},
		},
		{
			ID: "c4", DocID: "d3", Title: "私有化部署硬件要求",
			Content:  "私有化部署最低需要四核八GB内存，建议十六GB以上。需要可访问外部模型服务的网络出口，或配置内网模型服务。",
			Keywords: []string{"私有化", "部署", "硬件", "内存"},
		},
		{
			ID: "c5", DocID: "d3", Title: "私有化部署网络配置",
			Content:  "私有化部署需要开放应用端口与数据库端口，若使用外部模型服务还需开放出网访问。",
			Keywords: []string{"私有化", "部署", "网络", "端口"},
		},
	}
}

func newRetriever(t *testing.T, options Options) *Retriever {
	t.Helper()
	return New(testChunks(), NewHashEmbedder(512), options)
}

func TestRetrieveFindsRelevantChunk(t *testing.T) {
	r := newRetriever(t, DefaultOptions())
	result := r.Retrieve(context.Background(), "接口返回 401 鉴权失败怎么办")

	if !result.Gate.Sufficient {
		t.Fatalf("应判定为证据充分，实际 reason=%s message=%s", result.Gate.Reason, result.Gate.Message)
	}
	if len(result.Hits) == 0 {
		t.Fatal("应有命中")
	}
	if result.Hits[0].Chunk.ID != "c1" {
		t.Fatalf("最相关片段应为 c1（接口鉴权），实际 %s", result.Hits[0].Chunk.ID)
	}
	if result.Context == "" {
		t.Fatal("证据充分时必须生成上下文")
	}
	if !strings.Contains(result.Context, "鉴权") {
		t.Errorf("上下文应包含证据内容，实际：%s", result.Context)
	}
}

func TestRetrieveNoKnowledgeBaseYieldsNoKnowledge(t *testing.T) {
	r := New(nil, NewHashEmbedder(256), DefaultOptions())
	result := r.Retrieve(context.Background(), "任意问题")

	if result.Gate.Reason != ReasonNoKnowledge {
		t.Fatalf("未绑定知识库时应返回 no_knowledge，实际 %s", result.Gate.Reason)
	}
	if result.Gate.Sufficient {
		t.Error("无知识库不应判定为充分")
	}
	if result.Context != "" {
		t.Error("不充分时不应生成上下文")
	}
}

func TestRetrieveEmptyQueryYieldsEmptyQuery(t *testing.T) {
	r := newRetriever(t, DefaultOptions())
	for _, query := range []string{"", "   ", "\t\n"} {
		result := r.Retrieve(context.Background(), query)
		if result.Gate.Reason != ReasonEmptyQuery {
			t.Fatalf("空提问应返回 empty_query，实际 %s（输入 %q）", result.Gate.Reason, query)
		}
	}
}

// TestGateDistinguishesFailureReasons 是本文件最重要的用例。
//
// 它验证「失败归因」这一核心改进：必须能区分
//   - no_hit   ：知识库里根本没有相关内容 → 该补知识
//   - low_score：有字面命中但相关度不足   → 该调阈值或改向量模型
//
// 参考实现的判空 gate 无法区分这三者，改进方向因此不可归因。
func TestGateDistinguishesFailureReasons(t *testing.T) {
	t.Run("知识未覆盖应归因为 no_hit", func(t *testing.T) {
		r := newRetriever(t, DefaultOptions())
		// 知识库全是技术支持内容，与「报销流程」毫无关系。
		result := r.Retrieve(context.Background(), "员工报销流程需要哪些材料")

		if result.Gate.Sufficient {
			t.Fatalf("无关提问不应判定为充分")
		}
		if result.Gate.Reason != ReasonNoHit {
			t.Fatalf("应归因为 no_hit，实际 %s（%s）", result.Gate.Reason, result.Gate.Message)
		}
	})

	t.Run("阈值过高应归因为 low_score", func(t *testing.T) {
		// 把阈值抬到 0.99：即使确实命中了相关片段，也会因分数不达标而判负。
		// 覆盖率下限同时放得很低，使判定停在"分数不足"这一步。
		options := DefaultOptions()
		options.ScoreThreshold = 0.99
		options.MinCoverage = 0.05
		r := newRetriever(t, options)

		result := r.Retrieve(context.Background(), "接口返回 401 鉴权失败怎么办")
		if result.Gate.Sufficient {
			t.Fatal("阈值 0.99 时不应判定为充分")
		}
		// 关键：有命中但分数不足，必须与「没命中」区分开。
		if result.Gate.Reason != ReasonLowScore {
			t.Fatalf("应归因为 low_score，实际 %s（%s）", result.Gate.Reason, result.Gate.Message)
		}
		if len(result.Hits) == 0 {
			t.Error("low_score 意味着有命中，Hits 不应为空")
		}
	})

	t.Run("覆盖率不足应归因为 low_coverage", func(t *testing.T) {
		options := DefaultOptions()
		options.ScoreThreshold = 0.01 // 放行分数，只让覆盖率判负
		options.MinCoverage = 0.99
		r := newRetriever(t, options)

		result := r.Retrieve(context.Background(), "接口返回 401 鉴权失败怎么办")
		if result.Gate.Sufficient {
			t.Fatal("覆盖率下限 0.99 时不应判定为充分")
		}
		if result.Gate.Reason != ReasonLowCoverage {
			t.Fatalf("应归因为 low_coverage，实际 %s（%s）", result.Gate.Reason, result.Gate.Message)
		}
		if result.Gate.Coverage <= 0 {
			t.Error("low_coverage 必须回传覆盖率数值，否则无法诊断")
		}
	})
}

func TestRetrieveIsDeterministic(t *testing.T) {
	// 同一提问必须每次得到完全相同的命中顺序与上下文。
	// 否则评测结果不可复现，也无法做跨版本对比。
	r := newRetriever(t, DefaultOptions())
	query := "私有化部署需要什么网络配置"

	first := r.Retrieve(context.Background(), query)
	for i := 0; i < 10; i++ {
		again := r.Retrieve(context.Background(), query)
		if again.Context != first.Context {
			t.Fatalf("第 %d 次上下文不一致", i)
		}
		if len(again.Hits) != len(first.Hits) {
			t.Fatalf("第 %d 次命中数不一致", i)
		}
		for j := range first.Hits {
			if again.Hits[j].Chunk.ID != first.Hits[j].Chunk.ID {
				t.Fatalf("第 %d 次命中顺序漂移：位置 %d %s → %s",
					i, j, first.Hits[j].Chunk.ID, again.Hits[j].Chunk.ID)
			}
		}
	}
}

func TestBuildContextLimitsPerDocument(t *testing.T) {
	// 单文档的片段不得占满全部上下文名额，否则其他来源的证据会被挤掉，
	// 模型只能看到一个来源，而它无从知道还有其他文档。
	//
	// 阈值刻意放宽：本用例检验的是 buildContext 的配额逻辑，
	// 不该因为向量分恰好落在阈值边缘而被跳过（跳过等于没测）。
	options := DefaultOptions()
	options.MaxPerDoc = 1
	options.MaxContextItems = 5
	options.ScoreThreshold = 0.01
	options.MinCoverage = 0.01
	r := newRetriever(t, options)

	// d3 下同时有 c4（硬件要求）与 c5（网络配置）两条片段。
	result := r.Retrieve(context.Background(), "私有化部署 硬件 网络")
	if !result.Gate.Sufficient {
		t.Fatalf("用例前提不成立，应判定为充分：%s", result.Gate.Message)
	}

	// 断言：上下文里每个文档最多出现 MaxPerDoc 次。
	// 直接数文档标记，不依赖标题文本，避免标题改动让用例静默失效。
	counts := make(map[string]int)
	for _, line := range strings.Split(result.Context, "\n") {
		idx := strings.Index(line, "｜文档 ")
		if idx < 0 {
			continue
		}
		rest := line[idx+len("｜文档 "):]
		end := strings.Index(rest, "｜")
		if end < 0 {
			continue
		}
		counts[rest[:end]]++
	}
	if len(counts) == 0 {
		t.Fatalf("未从上下文中解析出文档标记：\n%s", result.Context)
	}
	for docID, count := range counts {
		if count > options.MaxPerDoc {
			t.Errorf("文档 %s 在上下文中出现 %d 次，超过 MaxPerDoc=%d", docID, count, options.MaxPerDoc)
		}
	}
}

func TestBuildContextRespectsMaxItems(t *testing.T) {
	options := DefaultOptions()
	options.MaxContextItems = 1
	options.ScoreThreshold = 0.01
	options.MinCoverage = 0.01
	r := newRetriever(t, options)

	result := r.Retrieve(context.Background(), "接口超时")
	if !result.Gate.Sufficient {
		t.Skip("未判定为充分，跳过")
	}
	if count := strings.Count(result.Context, "【证据"); count > 1 {
		t.Fatalf("上下文片段数应受 MaxContextItems 限制，实际 %d", count)
	}
}

func TestOptionsNormalization(t *testing.T) {
	// 非法参数必须回落到默认值，而不是让检索行为变得不可预测。
	options := Options{}.normalize()
	def := DefaultOptions()
	if options.TopK != def.TopK || options.ScoreThreshold != def.ScoreThreshold ||
		options.MaxContextItems != def.MaxContextItems || options.MaxPerDoc != def.MaxPerDoc {
		t.Fatalf("零值参数应回落到默认值，实际 %+v", options)
	}
}

func TestHashEmbedderIsDeterministicAndNormalized(t *testing.T) {
	e := NewHashEmbedder(128)

	first := e.Embed("接口返回 401")
	second := e.Embed("接口返回 401")
	if len(first) != 128 {
		t.Fatalf("维度应为 128，实际 %d", len(first))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("相同输入必须得到相同向量，位置 %d 不一致", i)
		}
	}

	// L2 归一化：模长应为 1（非空输入）。
	var norm float64
	for _, v := range first {
		norm += v * v
	}
	if norm < 0.999 || norm > 1.001 {
		t.Fatalf("向量应做 L2 归一化，实际模长平方 %f", norm)
	}

	// 空输入得到零向量而非 panic。
	empty := e.Embed("")
	for _, v := range empty {
		if v != 0 {
			t.Fatal("空输入应得到零向量")
		}
	}
}

func TestCosineSimilarityEdgeCases(t *testing.T) {
	// 维度不一致返回 0 而非报错：无法比较的片段跳过比让整次检索失败更合理。
	if got := cosineSimilarity([]float64{1, 2}, []float64{1, 2, 3}); got != 0 {
		t.Errorf("维度不一致应返回 0，实际 %f", got)
	}
	if got := cosineSimilarity(nil, []float64{1}); got != 0 {
		t.Errorf("空向量应返回 0，实际 %f", got)
	}
	if got := cosineSimilarity([]float64{0, 0}, []float64{1, 1}); got != 0 {
		t.Errorf("零向量应返回 0，实际 %f", got)
	}
	// 同向向量相似度为 1。
	if got := cosineSimilarity([]float64{1, 0}, []float64{2, 0}); got < 0.999 {
		t.Errorf("同向向量相似度应为 1，实际 %f", got)
	}
}

func TestKeywordCoverageIgnoresPunctuation(t *testing.T) {
	hits := []Hit{{Chunk: Chunk{ID: "x", Title: "接口鉴权", Content: "令牌过期"}}}
	// 加了标点后覆盖率不应下降。
	plain := keywordCoverage("接口鉴权令牌", hits)
	punctuated := keywordCoverage("接口，鉴权。令牌？", hits)
	if plain != punctuated {
		t.Fatalf("标点不应影响覆盖率：%.4f vs %.4f", plain, punctuated)
	}
}

func TestRetrieveRespectsContextCancellation(t *testing.T) {
	r := newRetriever(t, DefaultOptions())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := r.Retrieve(ctx, "接口超时")
	if result.Gate.Sufficient {
		t.Fatal("已取消的上下文不应判定为充分")
	}
}
