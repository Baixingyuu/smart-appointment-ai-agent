// Package rag 实现检索增强生成。
//
// 与参考实现（agent-desk）的关键差异：它的「可回答性判定」只检查检索上下文
// 是否为空的字符串，无法区分「知识库没覆盖」与「检索没召回」——这两种情况
// 的改进方向完全不同（补知识 vs 改检索）。
//
// 本实现给出真正的充分性判定，并把失败原因结构化为 Reason，
// 使评测能按原因分桶归因。
package rag

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Reason 检索不充分的原因。
//
// 必须区分原因而不是简单地"没检索到"：补知识库与改检索参数是两件完全不同的事。
type Reason string

const (
	ReasonSufficient  Reason = "sufficient"   // 证据充分
	ReasonNoHit       Reason = "no_hit"       // 完全没有命中任何片段
	ReasonLowScore    Reason = "low_score"    // 有命中但最高分低于阈值
	ReasonLowCoverage Reason = "low_coverage" // 有命中但未覆盖提问的关键词
	ReasonEmptyQuery  Reason = "empty_query"  // 提问为空
	ReasonNoKnowledge Reason = "no_knowledge" // 未绑定任何知识库
)

// Chunk 一个知识片段。
type Chunk struct {
	ID        string
	DocID     string
	Title     string
	Content   string
	Keywords  []string
	Embedding []float64
}

// Hit 一条检索命中。
type Hit struct {
	Chunk Chunk
	Score float64
}

// GateResult 证据充分性判定结果。
type GateResult struct {
	Sufficient bool
	Reason     Reason
	// Message 为可读说明，直接写入进展与日志，便于人工复核。
	Message string
	// Coverage 关键词覆盖率，仅当 Reason 为 low_coverage 时有诊断意义。
	Coverage float64
}

// Options 检索参数。
type Options struct {
	TopK int
	// ScoreThreshold 最高分低于该值即判定为不充分。
	ScoreThreshold float64
	// MinCoverage 提问关键词覆盖率下限，低于该值判定为不充分。
	MinCoverage float64
	// MaxContextItems 最终进入上下文的片段数上限。
	MaxContextItems int
	// MaxPerDoc 同一文档最多取几条，避免单文档刷屏挤占其他来源。
	MaxPerDoc int
}

// DefaultOptions 返回默认检索参数。
//
// 阈值 0.35 的取舍：过低会把无关片段判为可用，导致模型基于噪声作答；
// 过高则频繁误判为不充分、把本可自助解决的问题推给人工。默认值偏保守
// （宁可多转人工），因为错误作答的代价高于转人工。
func DefaultOptions() Options {
	return Options{
		TopK:            8,
		ScoreThreshold:  0.35,
		MinCoverage:     0.34,
		MaxContextItems: 4,
		MaxPerDoc:       2,
	}
}

// normalize 归一化非法参数，使调用方不必处理边界。
func (o Options) normalize() Options {
	def := DefaultOptions()
	if o.TopK <= 0 {
		o.TopK = def.TopK
	}
	if o.ScoreThreshold <= 0 {
		o.ScoreThreshold = def.ScoreThreshold
	}
	if o.MinCoverage <= 0 {
		o.MinCoverage = def.MinCoverage
	}
	if o.MaxContextItems <= 0 {
		o.MaxContextItems = def.MaxContextItems
	}
	if o.MaxPerDoc <= 0 {
		o.MaxPerDoc = def.MaxPerDoc
	}
	return o
}

// Result 一次检索的完整结果。
type Result struct {
	Query   string
	Hits    []Hit
	Context string
	Gate    GateResult
	// Rounds 记录检索轮次。一期固定为 1；二期引入查询改写后递增。
	Rounds int
}

// Retriever 检索器。
type Retriever struct {
	chunks   []Chunk
	embedder Embedder
	// scorer 为可选的专用打分器。
	//
	// 存在的原因：BM25 的打分公式与余弦相似度不同（含词频饱和与
	// 文档长度归一化），强行套用余弦会丢掉这两项关键设计。
	// 为空时退化为余弦相似度，适用于稠密向量。
	scorer  Scorer
	options Options
}

