package rag

import (
	"math"
	"sort"
	"strings"
)

// BM25Embedder 是基于 BM25 词权重的稀疏检索实现。
//
// 为什么不用「字符哈希投影」：那只把 bigram 随机投影到固定维度，
// 没有检索语义——无关文本也会因哈希碰撞拿到非零相似度，
// 实测导致模型不信任检索结果并反复改写查询重试，单轮 prompt 涨到近万 token。
//
// 为什么用 BM25 而不是模型向量化：
//   - 企业客服知识库以词面重叠为主（用户问"接口401"，文档写"接口返回401"），
//     BM25 在这类场景表现接近甚至优于小模型的稠密向量。
//   - 不需要任何外部服务与 API Key，离线评测与测试完全可复现。
//   - 打分可解释：每个片段的分数来自具体词项权重，便于归因。
//
// 局限：无法处理同义不同词（"登不上" vs "无法登录"）。生产环境应换成
// 模型向量化；Embedder 接口使这一替换不影响检索、门控与工具层。
type BM25Embedder struct {
	// k1 控制词频饱和度：出现多次的词权重增益递减。
	k1 float64
	// b 控制文档长度归一化：长文档不应仅因更长就得分更高。
	b float64
	// vocab 是词项到维度的映射，用于产出稀疏向量。
	vocab map[string]int
	// idf 记录每个词项的逆文档频率。
	idf map[string]float64
	// docFreq 记录词项出现的文档数。
	docFreq map[string]int
	// avgLen 为平均文档长度（词项数）。
	avgLen float64
	// total 为文档总数。
	total int
}

// BM25 参数默认值。k1=1.2、b=0.75 是信息检索领域的常用取值。
const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

// NewBM25Embedder 基于语料构建 BM25 检索器。
//
// 必须先喂入全部文档再检索：IDF 依赖全语料的文档频率统计。
// 这也是与通用 Embedder 接口的差异——稀疏检索需要语料统计，
// 因此提供 FromCorpus 构造入口而不是单文本 Embed。
func NewBM25Embedder(documents []string) *BM25Embedder {
	ret := &BM25Embedder{
		k1:      bm25K1,
		b:       bm25B,
		vocab:   make(map[string]int),
		idf:     make(map[string]float64),
		docFreq: make(map[string]int),
	}
	if len(documents) == 0 {
		return ret
	}
	ret.total = len(documents)

	var totalLen int
	for _, doc := range documents {
		terms := tokenize(doc)
		totalLen += len(terms)
		seen := make(map[string]struct{}, len(terms))
		for _, term := range terms {
			if _, ok := ret.vocab[term]; !ok {
				ret.vocab[term] = len(ret.vocab)
			}
			// 文档频率按「出现该词的文档数」计，同一文档内重复只算一次。
			if _, ok := seen[term]; ok {
				continue
			}
			seen[term] = struct{}{}
			ret.docFreq[term]++
		}
	}
	ret.avgLen = float64(totalLen) / float64(ret.total)

	// IDF 用带平滑的形式，避免出现负值：
	// 词项出现在超过半数文档时，标准 BM25 的 idf 会变成负数，
	// 导致「含该词的文档得分反而更低」这种反直觉行为。
	for term, freq := range ret.docFreq {
		ratio := (float64(ret.total) - float64(freq) + 0.5) / (float64(freq) + 0.5)
		ret.idf[term] = math.Log(1 + math.Max(ratio, 0))
	}
	return ret
}

// Dimension 返回词表大小。
func (e *BM25Embedder) Dimension() int { return len(e.vocab) }

// Embed 把文本编码为词频向量。
//
// 返回的向量在检索阶段会被 BM25 公式重新加权；
// 此处只承载词频，不作为最终分值。
func (e *BM25Embedder) Embed(text string) []float64 {
	if len(e.vocab) == 0 {
		return nil
	}
	vector := make([]float64, len(e.vocab))
	for _, term := range tokenize(text) {
		if index, ok := e.vocab[term]; ok {
			vector[index]++
		}
	}
	return vector
}

