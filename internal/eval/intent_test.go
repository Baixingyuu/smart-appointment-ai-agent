package eval

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mac/helpdesk-agent/internal/domain"
)

// stubClassifier 按文本返回预置意图，用于独立验证评测框架。
type stubClassifier struct {
	byText map[string]domain.Intent
	// parsed 为 false 时模拟模型输出无法解析的情况。
	unparsable map[string]bool
	err        error
	tokens     int
}

func newStubClassifier() *stubClassifier {
	return &stubClassifier{
		byText:     make(map[string]domain.Intent),
		unparsable: make(map[string]bool),
		tokens:     50,
	}
}

func (s *stubClassifier) ClassifyIntent(text string) (IntentPrediction, error) {
	if s.err != nil {
		return IntentPrediction{}, s.err
	}
	intent, ok := s.byText[text]
	if !ok {
		intent = domain.IntentKnowledge
	}
	return IntentPrediction{
		Intent:           intent,
		Raw:              string(intent),
		Parsed:           !s.unparsable[text],
		PromptTokens:     s.tokens,
		CompletionTokens: 2,
	}, nil
}

func intentDataset(cases ...IntentCase) *IntentDataset {
	return &IntentDataset{Version: 1, Cases: cases}
}

func intentCase(id, text string, intent domain.Intent) IntentCase {
	return IntentCase{ID: id, Text: text, Intent: intent}
}

func TestRunIntentEvalComputesAccuracy(t *testing.T) {
	runner := newStubClassifier()
	runner.byText["你好"] = domain.IntentChitchat
	runner.byText["接口401怎么办"] = domain.IntentKnowledge
	runner.byText["系统打不开了"] = domain.IntentIncident

	dataset := intentDataset(
		intentCase("c1", "你好", domain.IntentChitchat),
		intentCase("c2", "接口401怎么办", domain.IntentKnowledge),
		intentCase("c3", "系统打不开了", domain.IntentIncident),
	)

	report := RunIntentEval(dataset, runner)
	if report.Total != 3 || report.Correct != 3 {
		t.Fatalf("期望 3/3 正确，实际 %d/%d", report.Correct, report.Total)
	}
	if report.Accuracy != 1.0 {
		t.Errorf("准确率应为 1.0，实际 %f", report.Accuracy)
	}
	if report.ParseRate != 1.0 {
		t.Errorf("解析成功率应为 1.0，实际 %f", report.ParseRate)
	}
	// 寒暄不需工具循环（1/3），另两条需要。
	if report.ShortCircuitRate < 0.33 || report.ShortCircuitRate > 0.34 {
		t.Errorf("可短路比例应约 33%%，实际 %f", report.ShortCircuitRate)
	}
}

func TestRunIntentEvalRecordsConfusionMatrix(t *testing.T) {
	runner := newStubClassifier()
	// 把 incident 误判为 knowledge。
	runner.byText["系统打不开了"] = domain.IntentKnowledge

	dataset := intentDataset(
		intentCase("c1", "系统打不开了", domain.IntentIncident),
		intentCase("c2", "怎么重置密码", domain.IntentKnowledge),
	)
	runner.byText["怎么重置密码"] = domain.IntentKnowledge

	report := RunIntentEval(dataset, runner)

	if report.Confusion["incident"]["knowledge"] != 1 {
		t.Errorf("混淆矩阵应记录 incident→knowledge 一次，实际 %v", report.Confusion["incident"])
	}
	// 零值类别的行必须存在：只在出现时创建会让报告缺行，跨版本对比看不出差异。
	if _, ok := report.Confusion["handoff"]; !ok {
		t.Error("零值类别也应出现在混淆矩阵中")
	}
	if _, ok := report.ByClass["out_of_scope"]; !ok {
		t.Error("零值类别也应出现在按类别统计中")
	}
}

