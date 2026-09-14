package eval

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// stubRetriever 按查询返回预置命中，用于独立验证评测框架。
type stubRetriever struct {
	byQuery map[string]RetrievalResult
	err     error
	// calls 记录收到的 topK，用于断言 K 被正确传递。
	calls []int
}

func newStubRetriever() *stubRetriever {
	return &stubRetriever{byQuery: make(map[string]RetrievalResult)}
}

func (s *stubRetriever) RetrieveForEval(query string, topK int) (RetrievalResult, error) {
	s.calls = append(s.calls, topK)
	if s.err != nil {
		return RetrievalResult{}, s.err
	}
	result, ok := s.byQuery[query]
	if !ok {
		return RetrievalResult{}, nil
	}
	return result, nil
}

// hits 构造命中结果。decided 表示检索器是否自认找到了证据。
func hits(decided bool, reason string, docIDs ...string) RetrievalResult {
	ret := RetrievalResult{DecidedHit: decided, Reason: reason}
	for i, docID := range docIDs {
		ret.Hits = append(ret.Hits, RetrievalHit{DocID: docID, Score: float64(10 - i)})
	}
	return ret
}

func retrievalDataset(corpus []RetrievalChunk, queries ...RetrievalQuery) *RetrievalDataset {
	return &RetrievalDataset{Version: 1, Corpus: corpus, Queries: queries}
}

func chunk(id, docID string) RetrievalChunk {
	return RetrievalChunk{ID: id, DocID: docID, Title: "标题" + id, Content: "内容" + id}
}

func TestRunRetrievalEvalComputesRecallAndMRR(t *testing.T) {
	runner := newStubRetriever()
	// 第 1 名命中。
	runner.byQuery["问题一"] = hits(true, "sufficient", "doc-a", "doc-b")
	// 第 2 名命中（doc-x 不相关）。
	runner.byQuery["问题二"] = hits(true, "sufficient", "doc-x", "doc-c")
	// 完全未命中。
	runner.byQuery["问题三"] = hits(false, "no_hit", "doc-y", "doc-z")

	dataset := retrievalDataset(
		[]RetrievalChunk{chunk("k1", "doc-a"), chunk("k2", "doc-c")},
		RetrievalQuery{ID: "q1", Text: "问题一", RelevantDocIDs: []string{"doc-a"}},
		RetrievalQuery{ID: "q2", Text: "问题二", RelevantDocIDs: []string{"doc-c"}},
		RetrievalQuery{ID: "q3", Text: "问题三", RelevantDocIDs: []string{"doc-a"}},
	)

	report := RunRetrievalEval(dataset, runner, 5)

	if report.Answerable != 3 {
		t.Fatalf("可回答数应为 3，实际 %d", report.Answerable)
	}
	if report.Hits != 2 {
		t.Fatalf("命中数应为 2，实际 %d", report.Hits)
	}
	// Recall@5 = 2/3
	if report.RecallAtK < 0.666 || report.RecallAtK > 0.667 {
		t.Errorf("Recall@5 应约 0.667，实际 %f", report.RecallAtK)
	}
	// MRR = (1/1 + 1/2 + 0) / 3 = 0.5
	if report.MRR < 0.499 || report.MRR > 0.501 {
		t.Errorf("MRR 应为 0.5，实际 %f", report.MRR)
	}
}

func TestFirstRelevantRankDeduplicatesDocs(t *testing.T) {
	// 同一文档的多个片段不应重复占用排名位次，
	// 否则 MRR 会被同一来源的多个片段人为抬高。
	runner := newStubRetriever()
	runner.byQuery["问题"] = hits(true, "sufficient", "doc-a", "doc-a", "doc-a", "doc-b")

	dataset := retrievalDataset(
		[]RetrievalChunk{chunk("k1", "doc-a"), chunk("k2", "doc-b")},
		RetrievalQuery{ID: "q1", Text: "问题", RelevantDocIDs: []string{"doc-b"}},
	)
	report := RunRetrievalEval(dataset, runner, 5)

	// doc-b 去重后是第 2 位，而非第 4 位。
	if report.Results[0].FirstRank != 2 {
		t.Fatalf("去重后 doc-b 应排第 2，实际第 %d", report.Results[0].FirstRank)
	}
	if report.MRR < 0.499 || report.MRR > 0.501 {
		t.Errorf("MRR 应为 0.5，实际 %f", report.MRR)
	}
}