// Scorer 按查询给文档打相关性分数。
type Scorer interface {
	Score(query string, document string) float64
}

// NewWithScorer 构造使用专用打分器的检索器。
//
// 与 New 的区别：New 走「向量 + 余弦」，本函数走打分器给出的分数。
// BM25 这类稀疏检索必须用它，否则打分逻辑会被余弦覆盖。
func NewWithScorer(chunks []Chunk, embedder Embedder, scorer Scorer, options Options) *Retriever {
	ret := New(chunks, embedder, options)
	ret.scorer = scorer
	return ret
}

// New 构造检索器。
func New(chunks []Chunk, embedder Embedder, options Options) *Retriever {
	copied := make([]Chunk, len(chunks))
	copy(copied, chunks)

	// 预计算缺失的向量，避免每次检索重复计算。
	// 使用专用打分器时向量不参与打分，但保留计算以支持两种模式切换。
	if embedder != nil {
		for i := range copied {
			if len(copied[i].Embedding) == 0 {
				copied[i].Embedding = embedder.Embed(copied[i].titleAndContent())
			}
		}
	}
	return &Retriever{chunks: copied, embedder: embedder, options: options.normalize()}
}

// ChunkCount 返回知识库片段数。
func (r *Retriever) ChunkCount() int { return len(r.chunks) }

// Options 返回生效的检索参数。
func (r *Retriever) Options() Options { return r.options }

// Retrieve 执行检索并做证据充分性判定。
func (r *Retriever) Retrieve(ctx context.Context, query string) Result {
	result := Result{Query: strings.TrimSpace(query), Rounds: 1}

	if result.Query == "" {
		result.Gate = GateResult{Reason: ReasonEmptyQuery, Message: "提问为空，无法检索"}
		return result
	}
	if len(r.chunks) == 0 || (r.embedder == nil && r.scorer == nil) {
		result.Gate = GateResult{Reason: ReasonNoKnowledge, Message: "未绑定任何知识库"}
		return result
	}
	if err := ctx.Err(); err != nil {
		result.Gate = GateResult{Reason: ReasonNoKnowledge, Message: "检索被取消：" + err.Error()}
		return result
	}

	queryVector := r.embedder.Embed(result.Query)
	result.Hits = r.search(result.Query, queryVector)
	result.Gate = r.gate(result.Query, result.Hits)
	if result.Gate.Sufficient {
		result.Context = r.buildContext(result.Hits)
	}
	return result
}

// search 计算相似度并返回 TopK。
func (r *Retriever) search(query string, queryVector []float64) []Hit {
	hits := make([]Hit, 0, len(r.chunks))
	for _, chunk := range r.chunks {
		var score float64
		if r.scorer != nil {
			score = r.scorer.Score(query, chunk.titleAndContent())
		} else {
			score = cosineSimilarity(queryVector, chunk.Embedding)
		}
		if score <= 0 {
			continue
		}
		hits = append(hits, Hit{Chunk: chunk, Score: score})
	}
	// 排序必须先于截断，且平局时按 Chunk.ID 收敛，
	// 保证同一提问每次得到相同的检索结果。
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Chunk.ID < hits[j].Chunk.ID
	})
	if len(hits) > r.options.TopK {
		hits = hits[:r.options.TopK]
	}
	return hits
}

