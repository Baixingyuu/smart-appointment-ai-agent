package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mac/helpdesk-agent/internal/classify"
	"github.com/mac/helpdesk-agent/internal/domain"
	"github.com/mac/helpdesk-agent/internal/eval"
	"github.com/mac/helpdesk-agent/internal/evalrun"
	"github.com/mac/helpdesk-agent/internal/llm"
)

// runEvalIntent 跑意图分类评测。
//
// 两种模式：
//   - 真实模型（提供 -api-key）：产出可写入简历的真实准确率
//   - 离线规则（-offline）：关键词基线，零成本，用于对照「分类到底值不值一次模型调用」
//
// 为什么必须保留离线基线：如果规则基线的准确率已经接近模型，
// 那么为分类多付一次模型调用就不划算。只有对比才能判断。
func runEvalIntent(args []string) error {
	fs := flag.NewFlagSet("eval-intent", flag.ContinueOnError)
	datasetPath := fs.String("dataset", defaultIntentDatasetPath(), "意图评测集路径")
	jsonPath := fs.String("json", "", "机读报告输出路径")
	offline := fs.Bool("offline", false, "使用离线关键词基线（无需 API Key）")

	baseURL := fs.String("base-url", envOr("LLM_BASE_URL", "https://api.deepseek.com/v1"), "模型服务地址")
	apiKey := fs.String("api-key", envOr("LLM_API_KEY", ""), "模型 API Key")
	modelName := fs.String("model", envOr("LLM_MODEL", "deepseek-flash"), "模型名称")
	if err := fs.Parse(args); err != nil {
		return err
	}

	dataset, err := eval.LoadIntentDataset(*datasetPath)
	if err != nil {
		return err
	}
	// 先用数据集自身校验：拿一份有问题的数据跑出的指标是误导性的。
	if err := dataset.Validate(); err != nil {
		return fmt.Errorf("评测集校验失败: %w", err)
	}

	var classifier eval.IntentClassifier
	if *offline {
		classifier = keywordBaseline{}
		fmt.Fprintf(os.Stderr, "模式：离线关键词基线（零成本对照）\n\n")
	} else {
		if *apiKey == "" {
			return fmt.Errorf("缺少模型 API Key。请提供 -api-key 或设置 LLM_API_KEY；" +
				"离线对照请加 -offline")
		}
		model, err := llm.New(llm.Config{BaseURL: *baseURL, APIKey: *apiKey, Model: *modelName})
		if err != nil {
			return err
		}
		classifier = evalrun.NewIntentRunner(classify.New(model), context.Background())
		fmt.Fprintf(os.Stderr, "模式：真实模型 %s\n\n", *modelName)
	}

	startedAt := time.Now()
	report := eval.RunIntentEval(dataset, classifier)
	fmt.Printf("%s\n耗时 %v\n", report.Text(), time.Since(startedAt).Round(time.Millisecond))

	if *jsonPath != "" {
		if err := writeJSON(*jsonPath, report); err != nil {
			return err
		}
		fmt.Printf("\n机读报告已写入 %s\n", *jsonPath)
	}
	return nil
}

// keywordBaseline 是零成本的规则基线。
//
// 它的作用不是「可能比模型好」，而是**证明模型值不值得这一次调用**：
// 如果规则基线准确率已经足够高，那么为分类多付的 token 与延迟就没有意义。
// 这是评测里最基本的对照意识——没有基线，模型的数字无法解读。
type keywordBaseline struct{}

func (keywordBaseline) ClassifyIntent(text string) (eval.IntentPrediction, error) {
	return eval.IntentPrediction{
		Intent: classifyByKeyword(text),
		Raw:    "keyword-baseline",
		Parsed: true,
	}, nil
}

// 关键词表。刻意写得贴近真实规则引擎的形态：按优先级从上到下匹配。
var keywordRules = []struct {
	intent   string
	keywords []string
}{
	{"handoff", []string{"转人工", "人工客服", "真人", "找个人", "转接", "接人工", "人工服务"}},
	{"incident", []string{"报障", "建单", "建个工单", "创建工单", "登记", "记录问题", "坏了", "打不开", "用不了", "不可用", "失败", "报错", "宕机", "挂了"}},
	{"chitchat", []string{"你好", "您好", "谢谢", "多谢", "感谢", "再见", "拜拜", "在吗", "辛苦了", "好的"}},
	{"out_of_scope", []string{"写一首", "写个诗", "推荐餐厅", "推荐电影", "天气", "讲个笑话", "算命", "算一下", "翻译"}},
	{"knowledge", []string{"怎么", "如何", "为什么", "是什么", "什么原因", "怎么办", "步骤", "支持吗", "能不能", "可以吗"}},
}

func classifyByKeyword(text string) domain.Intent {
	for _, rule := range keywordRules {
		for _, keyword := range rule.keywords {
			if strings.Contains(text, keyword) {
				intent, ok := domain.ParseIntent(rule.intent)
				if ok {
					return intent
				}
			}
		}
	}
	// 全部未命中时回退到 knowledge：与分类器失败时的兜底一致，
	// 保证两条路径的默认行为相同，对比才公平。
	return domain.IntentKnowledge
}

// defaultIntentDatasetPath 定位意图评测集。
func defaultIntentDatasetPath() string {
	candidates := []string{
		"eval/datasets/intent.json",
		"../eval/datasets/intent.json",
		"../../eval/datasets/intent.json",
	}
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return filepath.Join("eval", "datasets", "intent.json")
}
