package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// RetrievalChunk 一个知识片段。
type RetrievalChunk struct {
	ID      string `json:"id"`
	DocID   string `json:"docId"`
	Title   string `json:"title"`
	Content string `json:"content"`
}

// RetrievalQuery 一条检索评测查询。
type RetrievalQuery struct {
	ID string `json:"id"`
	// Text 为真实用户提问。
	Text string `json:"text"`
	// RelevantDocIDs 为该提问的正确来源文档。
	//
	// 以文档而非片段为粒度：同一文档可能被切成多个片段，
	// 命中其中任一片段即算检索成功。用片段粒度会把「分块策略差异」
	// 误算成「检索失败」。
	RelevantDocIDs []string `json:"relevantDocIds"`
	Note           string   `json:"note,omitempty"`
}

// Answerable 判断该查询是否可被知识库回答。
func (q RetrievalQuery) Answerable() bool { return len(q.RelevantDocIDs) > 0 }

// RetrievalDataset 检索评测集。
type RetrievalDataset struct {
	Version     int              `json:"version"`
	Description string           `json:"description"`
	Corpus      []RetrievalChunk `json:"corpus"`
	Queries     []RetrievalQuery `json:"queries"`
}

// LoadRetrievalDataset 读取检索评测集。
func LoadRetrievalDataset(path string) (*RetrievalDataset, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取评测集 %s: %w", path, err)
	}
	var dataset RetrievalDataset
	if err := json.Unmarshal(data, &dataset); err != nil {
		return nil, fmt.Errorf("解析评测集 %s: %w", path, err)
	}
	if len(dataset.Corpus) == 0 {
		return nil, fmt.Errorf("评测集 %s 的语料为空", path)
	}
	if len(dataset.Queries) == 0 {
		return nil, fmt.Errorf("评测集 %s 不含任何查询", path)
	}
	return &dataset, nil
}

// Validate 校验数据集自身一致性。
//
// 评测前先校验，避免用有问题的数据跑出误导性指标。
// 尤其是 relevantDocIds 指向不存在的文档时，召回率会永远为 0，
// 而那看起来像是「检索效果差」，实际是数据错。
func (d *RetrievalDataset) Validate() error {
	knownDocs := make(map[string]struct{}, len(d.Corpus))
	knownChunks := make(map[string]struct{}, len(d.Corpus))
	for _, chunk := range d.Corpus {
		if strings.TrimSpace(chunk.ID) == "" {
			return fmt.Errorf("存在缺失 ID 的片段")
		}
		if _, ok := knownChunks[chunk.ID]; ok {
			return fmt.Errorf("片段 ID 重复: %s", chunk.ID)
		}
		knownChunks[chunk.ID] = struct{}{}
		if strings.TrimSpace(chunk.DocID) == "" {
			return fmt.Errorf("%s: 缺少 docId", chunk.ID)
		}
		if strings.TrimSpace(chunk.Content) == "" {
			return fmt.Errorf("%s: 内容为空", chunk.ID)
		}
		knownDocs[chunk.DocID] = struct{}{}
	}

	seenQueries := make(map[string]struct{}, len(d.Queries))
	unanswerable := 0
	for _, query := range d.Queries {
		if strings.TrimSpace(query.ID) == "" {
			return fmt.Errorf("存在缺失 ID 的查询")
		}
		if _, ok := seenQueries[query.ID]; ok {
			return fmt.Errorf("查询 ID 重复: %s", query.ID)
		}
		seenQueries[query.ID] = struct{}{}
		if strings.TrimSpace(query.Text) == "" {
			return fmt.Errorf("%s: 查询文本为空", query.ID)
		}
		for _, docID := range query.RelevantDocIDs {
			if _, ok := knownDocs[docID]; !ok {
				return fmt.Errorf("%s: 引用不存在的文档 %q", query.ID, docID)
			}
		}
		if !query.Answerable() {
			unanswerable++
		}
	}
	if unanswerable == 0 {
		return fmt.Errorf("缺少不可回答的查询：没有负样本就无法检验「假命中」")
	}
	return nil
}

// RetrievalHit 一次检索命中。
type RetrievalHit struct {
	DocID string
	Chunk string
	Score float64
}

// RetrievalResult 一次检索的观测结果。
type RetrievalResult struct {
	Hits []RetrievalHit
	// DecidedHit 为检索器自己的判定：是否认为找到了可用证据。
	// 与「实际是否命中金标」分开，用于计算假阳性与假阴性。
	DecidedHit bool
	// Reason 为检索器的失败归因，用于按原因分桶。
	Reason string
}

// Retriever 可被评测的检索器。
type Retriever interface {
	RetrieveForEval(query string, topK int) (RetrievalResult, error)
}

// RetrieverFunc 便于用函数构造。
type RetrieverFunc func(query string, topK int) (RetrievalResult, error)

// RetrieveForEval 实现 Retriever。
func (f RetrieverFunc) RetrieveForEval(query string, topK int) (RetrievalResult, error) {
	return f(query, topK)
}