// Score 返回归一化到 0..1 的 BM25 相关度。
//
// ⚠️ 该归一化分数**不适合作为门控判据**，仅用于排序与展示。
// 实测发现它把两类查询的信号洗掉了：可回答查询的中位数 0.44，
// 不可回答查询 0.46——几乎完全相同，据此设阈值只能二选一地
// 牺牲召回或容忍假命中。
//
// 原因：除以「该查询的理论满分」后，短查询会因分母小而虚高，
// 而无关的短查询恰恰就是短查询。门控应改用绝对判据：
// 命中词项数（CoverageReporter.InVocabTermCount）与原始分数（RawScore）。
func (e *BM25Embedder) Score(query string, document string) float64 {
	raw := e.rawScore(query, document)
	if raw <= 0 {
		return 0
	}
	ceiling := e.maxScore(query)
	if ceiling <= 0 {
		return 0
	}
	if normalized := raw / ceiling; normalized <= 1 {
		return normalized
	}
	return 1
}

// maxScore 返回该查询在当前语料下的理论最高分。
func (e *BM25Embedder) maxScore(query string) float64 {
	if e.total == 0 {
		return 0
	}
	var ceiling float64
	seen := make(map[string]struct{})
	for _, term := range tokenize(query) {
		if _, ok := seen[term]; ok {
			continue
		}
		seen[term] = struct{}{}

		idf, ok := e.idf[term]
		if !ok {
			continue
		}
		// 文档长度恰为平均长度时，长度归一化项为 1；
		// 词频趋于饱和时，tf 部分趋近 (k1+1)。
		ceiling += idf * (e.k1 + 1)
	}
	return ceiling
}

// rawScore 计算未归一化的 BM25 原始分数。
func (e *BM25Embedder) rawScore(query string, document string) float64 {
	if e.total == 0 {
		return 0
	}
	docTerms := tokenize(document)
	if len(docTerms) == 0 {
		return 0
	}

	docFreq := make(map[string]int, len(docTerms))
	for _, term := range docTerms {
		docFreq[term]++
	}
	docLen := float64(len(docTerms))

	var score float64
	seen := make(map[string]struct{})
	for _, term := range tokenize(query) {
		// 同一词项在查询里重复出现不重复计分：查询侧不做词频加权，
		// 否则用户复述同一关键词会异常放大该项权重。
		if _, ok := seen[term]; ok {
			continue
		}
		seen[term] = struct{}{}

		idf, ok := e.idf[term]
		if !ok {
			continue // 词项不在词表中，对全语料无区分力
		}
		freq := float64(docFreq[term])
		if freq == 0 {
			continue
		}
		numerator := freq * (e.k1 + 1)
		denominator := freq + e.k1*(1-e.b+e.b*docLen/e.avgLen)
		score += idf * numerator / denominator
	}
	return score
}

// tokenize 把文本切分为检索词项。
//
// 中文用字符 bigram，英文与数字用整词。理由：
//   - 中文没有词边界，引入分词词典会带来依赖与词典维护成本；
//     bigram 对本场景（短查询、词面重叠）足够，且无需外部资源。
//   - 英文/数字若也切 bigram，会把 "401" 切成 "40"、"01"，反而引入噪声。
func tokenize(text string) []string {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return nil
	}

	var terms []string
	var asciiRun []rune
	var cjkRun []rune

	flushASCII := func() {
		if len(asciiRun) > 0 {
			terms = append(terms, string(asciiRun))
			asciiRun = asciiRun[:0]
		}
	}
	flushCJK := func() {
		if len(cjkRun) == 1 {
			terms = append(terms, string(cjkRun))
		} else {
			for i := 0; i+1 < len(cjkRun); i++ {
				terms = append(terms, string(cjkRun[i:i+2]))
			}
		}
		cjkRun = cjkRun[:0]
	}

	for _, r := range text {
		switch {
		case isASCIIAlnum(r):
			flushCJK()
			asciiRun = append(asciiRun, r)
		case isCJK(r):
			flushASCII()
			cjkRun = append(cjkRun, r)
		default:
			// 标点与空白作为分隔符。
			flushASCII()
			flushCJK()
		}
	}
	flushASCII()
	flushCJK()
	return terms
}

