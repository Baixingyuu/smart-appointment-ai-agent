package agent

import (
	"errors"
	"testing"

	"github.com/mac/helpdesk-agent/internal/domain"
	"github.com/mac/helpdesk-agent/internal/llm"
)

// stubClassifier 按文本返回预置意图。
type stubClassifier struct {
	byText map[string]domain.Intent
	err    error
	calls  int
}

func (s *stubClassifier) ClassifyIntent(text string) (IntentOutcome, error) {
	s.calls++
	if s.err != nil {
		return IntentOutcome{}, s.err
	}
	intent, ok := s.byText[text]
	if !ok {
		intent = domain.IntentKnowledge
	}
	return IntentOutcome{
		Intent: intent, Parsed: true,
		Usage: llm.Usage{PromptTokens: 200, CompletionTokens: 5},
	}, nil
}

func TestShortCircuitSkipsToolLoop(t *testing.T) {
	// 寒暄必须短路：不进入工具循环，因此不消耗工具 schema 与证据的 token。
	classifier := &stubClassifier{byText: map[string]domain.Intent{"你好": domain.IntentChitchat}}
	h := newHarness(t, []*llm.Response{finalReply("你好，请描述你遇到的问题。")},
		withClassifier(classifier))

	result := h.runTurn(t, 1, "你好")

	if !result.ShortCircuited {
		t.Fatal("寒暄应走短路路径")
	}
	if result.Intent != domain.IntentChitchat {
		t.Errorf("意图应为 chitchat，实际 %s", result.Intent)
	}
	// 短路路径不调用任何工具。
	if len(result.ToolCalls) != 0 {
		t.Errorf("短路路径不应调用工具，实际 %v", result.ToolCalls)
	}
	if result.Reply == "" {
		t.Error("短路路径仍须给出回复")
	}
	// 分类成本必须计入总用量，否则「加分类层是升还是降」无法核算。
	if result.Usage.PromptTokens < 200 {
		t.Errorf("分类 token 应计入总用量，实际 %d", result.Usage.PromptTokens)
	}
}

func TestKnowledgeStillRunsToolLoop(t *testing.T) {
	classifier := &stubClassifier{byText: map[string]domain.Intent{"接口401": domain.IntentKnowledge}}
	h := newHarness(t, []*llm.Response{
		toolCall("c1", ToolRAGSearch, `{"query":"接口 401"}`),
		finalReply("请检查令牌。"),
	}, withClassifier(classifier))

	result := h.runTurn(t, 1, "接口401")

	if result.ShortCircuited {
		t.Fatal("knowledge 不应短路，需要检索")
	}
	if result.Intent != domain.IntentKnowledge {
		t.Errorf("意图应被记录，实际 %q", result.Intent)
	}
	if len(result.ToolCalls) == 0 {
		t.Error("knowledge 必须进入工具循环")
	}
}

func TestConfirmResumeTakesPrecedenceOverRouting(t *testing.T) {
	// 关键顺序断言：用户回复「确认」按语义属于 chitchat，
	// 若分类先于确认恢复执行，会被短路成寒暄回复，从而丢失建单确认。
	classifier := &stubClassifier{byText: map[string]domain.Intent{
		"帮我建个工单": domain.IntentIncident,
		"确认":     domain.IntentChitchat, // 故意分类错误，模拟真实模型的判定
	}}
	h := newHarness(t, []*llm.Response{
		toolCall("c1", ToolCreateConfirm, `{"title":"测试工单","description":"描述","category":"incident","priority":"P2"}`),
	}, withClassifier(classifier))

	first := h.runTurn(t, 500, "帮我建个工单")
	if !first.Interrupted {
		t.Fatal("应先发起确认")
	}

	second := h.runTurn(t, 500, "确认")
	if second.ShortCircuited {
		t.Fatal("确认恢复必须优先于意图分类，不能被短路")
	}
	if second.TicketID == 0 {
		t.Fatal("确认后应创建工单")
	}
}

func TestClassifierFailureFallsBackToToolLoop(t *testing.T) {
	// 分类失败不应阻断主流程：回退到完整链路，
	// 行为与无路由版本一致，只是丧失短路的成本收益。
	classifier := &stubClassifier{err: errors.New("模型不可用")}
	h := newHarness(t, []*llm.Response{finalReply("好的。")}, withClassifier(classifier))

	result := h.runTurn(t, 1, "随便说点什么")

	if result.ShortCircuited {
		t.Error("分类失败时不应短路")
	}
	if result.Reply == "" {
		t.Error("仍须给出回复")
	}
}

func TestWithoutClassifierBehavesAsBefore(t *testing.T) {
	// 未注入分类器时行为应与无路由版本完全一致。
	h := newHarness(t, []*llm.Response{
		toolCall("c1", ToolRAGSearch, `{"query":"接口 401"}`),
		finalReply("请检查令牌。"),
	})

	result := h.runTurn(t, 1, "接口401怎么办")

	if result.ShortCircuited {
		t.Error("未配置分类器时不应短路")
	}
	if result.Intent != "" {
		t.Errorf("未配置分类器时意图应为空，实际 %q", result.Intent)
	}
	if len(result.ToolCalls) == 0 {
		t.Error("应走完整工具链路")
	}
}

func TestHandoffAndOutOfScopeShortCircuit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		text   string
		intent domain.Intent
	}{
		{"转人工", "我要找人工客服", domain.IntentHandoff},
		{"无关请求", "帮我写一首诗", domain.IntentOutOfScope},
	} {
		t.Run(tc.name, func(t *testing.T) {
			classifier := &stubClassifier{byText: map[string]domain.Intent{tc.text: tc.intent}}
			h := newHarness(t, []*llm.Response{finalReply("好的。")}, withClassifier(classifier))

			result := h.runTurn(t, 1, tc.text)
			if !result.ShortCircuited {
				t.Fatalf("%s 应短路", tc.intent)
			}
			if len(result.ToolCalls) != 0 {
				t.Errorf("短路路径不应调用工具")
			}
		})
	}
}

func TestShortCircuitRecordsRoundUsage(t *testing.T) {
	// 短路路径也要留下逐轮记录：否则成本归因里会少一块，
	// 无法解释「为什么这次没有主循环的 token」。
	classifier := &stubClassifier{byText: map[string]domain.Intent{"你好": domain.IntentChitchat}}
	h := newHarness(t, []*llm.Response{finalReply("你好。")}, withClassifier(classifier))

	result := h.runTurn(t, 1, "你好")

	if len(result.RoundRecords) != 1 {
		t.Fatalf("短路路径应记录 1 轮，实际 %d", len(result.RoundRecords))
	}
	if len(result.RoundRecords[0].Tools) != 0 {
		t.Error("短路轮不应包含工具")
	}
}