// RetrievalCaseResult 单条查询的结果。
type RetrievalCaseResult struct {
	QueryID    string   `json:"queryId"`
	Text       string   `json:"text"`
	Answerable bool     `json:"answerable"`
	Relevant   []string `json:"relevantDocIds,omitempty"`
	Retrieved  []string `json:"retrievedDocIds"`
	// FirstRank 为第一个命中相关文档的排名（1 起）；未命中为 0。
	FirstRank int  `json:"firstRank"`
	HitAtK    bool `json:"hitAtK"`
	// DecidedHit 为检索器的自我判定。
	DecidedHit bool   `json:"decidedHit"`
	Reason     string `json:"reason,omitempty"`
	Error      string `json:"error,omitempty"`
}

// RetrievalReport 检索评测报告。
type RetrievalReport struct {
	Total int `json:"total"`
	// K 本次评测的截断位置。
	K int `json:"k"`

	// 召回类指标（分母为可回答查询数）
	Answerable int     `json:"answerable"`
	Hits       int     `json:"hits"`
	RecallAtK  float64 `json:"recallAtK"`
	// MRR 首个相关文档排名倒数的均值。
	MRR float64 `json:"mrr"`
	// PrecisionAtK 取回的文档中相关文档的比例（按文档去重后统计）。
	//
	// 与召回率互补：召回率看「该找到的找到了多少」，精确率看「找到的有多少是对的」。
	// 两者同时偏低时，哪一项更低决定了改进方向（补检索 vs 抑制噪声）。
	PrecisionAtK float64 `json:"precisionAtK"`

	// 置信类指标（分母为不可回答查询数）
	Unanswerable int `json:"unanswerable"`
	// FalsePositiveRate 不可回答的查询被检索器判为「有证据」的比例。
	//
	// 这是「假命中率」：检索器不该对知识库没有的问题表现自信，
	// 否则模型会基于无关片段作答。比召回率更隐蔽，也更容易造成错误回答。
	FalsePositiveRate float64 `json:"falsePositiveRate"`

	// 判定的混淆矩阵（以「检索器是否有证据」× 「是否真的可回答」计）
	DecidedHitOnAnswerable   int `json:"decidedHitOnAnswerable"`
	DecidedHitOnUnanswerable int `json:"decidedHitOnUnanswerable"`
	MissedOnAnswerable       int `json:"missedOnAnswerable"`

	// ByReason 按检索器给出的失败原因分桶。
	ByReason map[string]int `json:"byReason"`

	Errors  []RetrievalCaseResult `json:"errors,omitempty"`
	Results []RetrievalCaseResult `json:"results,omitempty"`
}

// RunRetrievalEval 执行检索评测。
//
// 指标刻意分成两组：召回类（可回答的查询有没有找到）与
// 置信类（不可回答的查询有没有被误判为有证据）。
// 只看召回率会漏掉后者，而假命中直接导致模型基于无关内容作答，
// 是比漏召回更隐蔽的错误。
func RunRetrievalEval(dataset *RetrievalDataset, retriever Retriever, topK int) RetrievalReport {
	if topK <= 0 {
		topK = 5
	}
	report := RetrievalReport{
		K:        topK,
		Total:    len(dataset.Queries),
		ByReason: make(map[string]int),
		Results:  make([]RetrievalCaseResult, 0, len(dataset.Queries)),
	}

	var mrrSum float64
	var precisionSum float64

	for _, query := range dataset.Queries {
		result := RetrievalCaseResult{
			QueryID:    query.ID,
			Text:       query.Text,
			Answerable: query.Answerable(),
			Relevant:   query.RelevantDocIDs,
		}

		observation, err := retriever.RetrieveForEval(query.Text, topK)
		if err != nil {
			result.Error = err.Error()
			report.Errors = append(report.Errors, result)
			report.Results = append(report.Results, result)
			continue
		}

		result.DecidedHit = observation.DecidedHit
		result.Reason = observation.Reason
		for _, hit := range observation.Hits {
			result.Retrieved = append(result.Retrieved, hit.DocID)
		}
		if observation.Reason != "" {
			report.ByReason[observation.Reason]++
		}

		if query.Answerable() {
			report.Answerable++
			rank := firstRelevantRank(result.Retrieved, query.RelevantDocIDs)
			result.FirstRank = rank
			result.HitAtK = rank > 0
			precisionSum += PrecisionAtK(result.Retrieved, query.RelevantDocIDs)
			if result.HitAtK {
				report.Hits++
				mrrSum += 1 / float64(rank)
			} else {
				report.MissedOnAnswerable++
				report.Errors = append(report.Errors, result)
			}
			if observation.DecidedHit {
				report.DecidedHitOnAnswerable++
			}
		} else {
			report.Unanswerable++
			if observation.DecidedHit {
				// 假命中：知识库没有这个主题，检索器却认为有证据。
				report.DecidedHitOnUnanswerable++
				report.Errors = append(report.Errors, result)
			}
		}

		report.Results = append(report.Results, result)
	}

	if report.Answerable > 0 {
		report.RecallAtK = float64(report.Hits) / float64(report.Answerable)
		report.MRR = mrrSum / float64(report.Answerable)
		report.PrecisionAtK = precisionSum / float64(report.Answerable)
	}
	if report.Unanswerable > 0 {
		report.FalsePositiveRate = float64(report.DecidedHitOnUnanswerable) / float64(report.Unanswerable)
	}
	return report
}