func TestRunIntentEvalComputesPrecisionRecallF1(t *testing.T) {
	runner := newStubClassifier()
	// knowledge 有 2 条：1 条判对，1 条被误判成 incident。
	runner.byText["怎么重置密码"] = domain.IntentKnowledge
	runner.byText["如何配置端口"] = domain.IntentIncident
	// incident 有 1 条且判对。
	runner.byText["系统打不开"] = domain.IntentIncident

	dataset := intentDataset(
		intentCase("c1", "怎么重置密码", domain.IntentKnowledge),
		intentCase("c2", "如何配置端口", domain.IntentKnowledge),
		intentCase("c3", "系统打不开", domain.IntentIncident),
	)

	report := RunIntentEval(dataset, runner)

	// knowledge: TP=1（c1），FN=1（c2 被误判），FP=0 → P=1.0, R=0.5, F1≈0.667
	knowledge := report.ByClass["knowledge"]
	if knowledge.Precision != 1.0 {
		t.Errorf("knowledge 精确率应为 1.0，实际 %f", knowledge.Precision)
	}
	if knowledge.Recall < 0.49 || knowledge.Recall > 0.51 {
		t.Errorf("knowledge 召回率应为 0.5，实际 %f", knowledge.Recall)
	}
	if knowledge.F1 < 0.66 || knowledge.F1 > 0.67 {
		t.Errorf("knowledge F1 应约 0.667，实际 %f", knowledge.F1)
	}

	// incident: TP=1（c3），FP=1（c2 被误判为 incident），FN=0 → P=0.5, R=1.0
	incident := report.ByClass["incident"]
	if incident.Precision < 0.49 || incident.Precision > 0.51 {
		t.Errorf("incident 精确率应为 0.5，实际 %f", incident.Precision)
	}
	if incident.Recall != 1.0 {
		t.Errorf("incident 召回率应为 1.0，实际 %f", incident.Recall)
	}
}

func TestMacroF1IgnoresEmptyClasses(t *testing.T) {
	// 只标注了两个类别。若把无样本类别的 F1=0 计入平均，
	// 指标会被人为压低，不同数据集之间无法比较。
	runner := newStubClassifier()
	runner.byText["你好"] = domain.IntentChitchat
	runner.byText["怎么用"] = domain.IntentKnowledge

	dataset := intentDataset(
		intentCase("c1", "你好", domain.IntentChitchat),
		intentCase("c2", "怎么用", domain.IntentKnowledge),
	)
	report := RunIntentEval(dataset, runner)

	// 两类都全对 → Macro-F1 应为 1.0（而非 2/5=0.4）。
	if report.MacroF1 != 1.0 {
		t.Fatalf("Macro-F1 应忽略无样本类别，期望 1.0，实际 %f", report.MacroF1)
	}
}

func TestUnparsableOutputIsCountedSeparately(t *testing.T) {
	// 解析失败与分类错误必须分开统计：前者是提示词或输出格式问题，
	// 后者是类别边界或模型理解问题，修复方向完全不同。
	runner := newStubClassifier()
	runner.byText["你好"] = domain.IntentChitchat
	runner.unparsable["你好"] = true

	dataset := intentDataset(intentCase("c1", "你好", domain.IntentChitchat))
	report := RunIntentEval(dataset, runner)

	if report.ParseRate != 0 {
		t.Errorf("解析成功率应为 0，实际 %f", report.ParseRate)
	}
	if len(report.Results) != 1 || report.Results[0].Parsed {
		t.Error("结果应标记为未成功解析")
	}
	// 关键：回退值恰好等于标注时，不得计为分类正确。
	//
	// 这是实测踩到的假绿——模型偶发返回空输出，回退成 knowledge 后
	// 与标注相同，于是被计成正确，准确率虚高而故障被掩盖。
	if report.Correct != 0 {
		t.Errorf("解析失败不得计为正确，实际 Correct=%d", report.Correct)
	}
	if report.Fallback != 1 {
		t.Errorf("应记录 1 条回退，实际 %d", report.Fallback)
	}
	if report.Accuracy != 0 {
		t.Errorf("准确率应为 0，实际 %f", report.Accuracy)
	}
}

func TestClassifierErrorDoesNotAbortRun(t *testing.T) {
	runner := newStubClassifier()
	runner.err = errors.New("模型不可用")

	dataset := intentDataset(
		intentCase("c1", "你好", domain.IntentChitchat),
		intentCase("c2", "怎么用", domain.IntentKnowledge),
	)
	report := RunIntentEval(dataset, runner)

	// 单条失败不应中断整轮评测，但必须记录下来。
	if len(report.Errors) != 2 {
		t.Fatalf("两条失败都应记录，实际 %d", len(report.Errors))
	}
	if report.Accuracy != 0 {
		t.Errorf("全部失败时准确率应为 0，实际 %f", report.Accuracy)
	}
}