// isASCIIAlnum 判断是否为 ASCII 字母或数字。
func isASCIIAlnum(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	default:
		return false
	}
}

// isCJK 判断是否为中日韩表意文字。
func isCJK(r rune) bool {
	switch {
	case r >= 0x4E00 && r <= 0x9FFF: // 基本区
		return true
	case r >= 0x3400 && r <= 0x4DBF: // 扩展 A
		return true
	case r >= 0xF900 && r <= 0xFAFF: // 兼容表意文字
		return true
	default:
		return false
	}
}

// WeightedCoverage 返回查询词项在语料中的加权覆盖率，以及命中的词项。
//
// 两条关键设计，都来自实测踩坑：
//
//  1. **词表外词项不进入分母。** 中文 bigram 会把「接口老是超时是什么原因」
//     切成「老是」「是什」「什么」等口语填充词。这些词从未出现在知识库中，
//     无法判断其信息量，把它们计入分母只会稀释覆盖率——实测导致正确文档
//     被判为「证据不足」，模型因此反复改写查询重试，单轮 prompt 涨到近万 token。
//     覆盖率衡量的是「知识库里确实存在的概念被覆盖了多少」，因此只统计词表内词项。
//
//  2. **按 IDF 加权。** 命中「鉴权」与命中「接口」的信息量不同，
//     不加权会让泛词主导判定。
func (e *BM25Embedder) WeightedCoverage(query string, documents []string) (float64, []string) {
	if len(documents) == 0 {
		return 0, nil
	}
	corpus := strings.ToLower(strings.Join(documents, " "))

	var totalWeight, matchedWeight float64
	var matched []string
	seen := make(map[string]struct{})
	for _, term := range tokenize(query) {
		if _, ok := seen[term]; ok {
			continue
		}
		seen[term] = struct{}{}

		idf, inVocab := e.idf[term]
		if !inVocab {
			// 知识库中从未出现的词项：不参与覆盖率计算。
			continue
		}
		totalWeight += idf
		if strings.Contains(corpus, term) {
			matchedWeight += idf
			matched = append(matched, term)
		}
	}
	// 查询中没有任何词项出现在知识库里 —— 这是「知识库未覆盖」的强信号。
	if totalWeight == 0 {
		return 0, nil
	}
	return matchedWeight / totalWeight, matched
}

// InVocabTermCount 返回查询中出现在语料词表内的词项数量。
//
// 用途：作为「证据强度」的绝对判据。覆盖率是比例指标，对短查询会失真——
// 无关查询中落在词表内的词项本就很少，匹配上其中一个就可能得到 100% 覆盖率，
// 从而把门控抬过线（实测假命中率达 70%）。真实证据应当有多个词项同时命中。
func (e *BM25Embedder) InVocabTermCount(query string) int {
	seen := make(map[string]struct{})
	for _, term := range tokenize(query) {
		if _, ok := e.idf[term]; !ok {
			continue
		}
		seen[term] = struct{}{}
	}
	return len(seen)
}

// RawScore 暴露原始 BM25 分数，供调试与对比使用。
func (e *BM25Embedder) RawScore(query string, document string) float64 {
	return e.rawScore(query, document)
}

// RankBM25 按 BM25 得分对文档排序，返回索引与分数。
//
// 平局按索引升序收敛，保证同一查询每次得到相同排序。
func (e *BM25Embedder) RankBM25(query string, documents []string) []ScoredIndex {
	ret := make([]ScoredIndex, 0, len(documents))
	for i, doc := range documents {
		score := e.Score(query, doc)
		if score <= 0 {
			continue
		}
		ret = append(ret, ScoredIndex{Index: i, Score: score})
	}
	sort.SliceStable(ret, func(i, j int) bool {
		if ret[i].Score != ret[j].Score {
			return ret[i].Score > ret[j].Score
		}
		return ret[i].Index < ret[j].Index
	})
	return ret
}

// ScoredIndex 一个带分数的文档索引。
type ScoredIndex struct {
	Index int
	Score float64
}

var _ Embedder = (*BM25Embedder)(nil)
