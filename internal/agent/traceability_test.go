package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/mac/helpdesk-agent/internal/llm"
)

func TestScriptedTraceabilityTracesLiteral(t *testing.T) {
	checker := NewScriptedTraceabilityChecker()
	corpus := []string{"核心下单接口从下午开始一直报 502", "我已经重启过网关了"}

	verdicts, err := checker.CheckAll(context.Background(), []Claim{
		{ID: "c1", Text: "核心下单接口"},  // 原样出现在用户话里
		{ID: "c2", Text: "重启过网关"},   // 去标点后子串命中
		{ID: "c3", Text: "已联系财务确认"}, // 用户从没说过
	}, corpus)
	if err != nil {
		t.Fatalf("规则级不应报错: %v", err)
	}
	if verdicts[0].Verdict != VerdictTraced {
		t.Errorf("c1 应 traced，实际 %s（%s）", verdicts[0].Verdict, verdicts[0].Reason)
	}
	if verdicts[1].Verdict != VerdictTraced {
		t.Errorf("c2 应 traced，实际 %s（%s）", verdicts[1].Verdict, verdicts[1].Reason)
	}
	if verdicts[2].Verdict != VerdictUnverified {
		t.Errorf("c3 用户没说，应 unverified，实际 %s", verdicts[2].Verdict)
	}
}

// TestRuleCheckerToleratesPunctuationAndCase 锁住"部分匹配容错"这一目的：
// 小模型给不出逐字 quote，但去标点、统一大小写后应能判成可追溯。
func TestRuleCheckerToleratesPunctuationAndCase(t *testing.T) {
	checker := NewScriptedTraceabilityChecker()
	corpus := []string{"登录 一直 转圈！！！"}
	verdicts, _ := checker.CheckAll(context.Background(), []Claim{
		{ID: "c1", Text: "登录一直转圈"},
	}, corpus)
	if verdicts[0].Verdict != VerdictTraced {
		t.Fatalf("去空格标点后应子串命中，实际 %s（%s）", verdicts[0].Verdict, verdicts[0].Reason)
	}
}

func TestRuleCheckerHighThresholdRejectsPartial(t *testing.T) {
	// 阈值调到 1.0：几乎不再有 traced。这是 sabotage 的一条——用于证明阈值承重。
	checker := NewRuleTraceabilityChecker(TraceabilityConfig{TokenOverlapThreshold: 1.0})
	corpus := []string{"下单接口报 502，页面打不开"}
	verdicts, _ := checker.CheckAll(context.Background(), []Claim{
		{ID: "c1", Text: "下单接口偶发超时"}, // 与语料部分重叠，但非完全
	}, corpus)
	if verdicts[0].Verdict == VerdictTraced {
		t.Errorf("阈值 1.0 时部分重叠不应判 traced，实际 %s", verdicts[0].Verdict)
	}
}

// entailModel 是可控的蕴含桩：按脚本决定 entailed / not / error。
type entailModel struct {
	sayEntail bool
	fail      bool
	calls     int
}

func (m *entailModel) Chat(_ context.Context, _, _ string) (*llm.Response, error) {
	m.calls++
	if m.fail {
		return nil, errors.New("模型不可用")
	}
	if m.sayEntail {
		return &llm.Response{Content: "ENTAIL"}, nil
	}
	return &llm.Response{Content: "NO"}, nil
}

func (m *entailModel) ChatWithTools(context.Context, llm.ToolRequest) (*llm.Response, error) {
	return nil, errors.New("蕴含判定不应调用带工具接口")
}

func TestTwoLevelEscalatesToSemanticOnlyForUntraced(t *testing.T) {
	model := &entailModel{sayEntail: true}
	checker := NewTwoLevelTraceabilityChecker(model, DefaultTraceabilityConfig())

	verdicts, err := checker.CheckAll(context.Background(), []Claim{
		{ID: "c1", Text: "下单接口报 502"}, // 规则直接 traced，不该惊动模型
		{ID: "c2", Text: "疑似网关超时"},    // 规则没命中，交给模型，模型说能推出
	}, []string{"下单接口报 502，帮忙看看"})
	if err != nil {
		t.Fatalf("两级判定不应报错: %v", err)
	}
	if verdicts[0].Verdict != VerdictTraced {
		t.Errorf("c1 应由规则判 traced，实际 %s", verdicts[0].Verdict)
	}
	if verdicts[1].Verdict != VerdictEntailed {
		t.Errorf("c2 规则没过但模型判定可推出，应 entailed，实际 %s", verdicts[1].Verdict)
	}
	// 关键成本断言：规则命中一条，模型只该被调一次（只为没过的 claim）。
	if model.calls != 1 {
		t.Errorf("已 traced 的 claim 不应再问模型，模型调用应为 1，实际 %d", model.calls)
	}
}

func TestTwoLevelFallsBackToUnverifiedOnModelError(t *testing.T) {
	model := &entailModel{fail: true}
	checker := NewTwoLevelTraceabilityChecker(model, DefaultTraceabilityConfig())
	verdicts, err := checker.CheckAll(context.Background(), []Claim{
		{ID: "c1", Text: "完全没提过的编造内容xyzzy"},
	}, []string{"用户只说了别的事"})
	if err != nil {
		t.Fatalf("模型失败应被吞成 unverified，而不是让整批失败: %v", err)
	}
	if verdicts[0].Verdict != VerdictUnverified {
		t.Errorf("模型失败时保守判 unverified，实际 %s", verdicts[0].Verdict)
	}
}

func TestTwoLevelWithNilModelDegradesToRule(t *testing.T) {
	// 传 nil 模型应退化为规则级（构造时即返回 rule），不 panic。
	checker := NewTwoLevelTraceabilityChecker(nil, DefaultTraceabilityConfig())
	verdicts, _ := checker.CheckAll(context.Background(), []Claim{
		{ID: "c1", Text: "下单接口"},
	}, []string{"下单接口报 502"})
	if verdicts[0].Verdict != VerdictTraced {
		t.Errorf("nil 模型时规则仍应生效，实际 %s", verdicts[0].Verdict)
	}
}