func TestFalsePositiveRateOnUnanswerableQueries(t *testing.T) {
	// 关键指标：知识库没有的主题，检索器不该表现自信。
	// 只看召回率会漏掉这种假命中，而它直接导致模型基于无关内容作答。
	runner := newStubRetriever()
	runner.byQuery["报销流程"] = hits(true, "sufficient", "doc-a")   // 假命中
	runner.byQuery["请假审批"] = hits(false, "no_hit")               // 正确拒绝
	runner.byQuery["怎么重置密码"] = hits(true, "sufficient", "doc-a") // 正常命中

	dataset := retrievalDataset(
		[]RetrievalChunk{chunk("k1", "doc-a")},
		RetrievalQuery{ID: "q1", Text: "报销流程"},
		RetrievalQuery{ID: "q2", Text: "请假审批"},
		RetrievalQuery{ID: "q3", Text: "怎么重置密码", RelevantDocIDs: []string{"doc-a"}},
	)

	report := RunRetrievalEval(dataset, runner, 5)

	if report.Unanswerable != 2 {
		t.Fatalf("不可回答数应为 2，实际 %d", report.Unanswerable)
	}
	if report.DecidedHitOnUnanswerable != 1 {
		t.Fatalf("应检出 1 次假命中，实际 %d", report.DecidedHitOnUnanswerable)
	}
	if report.FalsePositiveRate != 0.5 {
		t.Errorf("假命中率应为 0.5，实际 %f", report.FalsePositiveRate)
	}
	// 假命中样本必须出现在问题样本里，便于定位。
	var found bool
	for _, item := range report.Errors {
		if item.QueryID == "q1" {
			found = true
		}
	}
	if !found {
		t.Error("假命中样本应列入问题样本")
	}
}

func TestSeparatesMissedFromFalsePositive(t *testing.T) {
	// 漏召回与假命中是两类不同的失败，必须分开计数：
	// 前者该补检索，后者该收紧置信判定。
	runner := newStubRetriever()
	runner.byQuery["漏了"] = hits(false, "no_hit", "doc-x")     // 可回答但没找到
	runner.byQuery["多嘴了"] = hits(true, "sufficient", "doc-y") // 不可回答却判有证据

	dataset := retrievalDataset(
		[]RetrievalChunk{chunk("k1", "doc-a")},
		RetrievalQuery{ID: "q1", Text: "漏了", RelevantDocIDs: []string{"doc-a"}},
		RetrievalQuery{ID: "q2", Text: "多嘴了"},
	)

	report := RunRetrievalEval(dataset, runner, 5)
	if report.MissedOnAnswerable != 1 {
		t.Errorf("漏召回应为 1，实际 %d", report.MissedOnAnswerable)
	}
	if report.DecidedHitOnUnanswerable != 1 {
		t.Errorf("假命中应为 1，实际 %d", report.DecidedHitOnUnanswerable)
	}
	// 两者不应互相污染。
	if report.Hits != 0 {
		t.Errorf("漏召回不应计入命中，实际 %d", report.Hits)
	}
}

func TestTopKIsPassedToRetriever(t *testing.T) {
	runner := newStubRetriever()
	dataset := retrievalDataset(
		[]RetrievalChunk{chunk("k1", "doc-a")},
		RetrievalQuery{ID: "q1", Text: "问题", RelevantDocIDs: []string{"doc-a"}},
	)

	RunRetrievalEval(dataset, runner, 3)
	if len(runner.calls) != 1 || runner.calls[0] != 3 {
		t.Fatalf("topK 应被传递给检索器，实际 %v", runner.calls)
	}

	// 非法 K 应回落到默认值，而不是取回 0 条导致召回率恒为 0。
	runner.calls = nil
	RunRetrievalEval(dataset, runner, 0)
	if len(runner.calls) != 1 || runner.calls[0] <= 0 {
		t.Fatalf("非法 K 应回落到正数默认值，实际 %v", runner.calls)
	}
}

func TestByReasonBucketsRetrievalFailures(t *testing.T) {
	// 归因分桶决定改进方向：no_hit 该补知识，low_score 该调检索。
	runner := newStubRetriever()
	runner.byQuery["a"] = hits(false, "no_hit")
	runner.byQuery["b"] = hits(false, "low_score", "doc-x")
	runner.byQuery["c"] = hits(false, "low_coverage", "doc-y")

	dataset := retrievalDataset(
		[]RetrievalChunk{chunk("k1", "doc-a")},
		RetrievalQuery{ID: "q1", Text: "a", RelevantDocIDs: []string{"doc-a"}},
		RetrievalQuery{ID: "q2", Text: "b", RelevantDocIDs: []string{"doc-a"}},
		RetrievalQuery{ID: "q3", Text: "c", RelevantDocIDs: []string{"doc-a"}},
	)

	report := RunRetrievalEval(dataset, runner, 5)
	for _, reason := range []string{"no_hit", "low_score", "low_coverage"} {
		if report.ByReason[reason] != 1 {
			t.Errorf("归因 %s 应计 1 次，实际 %d", reason, report.ByReason[reason])
		}
	}
}

