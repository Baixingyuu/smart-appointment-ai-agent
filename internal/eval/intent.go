package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/mac/helpdesk-agent/internal/domain"
)

// IntentCase 一条意图评测样本。
type IntentCase struct {
	ID     string        `json:"id"`
	Text   string        `json:"text"`
	Intent domain.Intent `json:"intent"`
	// Note 记录歧义点，便于人工复核标注是否合理。
	Note string `json:"note,omitempty"`
}

// IntentDataset 意图评测集。
type IntentDataset struct {
	Version     int               `json:"version"`
	Description string            `json:"description"`
	Taxonomy    map[string]string `json:"taxonomy,omitempty"`
	Cases       []IntentCase      `json:"cases"`
}

// LoadIntentDataset 读取意图评测集。
func LoadIntentDataset(path string) (*IntentDataset, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取评测集 %s: %w", path, err)
	}
	var dataset IntentDataset
	if err := json.Unmarshal(data, &dataset); err != nil {
		return nil, fmt.Errorf("解析评测集 %s: %w", path, err)
	}
	if len(dataset.Cases) == 0 {
		return nil, fmt.Errorf("评测集 %s 不含任何样本", path)
	}
	return &dataset, nil
}

// Validate 校验评测集自身的一致性。
//
// 在评测前先校验数据集，避免用一份有问题的数据跑出误导性指标。
func (d *IntentDataset) Validate() error {
	if len(d.Cases) == 0 {
		return fmt.Errorf("评测集为空")
	}
	seen := make(map[string]struct{}, len(d.Cases))
	for _, item := range d.Cases {
		if strings.TrimSpace(item.ID) == "" {
			return fmt.Errorf("存在缺失 ID 的样本")
		}
		if _, ok := seen[item.ID]; ok {
			return fmt.Errorf("样本 ID 重复: %s", item.ID)
		}
		seen[item.ID] = struct{}{}

		if strings.TrimSpace(item.Text) == "" {
			return fmt.Errorf("%s: 文本为空", item.ID)
		}
		if err := item.Intent.Validate(); err != nil {
			return fmt.Errorf("%s: %w", item.ID, err)
		}
	}
	return nil
}

// IntentPrediction 一次分类预测。
//
// 与观测分离：分类器只产出这些字段，评测框架据此判定，
// 使框架不依赖 classify 包的具体实现。
type IntentPrediction struct {
	Intent domain.Intent
	// Raw 为模型原始输出，解析失败时用于诊断。
	Raw string
	// Parsed 为 false 表示输出无法解析为已知意图。
	Parsed bool
	// PromptTokens / CompletionTokens 记录分类本身的成本，
	// 用于核算「加了分类层之后总成本是升还是降」。
	PromptTokens     int
	CompletionTokens int
}

// IntentClassifier 可被评测的分类器。
type IntentClassifier interface {
	ClassifyIntent(text string) (IntentPrediction, error)
}

// IntentClassifierFunc 便于用函数构造。
type IntentClassifierFunc func(text string) (IntentPrediction, error)

// ClassifyIntent 实现 IntentClassifier。
func (f IntentClassifierFunc) ClassifyIntent(text string) (IntentPrediction, error) {
	return f(text)
}

// IntentCaseResult 单条意图样本的结果。
type IntentCaseResult struct {
	CaseID  string        `json:"caseId"`
	Text    string        `json:"text"`
	Want    domain.Intent `json:"want"`
	Got     domain.Intent `json:"got"`
	Raw     string        `json:"raw,omitempty"`
	Parsed  bool          `json:"parsed"`
	Correct bool          `json:"correct"`
	Note    string        `json:"note,omitempty"`
	Error   string        `json:"error,omitempty"`
}

// IntentClassStat 单个类别的统计。
type IntentClassStat struct {
	Total     int     `json:"total"`
	Correct   int     `json:"correct"`
	Precision float64 `json:"precision"`
	Recall    float64 `json:"recall"`
	F1        float64 `json:"f1"`
}