// gate 判定证据是否足以作答。
//
// 判据顺序刻意设计为「先词面、后向量」：
//
//	① 词面命中：提问的关键词是否在片段文本中出现过
//	   完全没有 → no_hit（知识库确实没覆盖，该补知识）
//	② 词面覆盖率：出现过的比例是否达标
//	   不达标 → low_coverage（检索回来了但只沾到边，该调召回）
//	③ 向量相关度：最高分是否达标
//	   不达标 → low_score（词面沾边但语义相关度低，该换向量模型或调阈值）
//
// 为什么必须引入词面判据：单看向量分数无法区分「知识没覆盖」与「检索没召回」。
// 实测发现离线 bigram 向量化器存在偶然哈希碰撞，无关提问也能拿到 0.16 的相似度，
// 于是「知识库没有这个话题」会被误报成「相关度偏低」，改进方向随之被误导。
// 词面零命中是一个廉价且可靠的「根本没这条知识」信号。
func (r *Retriever) gate(query string, hits []Hit) GateResult {
	if len(hits) == 0 {
		return GateResult{
			Reason:  ReasonNoHit,
			Message: "知识库中未找到相关内容（可能是知识未覆盖该问题）",
		}
	}

	coverage := r.coverage(query, hits)
	// 覆盖率低于地板值即视为「无实质交集」。
	//
	// 为什么不能只判 coverage == 0：中文里「流程」「材料」「问题」这类泛词
	// 会偶然出现在无关语料中，实测一个完全无关的提问也能拿到 9% 覆盖率。
	// 9% 与 0% 在语义上等价——都代表「知识库没有这条知识」，
	// 若归因为 low_coverage 会把改进方向误导到「调召回」，而实际该「补知识」。
	if coverage < coverageFloor(r.options) {
		return GateResult{
			Reason:   ReasonNoHit,
			Coverage: coverage,
			Message: fmt.Sprintf("检索结果与提问几乎无词面交集（覆盖率 %.0f%%），判定为知识库未覆盖该问题",
				coverage*100),
		}
	}

	top := hits[0].Score
	if coverage < r.options.MinCoverage {
		return GateResult{
			Reason:   ReasonLowCoverage,
			Coverage: coverage,
			Message: fmt.Sprintf("命中内容未覆盖提问关键信息（覆盖率 %.0f%%，下限 %.0f%%）",
				coverage*100, r.options.MinCoverage*100),
		}
	}
	if top < r.options.ScoreThreshold {
		return GateResult{
			Reason:   ReasonLowScore,
			Coverage: coverage,
			Message: fmt.Sprintf("相关内容相关度过低（最高 %.2f，阈值 %.2f），不足以作答",
				top, r.options.ScoreThreshold),
		}
	}

	return GateResult{
		Sufficient: true,
		Reason:     ReasonSufficient,
		Coverage:   coverage,
		Message: fmt.Sprintf("证据充分：命中 %d 条，最高相关度 %.2f，关键词覆盖 %.0f%%",
			len(hits), top, coverage*100),
	}
}

// coverageFloor 返回「无实质交集」的覆盖率地板值。
//
// 取 MinCoverage 的一半：与其保持固定比例，使两者不会互相矛盾
// （地板永远低于判定下限），调参时也不必同时改两个常数。
func coverageFloor(options Options) float64 {
	floor := options.MinCoverage / 2
	if floor < 0.02 {
		floor = 0.02
	}
	return floor
}

// buildContext 拼接进入模型上下文的证据文本。
//
// 同时限制总条数与单文档条数：只限总条数时，一篇长文档可能占满全部名额，
// 其他文档的证据被挤掉，模型因此只看到一个来源。
func (r *Retriever) buildContext(hits []Hit) string {
	perDoc := make(map[string]int)
	picked := make([]Hit, 0, r.options.MaxContextItems)

	for _, hit := range hits {
		if len(picked) >= r.options.MaxContextItems {
			break
		}
		if perDoc[hit.Chunk.DocID] >= r.options.MaxPerDoc {
			continue
		}
		perDoc[hit.Chunk.DocID]++
		picked = append(picked, hit)
	}

	var b strings.Builder
	for i, hit := range picked {
		// 带上 DocID：既让模型知道证据来源可区分，也让配额逻辑可被测试直接断言，
		// 而不必依赖标题文本匹配（标题重复或改动都会让断言失效）。
		fmt.Fprintf(&b, "【证据 %d｜文档 %s｜%s｜相关度 %.2f】\n%s\n\n",
			i+1, hit.Chunk.DocID, hit.Chunk.Title, hit.Score, strings.TrimSpace(hit.Chunk.Content))
	}
	return strings.TrimSpace(b.String())
}

// CoverageReporter 可选能力：按词项区分度返回覆盖率。
//
// 稀疏检索器（BM25）实现该接口，使覆盖率不把「鉴权」与「什么」同等看待。
type CoverageReporter interface {
	WeightedCoverage(query string, documents []string) (float64, []string)
}