func TestRetrieverErrorDoesNotAbortRun(t *testing.T) {
	runner := newStubRetriever()
	runner.err = errors.New("检索服务不可用")

	dataset := retrievalDataset(
		[]RetrievalChunk{chunk("k1", "doc-a")},
		RetrievalQuery{ID: "q1", Text: "问题", RelevantDocIDs: []string{"doc-a"}},
	)
	report := RunRetrievalEval(dataset, runner, 5)

	if len(report.Errors) != 1 {
		t.Fatalf("失败应被记录，实际 %d", len(report.Errors))
	}
	if report.RecallAtK != 0 {
		t.Errorf("全部失败时召回率应为 0，实际 %f", report.RecallAtK)
	}
}

func TestDatasetValidateRejectsBrokenGoldReferences(t *testing.T) {
	// 金标指向不存在的文档时，召回率会永远为 0——
	// 那看起来像「检索效果差」，实际是数据错。必须在校验阶段拦住。
	cases := []struct {
		name    string
		dataset *RetrievalDataset
		wantErr bool
	}{
		{
			"合法",
			retrievalDataset(
				[]RetrievalChunk{chunk("k1", "doc-a")},
				RetrievalQuery{ID: "q1", Text: "问题", RelevantDocIDs: []string{"doc-a"}},
				RetrievalQuery{ID: "q2", Text: "无关"},
			),
			false,
		},
		{
			"金标引用不存在的文档",
			retrievalDataset(
				[]RetrievalChunk{chunk("k1", "doc-a")},
				RetrievalQuery{ID: "q1", Text: "问题", RelevantDocIDs: []string{"doc-nonexistent"}},
				RetrievalQuery{ID: "q2", Text: "无关"},
			),
			true,
		},
		{
			"缺少负样本",
			retrievalDataset(
				[]RetrievalChunk{chunk("k1", "doc-a")},
				RetrievalQuery{ID: "q1", Text: "问题", RelevantDocIDs: []string{"doc-a"}},
			),
			true,
		},
		{
			"片段 ID 重复",
			retrievalDataset(
				[]RetrievalChunk{chunk("k1", "doc-a"), chunk("k1", "doc-b")},
				RetrievalQuery{ID: "q1", Text: "问题", RelevantDocIDs: []string{"doc-a"}},
				RetrievalQuery{ID: "q2", Text: "无关"},
			),
			true,
		},
		{
			"查询 ID 重复",
			retrievalDataset(
				[]RetrievalChunk{chunk("k1", "doc-a")},
				RetrievalQuery{ID: "q1", Text: "问题", RelevantDocIDs: []string{"doc-a"}},
				RetrievalQuery{ID: "q1", Text: "无关"},
			),
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.dataset.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("应校验失败")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("不应失败: %v", err)
			}
		})
	}
}

func TestPrecisionAtKDeduplicatesAndHandlesEmpty(t *testing.T) {
	// 空结果集应返回 0 而非 1：没取回任何东西不能算「精确率满分」。
	if got := PrecisionAtK(nil, []string{"doc-a"}); got != 0 {
		t.Errorf("空结果集精确率应为 0，实际 %f", got)
	}
	// 去重后：doc-a 相关、doc-b 不相关 → 1/2
	if got := PrecisionAtK([]string{"doc-a", "doc-a", "doc-b"}, []string{"doc-a"}); got != 0.5 {
		t.Errorf("去重后精确率应为 0.5，实际 %f", got)
	}
	// 全相关 → 1.0
	if got := PrecisionAtK([]string{"doc-a"}, []string{"doc-a"}); got != 1.0 {
		t.Errorf("全相关精确率应为 1.0，实际 %f", got)
	}
}

func TestLoadRealRetrievalDataset(t *testing.T) {
	path := filepath.Join("..", "..", "eval", "datasets", "retrieval.json")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("未找到检索评测集: %v", err)
	}

	dataset, err := LoadRetrievalDataset(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if err := dataset.Validate(); err != nil {
		t.Fatalf("数据集校验失败: %v", err)
	}
	if len(dataset.Corpus) < 30 {
		t.Errorf("语料过小：%d 段", len(dataset.Corpus))
	}
	if len(dataset.Queries) < 80 {
		t.Errorf("查询过少：%d 条", len(dataset.Queries))
	}

	answerable, withNote := 0, 0
	for _, query := range dataset.Queries {
		if query.Answerable() {
			answerable++
		}
		if query.Note != "" {
			withNote++
		}
	}
	// 负样本与边界样本都必须有足够比例，否则指标区分力不足。
	if unanswerable := len(dataset.Queries) - answerable; unanswerable < 8 {
		t.Errorf("不可回答样本过少：%d", unanswerable)
	}
	if withNote < 20 {
		t.Errorf("带歧义说明的困难样本过少：%d", withNote)
	}
	t.Logf("语料 %d 段，查询 %d 条（可回答 %d），困难样本 %d 条",
		len(dataset.Corpus), len(dataset.Queries), answerable, withNote)
}