// IntentReport 意图评测报告。
type IntentReport struct {
	Total int `json:"total"`
	// Correct 分类正确数。解析失败的样本不计入正确，
	// 即便回退值与标注恰好相同——那是回退成功而非分类正确，
	// 计入正确会让偶发故障被伪装成准确率。
	Correct int `json:"correct"`
	// Accuracy 分类正确率（分母为全部样本）。
	Accuracy float64 `json:"accuracy"`
	// Fallback 因输出无法解析而回退到默认意图的样本数。
	//
	// 单独统计的原因：回退会让这些样本「看起来正确」，
	// 而实际模型没有给出有效分类。它是提示词或输出稳定性的问题，
	// 与分类边界判断错误不是一回事。
	Fallback int `json:"fallback"`
	// MacroF1 各类别 F1 的算术平均，类别不平衡时比 Accuracy 更能反映问题。
	MacroF1 float64 `json:"macroF1"`
	// ParseRate 模型输出可被解析为已知意图的比例。
	//
	// 与 Accuracy 分开统计是刻意的：解析失败说明提示词或输出格式有问题，
	// 分类错误说明类别边界或模型理解有问题。两者的修复方向完全不同，
	// 混在一个数字里就无法定位。
	ParseRate float64 `json:"parseRate"`
	// ShortCircuitRate 可短路（无需进入工具循环）的样本比例，
	// 用于估算路由带来的成本节省空间。
	ShortCircuitRate float64 `json:"shortCircuitRate"`

	PromptTokens     int `json:"promptTokens"`
	CompletionTokens int `json:"completionTokens"`

	// ByClass 各类别的精确率/召回率/F1。
	ByClass map[string]IntentClassStat `json:"byClass"`
	// Confusion 混淆矩阵：Confusion[want][got] = 计数。
	Confusion map[string]map[string]int `json:"confusion"`

	Errors  []IntentCaseResult `json:"errors,omitempty"`
	Results []IntentCaseResult `json:"results,omitempty"`
}

// RunIntentEval 执行意图评测。
func RunIntentEval(dataset *IntentDataset, classifier IntentClassifier) IntentReport {
	report := IntentReport{
		Total:     len(dataset.Cases),
		ByClass:   make(map[string]IntentClassStat),
		Confusion: make(map[string]map[string]int),
		Results:   make([]IntentCaseResult, 0, len(dataset.Cases)),
	}

	// 初始化混淆矩阵，保证零值类别也出现在报告中——
	// 只在出现时创建会让报告缺行，跨版本对比时看不出差异。
	for _, want := range domain.AllIntents() {
		report.Confusion[string(want)] = make(map[string]int)
		for _, got := range domain.AllIntents() {
			report.Confusion[string(want)][string(got)] = 0
		}
		report.ByClass[string(want)] = IntentClassStat{}
	}

	shortCircuit := 0
	for _, item := range dataset.Cases {
		result := IntentCaseResult{CaseID: item.ID, Text: item.Text, Want: item.Intent, Note: item.Note}

		prediction, err := classifier.ClassifyIntent(item.Text)
		if err != nil {
			result.Error = err.Error()
			report.Errors = append(report.Errors, result)
			report.Results = append(report.Results, result)
			continue
		}

		result.Got = prediction.Intent
		result.Raw = prediction.Raw
		result.Parsed = prediction.Parsed
		// 解析失败即判为不正确：模型没有给出有效分类，
		// 回退值碰巧与标注相同属于巧合，不应计为能力。
		result.Correct = prediction.Parsed && prediction.Intent == item.Intent
		if !prediction.Parsed {
			report.Fallback++
		}

		report.PromptTokens += prediction.PromptTokens
		report.CompletionTokens += prediction.CompletionTokens
		if prediction.Parsed {
			report.ParseRate++
		}
		if !prediction.Intent.NeedsToolLoop() {
			shortCircuit++
		}
		if result.Correct {
			report.Correct++
		} else {
			report.Errors = append(report.Errors, result)
		}

		report.Confusion[string(item.Intent)][string(prediction.Intent)]++
		report.Results = append(report.Results, result)
	}

	if report.Total > 0 {
		report.Accuracy = float64(report.Correct) / float64(report.Total)
		report.ParseRate /= float64(report.Total)
		report.ShortCircuitRate = float64(shortCircuit) / float64(report.Total)
	}
	report.ByClass = computeClassStats(report.Confusion)
	report.MacroF1 = macroF1(report.ByClass)
	return report
}

// computeClassStats 由混淆矩阵计算各类的精确率/召回率/F1。
//
// 从混淆矩阵推导而非单独统计：单一数据源能保证
// 报告表格与混淆矩阵永远一致，避免两处统计口径漂移。
func computeClassStats(confusion map[string]map[string]int) map[string]IntentClassStat {
	ret := make(map[string]IntentClassStat, len(confusion))
	for _, intent := range domain.AllIntents() {
		label := string(intent)
		var truePositive, falsePositive, falseNegative int

		for want, row := range confusion {
			for got, count := range row {
				switch {
				case want == label && got == label:
					truePositive += count
				case want == label:
					falseNegative += count
				case got == label:
					falsePositive += count
				}
			}
		}

		stat := IntentClassStat{Total: truePositive + falseNegative, Correct: truePositive}
		if truePositive+falsePositive > 0 {
			stat.Precision = float64(truePositive) / float64(truePositive+falsePositive)
		}
		if truePositive+falseNegative > 0 {
			stat.Recall = float64(truePositive) / float64(truePositive+falseNegative)
		}
		if stat.Precision+stat.Recall > 0 {
			stat.F1 = 2 * stat.Precision * stat.Recall / (stat.Precision + stat.Recall)
		}
		ret[label] = stat
	}
	return ret
}