func TestTokenUsageIsAccumulated(t *testing.T) {
	// 分类本身的 token 必须被记录，否则无法核算
	// 「加了分类层之后总成本是升还是降」。
	runner := newStubClassifier()
	runner.byText["你好"] = domain.IntentChitchat

	dataset := intentDataset(intentCase("c1", "你好", domain.IntentChitchat))
	report := RunIntentEval(dataset, runner)

	if report.PromptTokens != 50 {
		t.Errorf("prompt token 应累计为 50，实际 %d", report.PromptTokens)
	}
	if report.CompletionTokens != 2 {
		t.Errorf("completion token 应累计为 2，实际 %d", report.CompletionTokens)
	}
}

func TestIntentDatasetValidate(t *testing.T) {
	cases := []struct {
		name    string
		dataset *IntentDataset
		wantErr bool
	}{
		{"合法", intentDataset(intentCase("c1", "你好", domain.IntentChitchat)), false},
		{"空数据集", &IntentDataset{}, true},
		{"缺 ID", intentDataset(intentCase("", "你好", domain.IntentChitchat)), true},
		{"空文本", intentDataset(intentCase("c1", "  ", domain.IntentChitchat)), true},
		{"非法意图", intentDataset(intentCase("c1", "你好", domain.Intent("unknown"))), true},
		{"ID 重复", intentDataset(
			intentCase("c1", "你好", domain.IntentChitchat),
			intentCase("c1", "谢谢", domain.IntentChitchat),
		), true},
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

func TestParseIntentToleratesModelOutputVariants(t *testing.T) {
	// 模型常返回带大小写、标点或解释的文本。严格匹配会让大量
	// 本可用的结果被判为解析失败，使准确率指标失真。
	cases := map[string]domain.Intent{
		"knowledge":        domain.IntentKnowledge,
		"Knowledge":        domain.IntentKnowledge,
		"  knowledge  ":    domain.IntentKnowledge,
		"knowledge.":       domain.IntentKnowledge,
		"`knowledge`":      domain.IntentKnowledge,
		"knowledge（在问原因）":  domain.IntentKnowledge,
		"intent=incident":  domain.IntentIncident,
		"handoff\n":        domain.IntentHandoff,
		"答案是 out_of_scope": domain.IntentOutOfScope,
		"chitchat":         domain.IntentChitchat,
		"我判断为 incident":    domain.IntentIncident,
	}
	for raw, want := range cases {
		got, ok := domain.ParseIntent(raw)
		if !ok {
			t.Errorf("%q 应能解析出意图", raw)
			continue
		}
		if got != want {
			t.Errorf("%q 期望 %s，实际 %s", raw, want, got)
		}
	}

	for _, raw := range []string{"", "   ", "无法判断", "abc"} {
		if _, ok := domain.ParseIntent(raw); ok {
			t.Errorf("%q 不应解析出意图", raw)
		}
	}
}

func TestIntentNeedsToolLoop(t *testing.T) {
	// 这是路由的成本依据：只有这两种意图值得付出工具目录与证据的 token。
	if !domain.IntentKnowledge.NeedsToolLoop() {
		t.Error("knowledge 需要工具循环")
	}
	if !domain.IntentIncident.NeedsToolLoop() {
		t.Error("incident 需要工具循环")
	}
	for _, intent := range []domain.Intent{domain.IntentChitchat, domain.IntentOutOfScope, domain.IntentHandoff} {
		if intent.NeedsToolLoop() {
			t.Errorf("%s 不应进入工具循环", intent)
		}
	}
}

func TestLoadRealIntentDataset(t *testing.T) {
	path := filepath.Join("..", "..", "eval", "datasets", "intent.json")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("未找到意图评测集: %v", err)
	}

	dataset, err := LoadIntentDataset(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if err := dataset.Validate(); err != nil {
		t.Fatalf("数据集校验失败: %v", err)
	}
	if len(dataset.Cases) < 150 {
		t.Fatalf("样本量过少：%d", len(dataset.Cases))
	}

	counts := make(map[domain.Intent]int)
	withNote := 0
	for _, item := range dataset.Cases {
		counts[item.Intent]++
		if strings.TrimSpace(item.Note) != "" {
			withNote++
		}
	}
	// 每个类别都必须有样本，否则该类的精确率/召回率无从计算。
	for _, intent := range domain.AllIntents() {
		if counts[intent] == 0 {
			t.Errorf("类别 %s 没有样本，该类的精确率与召回率无法计算", intent)
		}
	}
	// 边界样本必须占一定比例，否则数据集区分力不足。
	if withNote < 25 {
		t.Errorf("标注了歧义说明的边界样本过少：%d，期望 ≥25", withNote)
	}
	t.Logf("样本分布：%v；带歧义说明 %d 条", counts, withNote)
}