// firstRelevantRank 返回第一个相关文档的排名（1 起），未命中返回 0。
//
// 按文档去重后计算：同一文档的多个片段不应重复占用排名位次，
// 否则 MRR 会被同一来源的多个片段人为抬高。
func firstRelevantRank(retrieved []string, relevant []string) int {
	wanted := make(map[string]struct{}, len(relevant))
	for _, docID := range relevant {
		wanted[docID] = struct{}{}
	}
	seen := make(map[string]struct{}, len(retrieved))
	rank := 0
	for _, docID := range retrieved {
		if _, ok := seen[docID]; ok {
			continue
		}
		seen[docID] = struct{}{}
		rank++
		if _, ok := wanted[docID]; ok {
			return rank
		}
	}
	return 0
}

// Text 渲染人读报告。
func (r RetrievalReport) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "检索评测报告\n")
	fmt.Fprintf(&b, "%s\n", strings.Repeat("=", 62))
	fmt.Fprintf(&b, "查询总数        %d（截断 K=%d）\n", r.Total, r.K)
	fmt.Fprintf(&b, "可回答          %d\n", r.Answerable)
	fmt.Fprintf(&b, "不可回答        %d\n", r.Unanswerable)

	fmt.Fprintf(&b, "\n召回指标（分母为可回答查询）\n%s\n", strings.Repeat("-", 62))
	fmt.Fprintf(&b, "命中数          %d\n", r.Hits)
	fmt.Fprintf(&b, "Recall@%d        %.2f%%\n", r.K, r.RecallAtK*100)
	fmt.Fprintf(&b, "MRR             %.4f\n", r.MRR)
	fmt.Fprintf(&b, "Precision@%d     %.2f%%\n", r.K, r.PrecisionAtK*100)

	fmt.Fprintf(&b, "\n置信指标（分母为不可回答查询）\n%s\n", strings.Repeat("-", 62))
	fmt.Fprintf(&b, "假命中率        %.2f%%\n", r.FalsePositiveRate*100)
	fmt.Fprintf(&b, "  可回答且有证据    %d/%d\n", r.DecidedHitOnAnswerable, r.Answerable)
	fmt.Fprintf(&b, "  可回答但漏召回    %d/%d\n", r.MissedOnAnswerable, r.Answerable)
	fmt.Fprintf(&b, "  不可回答却假命中  %d/%d\n", r.DecidedHitOnUnanswerable, r.Unanswerable)

	if len(r.ByReason) > 0 {
		fmt.Fprintf(&b, "\n按归因分桶\n%s\n", strings.Repeat("-", 62))
		reasons := make([]string, 0, len(r.ByReason))
		for reason := range r.ByReason {
			reasons = append(reasons, reason)
		}
		sort.Strings(reasons)
		for _, reason := range reasons {
			fmt.Fprintf(&b, "  %-18s %d\n", reason, r.ByReason[reason])
		}
	}

	if len(r.Errors) > 0 {
		fmt.Fprintf(&b, "\n问题样本\n%s\n", strings.Repeat("-", 62))
		for i, item := range r.Errors {
			if i >= 15 {
				fmt.Fprintf(&b, "... 另有 %d 条未列出\n", len(r.Errors)-15)
				break
			}
			switch {
			case item.Error != "":
				fmt.Fprintf(&b, "%s [执行失败] %s\n", item.QueryID, item.Error)
			case !item.Answerable:
				fmt.Fprintf(&b, "%s [假命中] 知识库无此主题，检索器却判为有证据\n", item.QueryID)
				fmt.Fprintf(&b, "     %s\n", truncateText(item.Text, 40))
			default:
				fmt.Fprintf(&b, "%s [漏召回] 期望 %v，实际 %v\n",
					item.QueryID, item.Relevant, item.Retrieved)
				fmt.Fprintf(&b, "     %s\n", truncateText(item.Text, 40))
			}
		}
	}
	return b.String()
}

// PrecisionAtK 计算精确率：取回的文档中相关文档的比例。
//
// 与召回率互补：召回率看「该找到的找到了多少」，
// 精确率看「找到的有多少是对的」。检索质量差时二者会同时下降，
// 但哪一项更低决定了改进方向（补检索 vs 抑制噪声）。
func PrecisionAtK(retrieved []string, relevant []string) float64 {
	if len(retrieved) == 0 {
		return 0
	}
	wanted := make(map[string]struct{}, len(relevant))
	for _, docID := range relevant {
		wanted[docID] = struct{}{}
	}
	seen := make(map[string]struct{}, len(retrieved))
	var hit, total int
	for _, docID := range retrieved {
		if _, ok := seen[docID]; ok {
			continue
		}
		seen[docID] = struct{}{}
		total++
		if _, ok := wanted[docID]; ok {
			hit++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(hit) / float64(total)
}