// macroF1 计算各类 F1 的算术平均。
//
// 只计入有样本的类别：把无样本类别的 F1=0 计进来会人为压低指标，
// 使不同数据集之间无法比较。
func macroF1(stats map[string]IntentClassStat) float64 {
	var sum float64
	var counted int
	for _, stat := range stats {
		if stat.Total == 0 {
			continue
		}
		sum += stat.F1
		counted++
	}
	if counted == 0 {
		return 0
	}
	return sum / float64(counted)
}

// Text 渲染人读报告。
func (r IntentReport) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "意图分类评测报告\n")
	fmt.Fprintf(&b, "%s\n", strings.Repeat("=", 62))
	fmt.Fprintf(&b, "样本总数        %d\n", r.Total)
	fmt.Fprintf(&b, "分类正确        %d\n", r.Correct)
	fmt.Fprintf(&b, "准确率          %.2f%%\n", r.Accuracy*100)
	fmt.Fprintf(&b, "Macro-F1        %.4f\n", r.MacroF1)
	fmt.Fprintf(&b, "解析成功率      %.2f%%\n", r.ParseRate*100)
	fmt.Fprintf(&b, "回退数          %d\n", r.Fallback)
	fmt.Fprintf(&b, "可短路比例      %.2f%%\n", r.ShortCircuitRate*100)
	fmt.Fprintf(&b, "分类 token      prompt=%d completion=%d\n", r.PromptTokens, r.CompletionTokens)

	fmt.Fprintf(&b, "\n按类别\n%s\n", strings.Repeat("-", 62))
	fmt.Fprintf(&b, "%-14s %6s %6s %8s %8s %8s\n", "类别", "样本", "正确", "精确率", "召回率", "F1")
	for _, intent := range domain.AllIntents() {
		stat := r.ByClass[string(intent)]
		if stat.Total == 0 {
			continue
		}
		fmt.Fprintf(&b, "%-14s %6d %6d %7.2f%% %7.2f%% %8.4f\n",
			intent, stat.Total, stat.Correct, stat.Precision*100, stat.Recall*100, stat.F1)
	}

	// 混淆矩阵只在有误判时输出：全对时它没有信息量，只是噪音。
	if hasConfusion := hasOffDiagonal(r.Confusion); hasConfusion {
		fmt.Fprintf(&b, "\n混淆矩阵（行=标注，列=预测，仅列出非零误判）\n%s\n", strings.Repeat("-", 62))
		labels := make([]string, 0, len(r.Confusion))
		for want := range r.Confusion {
			labels = append(labels, want)
		}
		sort.Strings(labels)
		for _, want := range labels {
			for got, count := range r.Confusion[want] {
				if want == got || count == 0 {
					continue
				}
				fmt.Fprintf(&b, "  %-14s → %-14s %d 条\n", want, got, count)
			}
		}
	}

	if len(r.Errors) > 0 {
		fmt.Fprintf(&b, "\n未正确分类样本\n%s\n", strings.Repeat("-", 62))
		for i, item := range r.Errors {
			if i >= 15 {
				fmt.Fprintf(&b, "... 另有 %d 条未列出\n", len(r.Errors)-15)
				break
			}
			if item.Error != "" {
				fmt.Fprintf(&b, "%s [执行失败] %s\n", item.CaseID, item.Error)
				continue
			}
			mark := ""
			if !item.Parsed {
				mark = "（输出无法解析）"
			}
			fmt.Fprintf(&b, "%s 标注=%-12s 预测=%-12s%s\n",
				item.CaseID, item.Want, item.Got, mark)
			fmt.Fprintf(&b, "     %s\n", truncateText(item.Text, 50))
			if item.Raw != "" && !item.Parsed {
				fmt.Fprintf(&b, "     原始输出: %s\n", truncateText(item.Raw, 40))
			}
		}
	}
	return b.String()
}

// hasOffDiagonal 判断混淆矩阵是否存在非对角线计数（即存在误判）。
func hasOffDiagonal(confusion map[string]map[string]int) bool {
	for want, row := range confusion {
		for got, count := range row {
			if want != got && count > 0 {
				return true
			}
		}
	}
	return false
}

func truncateText(text string, limit int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit]) + "…"
}