// coverage 计算提问被命中片段覆盖的程度。
//
// 优先使用检索器提供的 IDF 加权覆盖率；不支持时退回朴素词面覆盖率。
func (r *Retriever) coverage(query string, hits []Hit) float64 {
	if reporter, ok := r.scorer.(CoverageReporter); ok {
		documents := make([]string, 0, len(hits))
		for _, hit := range hits {
			documents = append(documents, hit.Chunk.titleAndContent())
		}
		coverage, _ := reporter.WeightedCoverage(query, documents)
		return coverage
	}
	return keywordCoverage(query, hits)
}

// keywordCoverage 计算提问关键词被命中片段覆盖的比例。
//
// 用字符 bigram 而非分词：中文分词需要词典，而 bigram 无需外部依赖
// 且对本场景足够——目的是判断"命中了没有"，不是精确语义匹配。
// 已被高相关度片段覆盖的比例即为该指标。
func keywordCoverage(query string, hits []Hit) float64 {
	terms := bigrams(query)
	if len(terms) == 0 {
		return 1 // 无可比对的关键词，不因此判负
	}
	corpus := make([]string, 0, len(hits))
	for _, hit := range hits {
		corpus = append(corpus, hit.Chunk.titleAndContent())
	}
	joined := strings.ToLower(strings.Join(corpus, " "))

	covered := 0
	for term := range terms {
		if strings.Contains(joined, term) {
			covered++
		}
	}
	return float64(covered) / float64(len(terms))
}

// bigrams 提取字符串的字符级二元组，忽略空白与标点。
func bigrams(text string) map[string]struct{} {
	runes := make([]rune, 0, len(text))
	for _, r := range strings.ToLower(text) {
		if isIgnorable(r) {
			continue
		}
		runes = append(runes, r)
	}
	ret := make(map[string]struct{})
	if len(runes) == 1 {
		ret[string(runes)] = struct{}{}
		return ret
	}
	for i := 0; i+1 < len(runes); i++ {
		ret[string(runes[i:i+2])] = struct{}{}
	}
	return ret
}

// isIgnorable 判断字符是否应被忽略（空白与常见中英标点）。
func isIgnorable(r rune) bool {
	if r <= ' ' {
		return true
	}
	switch r {
	case '，', '。', '、', '；', '：', '？', '！', '“', '”', '‘', '’',
		'（', '）', '《', '》', '【', '】', ',', '.', ';', ':', '?', '!',
		'"', '\'', '(', ')', '[', ']', '<', '>', '/', '\\', '|', '-', '_':
		return true
	}
	return false
}

// cosineSimilarity 计算余弦相似度。
//
// 向量维度不一致时返回 0 而非报错：维度不匹配说明该片段无法比较，
// 跳过它比让整次检索失败更合理。
func cosineSimilarity(left, right []float64) float64 {
	if len(left) == 0 || len(left) != len(right) {
		return 0
	}
	var dot, normLeft, normRight float64
	for i := range left {
		dot += left[i] * right[i]
		normLeft += left[i] * left[i]
		normRight += right[i] * right[i]
	}
	if normLeft == 0 || normRight == 0 {
		return 0
	}
	return dot / (math.Sqrt(normLeft) * math.Sqrt(normRight))
}

// Embedder 向量化接口。
//
// 抽象为接口是为了让检索逻辑可在无外部服务的情况下测试：
// 生产用模型 embedding，测试与演示用确定性的离线实现。
type Embedder interface {
	Embed(text string) []float64
	// Dimension 返回向量维度，供一致性校验与文档使用。
	Dimension() int
}

func (c Chunk) titleAndContent() string {
	parts := []string{c.Title, c.Content}
	if len(c.Keywords) > 0 {
		parts = append(parts, strings.Join(c.Keywords, " "))
	}
	return strings.Join(parts, " ")
}

// ErrNoEmbedder 在缺少向量化实现时返回。
var ErrNoEmbedder = errors.New("未配置向量化实现")
