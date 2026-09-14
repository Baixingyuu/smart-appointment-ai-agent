package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mac/agentdesk/internal/eval"
	"github.com/mac/agentdesk/internal/evalrun"
	"github.com/mac/agentdesk/internal/rag"
)

// runEvalRetrieval 跑检索召回评测。
//
// 与其它评测命令的区别：本命令用**评测集自带的语料**构建索引，
// 而不是复用 seed 的知识库。原因是召回率必须相对一份固定的、
// 带金标标注的语料来衡量；用会变动的业务知识库跑，
// 指标就失去了跨版本可比性。
func runEvalRetrieval(args []string) error {
	fs := flag.NewFlagSet("eval-retrieval", flag.ContinueOnError)
	datasetPath := fs.String("dataset", defaultRetrievalDatasetPath(), "检索评测集路径")
	jsonPath := fs.String("json", "", "机读报告输出路径")
	topK := fs.Int("k", 5, "召回截断位置 K，同时决定 Recall@K 的含义")
	// 允许调阈值做敏感性分析：假命中率与召回率对此高度敏感，
	// 只看单一阈值下的数字无法判断该如何取舍。
	threshold := fs.Float64("threshold", 0, "覆盖检索器的分数阈值（0 表示用默认值）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	dataset, err := eval.LoadRetrievalDataset(*datasetPath)
	if err != nil {
		return err
	}
	if err := dataset.Validate(); err != nil {
		return fmt.Errorf("评测集校验失败: %w", err)
	}

	retriever := buildRetrieverFromCorpus(dataset.Corpus, *topK, *threshold)
	runner := evalrun.NewRetrievalRunner(retriever, context.Background())

	startedAt := time.Now()
	report := eval.RunRetrievalEval(dataset, runner, *topK)

	fmt.Fprintf(os.Stderr, "语料 %d 段，查询 %d 条，K=%d\n\n",
		len(dataset.Corpus), len(dataset.Queries), *topK)
	fmt.Printf("%s\n耗时 %v\n", report.Text(), time.Since(startedAt).Round(time.Millisecond))

	if *jsonPath != "" {
		if err := writeJSON(*jsonPath, report); err != nil {
			return err
		}
		fmt.Printf("\n机读报告已写入 %s\n", *jsonPath)
	}
	return nil
}

// buildRetrieverFromCorpus 用评测集语料构建 BM25 检索器。
//
// 默认使用 BM25 而非模型向量化：BM25 无需外部服务，评测因此完全可复现，
// 这也是离线基线应有的形态。要对比模型向量化的效果，
// 可在此基础上接入 rag.NewOpenAIEmbedder。
func buildRetrieverFromCorpus(chunks []eval.RetrievalChunk, topK int, threshold float64) *rag.Retriever {
	ragChunks := make([]rag.Chunk, 0, len(chunks))
	documents := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		ragChunks = append(ragChunks, rag.Chunk{
			ID:      chunk.ID,
			DocID:   chunk.DocID,
			Title:   chunk.Title,
			Content: chunk.Content,
		})
		documents = append(documents, chunk.Title+" "+chunk.Content)
	}

	bm25 := rag.NewBM25Embedder(documents)
	options := rag.DefaultOptions()
	options.TopK = topK
	if threshold > 0 {
		options.ScoreThreshold = threshold
	}
	return rag.NewWithScorer(ragChunks, bm25, bm25, options)
}

// defaultRetrievalDatasetPath 定位检索评测集。
func defaultRetrievalDatasetPath() string {
	candidates := []string{
		"eval/datasets/retrieval.json",
		"../eval/datasets/retrieval.json",
		"../../eval/datasets/retrieval.json",
	}
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return filepath.Join("eval", "datasets", "retrieval.json")
}
