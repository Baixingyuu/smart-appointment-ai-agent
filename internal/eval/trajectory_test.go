package eval

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubRunner 按会话返回预置观测，用于独立验证评测框架本身。
//
// 不依赖 agent 包：评测框架必须能被单独测试，否则它的正确性
// 会被被测系统的行为混淆。
type stubRunner struct {
	// byConversation 键为会话 ID。
	byConversation map[int64][]TurnObservation
	// byMessage 键为消息内容，优先于 byConversation 匹配。
	byMessage map[string]TurnObservation
	// calls 记录每个会话被调用的次数，用于按轮次取观测。
	calls map[int64]int
	err   error
}

func newStubRunner() *stubRunner {
	return &stubRunner{
		byConversation: make(map[int64][]TurnObservation),
		byMessage:      make(map[string]TurnObservation),
		calls:          make(map[int64]int),
	}
}

func (s *stubRunner) RunTurn(conversationID int64, message string) (TurnObservation, error) {
	if s.err != nil {
		return TurnObservation{}, s.err
	}
	if observation, ok := s.byMessage[message]; ok {
		return observation, nil
	}
	sequence := s.byConversation[conversationID]
	if len(sequence) == 0 {
		return TurnObservation{Reply: "默认回复", Rounds: 1}, nil
	}
	index := s.calls[conversationID]
	s.calls[conversationID]++
	if index >= len(sequence) {
		index = len(sequence) - 1
	}
	return sequence[index], nil
}

// obs 构造观测结果。
func obs(reply string, tools ...string) TurnObservation {
	invocations := make([]ToolInvocation, 0, len(tools))
	for _, code := range tools {
		invocations = append(invocations, ToolInvocation{Code: code, Status: "completed"})
	}
	return TurnObservation{
		Reply: reply, Tools: invocations, Rounds: 1,
		Usage: TokenUsage{PromptTokens: 100, CompletionTokens: 20}, DurationUS: 500,
	}
}

func trajectoryDataset(t *testing.T, cases ...TrajectoryCase) *TrajectoryDataset {
	t.Helper()
	return &TrajectoryDataset{Version: 1, Cases: cases}
}

func boolPtr(v bool) *bool { return &v }

func TestRunPassesWhenToolsMatchExpectation(t *testing.T) {
	runner := newStubRunner()
	runner.byConversation[1] = []TurnObservation{
		obs("已检索知识库", ToolRAGSearchConst),
	}

	dataset := trajectoryDataset(t, TrajectoryCase{
		ID: "c1", Scenario: "knowledge_answered", ConversationID: 1,
		Messages: []string{"接口报错"},
		Expect: Expected{
			Tools:               []string{ToolRAGSearchConst},
			ForbiddenTools:      []string{ToolConfirmConst},
			MaxRounds:           2,
			ExpectTicketCreated: boolPtr(false),
		},
	})

	report := Run(dataset, runner)
	if report.Passed != 1 {
		t.Fatalf("应通过，失败原因：%+v", report.Failures)
	}
	if report.ToolAccuracy != 1.0 {
		t.Errorf("工具选择准确率应为 1.0，实际 %f", report.ToolAccuracy)
	}
	if report.SafetyViolationRate != 0 {
		t.Errorf("不应有安全越界，实际 %f", report.SafetyViolationRate)
	}
}

func TestRunDetectsMissingExpectedTool(t *testing.T) {
	runner := newStubRunner()
	// 模型直接回答，没有检索。
	runner.byConversation[1] = []TurnObservation{obs("我直接回答")}

	dataset := trajectoryDataset(t, TrajectoryCase{
		ID: "c1", ConversationID: 1, Messages: []string{"接口报错"},
		Expect: Expected{Tools: []string{ToolRAGSearchConst}},
	})

	report := Run(dataset, runner)
	if report.Passed != 0 {
		t.Fatal("未调用期望工具应判失败")
	}
	failure := report.Failures[0]
	if len(failure.MissingTools) != 1 || failure.MissingTools[0] != ToolRAGSearchConst {
		t.Fatalf("应记录缺失工具，实际 %v", failure.MissingTools)
	}
	if report.ToolAccuracy != 0 {
		t.Errorf("工具选择准确率应为 0，实际 %f", report.ToolAccuracy)
	}
}

func TestRunDetectsForbiddenToolCall(t *testing.T) {
	// 安全维度：不该建单的场景却建了单，必须判失败并计入越界率。
	runner := newStubRunner()
	created := obs("已为你建单", ToolConfirmConst)
	created.TicketID = 42
	runner.byConversation[1] = []TurnObservation{created}

	dataset := trajectoryDataset(t, TrajectoryCase{
		ID: "c1", ConversationID: 1, Messages: []string{"如何重置密码"},
		Expect: Expected{
			ForbiddenTools:      []string{ToolConfirmConst},
			ExpectTicketCreated: boolPtr(false),
		},
	})

	report := Run(dataset, runner)
	if report.Passed != 0 {
		t.Fatal("调用被禁止工具应判失败")
	}
	if report.SafetyViolationRate != 1.0 {
		t.Fatalf("越界率应为 1.0，实际 %f", report.SafetyViolationRate)
	}
	// 越界与建单两个断言都应报出，便于定位。
	if len(report.Failures[0].Failures) < 2 {
		t.Errorf("应同时报出越界与建单失败，实际 %v", report.Failures[0].Failures)
	}
}

func TestRunDetectsOrderViolation(t *testing.T) {
	runner := newStubRunner()
	// 顺序颠倒：先建单再查重。
	runner.byConversation[1] = []TurnObservation{
		obs("先建单", ToolConfirmConst),
		obs("再查重", ToolFindConst),
	}

	dataset := trajectoryDataset(t, TrajectoryCase{
		ID: "c1", ConversationID: 1, Messages: []string{"a", "b"},
		Expect: Expected{OrderedTools: []string{ToolFindConst, ToolConfirmConst}},
	})

	report := Run(dataset, runner)
	if report.Passed != 0 {
		t.Fatal("顺序不符应判失败")
	}
	if !report.Failures[0].OrderViolated {
		t.Error("应标记顺序违规")
	}
	if report.OrderAccuracy != 0 {
		t.Errorf("顺序准确率应为 0，实际 %f", report.OrderAccuracy)
	}
}

func TestOrderIsSubsequenceNotExactMatch(t *testing.T) {
	// 顺序断言用子序列匹配：期望序列之间允许有额外调用，
	// 否则会因「多做了一步无害检索」而误判失败。
	runner := newStubRunner()
	runner.byConversation[1] = []TurnObservation{
		obs("检索", ToolRAGSearchConst),
		obs("查重", ToolFindConst),
		obs("建单", ToolConfirmConst),
	}

	dataset := trajectoryDataset(t, TrajectoryCase{
		ID: "c1", ConversationID: 1, Messages: []string{"a", "b", "c"},
		Expect: Expected{OrderedTools: []string{ToolFindConst, ToolConfirmConst}},
	})

	report := Run(dataset, runner)
	if report.Passed != 1 {
		t.Fatalf("额外的前置调用不应导致顺序失败：%+v", report.Failures)
	}
}

func TestRunDetectsExcessiveRounds(t *testing.T) {
	runner := newStubRunner()
	tooMany := obs("绕了很多步", ToolRAGSearchConst)
	tooMany.Rounds = 9
	runner.byConversation[1] = []TurnObservation{tooMany}

	dataset := trajectoryDataset(t, TrajectoryCase{
		ID: "c1", ConversationID: 1, Messages: []string{"接口报错"},
		Expect: Expected{MaxRounds: 3},
	})

	report := Run(dataset, runner)
	if report.Passed != 0 {
		t.Fatal("轮次超上限应判失败")
	}
	if !strings.Contains(strings.Join(report.Failures[0].Failures, " "), "步骤过多") {
		t.Errorf("失败原因应说明步骤过多，实际 %v", report.Failures[0].Failures)
	}
}

func TestRunDetectsTokenBudgetOverrun(t *testing.T) {
	// 成本回归：上下文悄悄膨胀时必须失败。
	runner := newStubRunner()
	expensive := obs("贵", ToolRAGSearchConst)
	expensive.Usage = TokenUsage{PromptTokens: 50000, CompletionTokens: 100}
	runner.byConversation[1] = []TurnObservation{expensive}

	dataset := trajectoryDataset(t, TrajectoryCase{
		ID: "c1", ConversationID: 1, Messages: []string{"接口报错"},
		Expect: Expected{MaxPromptTokens: 1000},
	})

	report := Run(dataset, runner)
	if report.Passed != 0 {
		t.Fatal("token 超上限应判失败")
	}
	if report.PromptTokens != 50000 {
		t.Errorf("应累计 prompt token，实际 %d", report.PromptTokens)
	}
}

func TestRunCountsBudgetRejectionsSeparately(t *testing.T) {
	// 预算超限与其他拒绝原因必须分开统计：前者该调预算，
	// 后者该改工具设计，改进方向不同。
	runner := newStubRunner()
	observation := TurnObservation{
		Reply: "被拒了", Rounds: 1,
		Tools: []ToolInvocation{
			{Code: ToolRAGSearchConst, Status: "completed"},
			{Code: ToolRAGSearchConst, Status: "failed", ErrorKind: "budget_exceeded"},
			{Code: "made_up_tool", Status: "failed", ErrorKind: "unknown_tool"},
		},
	}
	runner.byConversation[1] = []TurnObservation{observation}

	dataset := trajectoryDataset(t, TrajectoryCase{
		ID: "c1", ConversationID: 1, Messages: []string{"x"},
		Expect: Expected{},
	})

	report := Run(dataset, runner)
	if report.BudgetRejectRate != 1.0 {
		t.Errorf("预算超限率应为 1.0，实际 %f", report.BudgetRejectRate)
	}
	// 被拒绝但不属于预算的调用不应计入预算超限。
	if report.Results[0].RejectedCalls != 2 {
		t.Errorf("拒绝次数应为 2，实际 %d", report.Results[0].RejectedCalls)
	}
}

func TestRunAggregatesAcrossTurns(t *testing.T) {
	// 多轮用例（含确认恢复）必须聚合全部轮次，而不是只看最后一轮。
	runner := newStubRunner()
	first := obs("请确认", ToolConfirmConst)
	first.Interrupted = true
	second := obs("已建单", ToolConfirmConst)
	second.TicketID = 7
	runner.byConversation[1] = []TurnObservation{first, second}

	dataset := trajectoryDataset(t, TrajectoryCase{
		ID: "c1", ConversationID: 1, Messages: []string{"建个工单", "确认"},
		Expect: Expected{
			Tools:               []string{ToolConfirmConst},
			ExpectTicketCreated: boolPtr(true),
			MaxRounds:           4,
		},
	})

	report := Run(dataset, runner)
	if report.Passed != 1 {
		t.Fatalf("多轮聚合后应通过：%+v", report.Failures)
	}
	if report.Results[0].Rounds != 2 {
		t.Errorf("轮次应跨轮累计为 2，实际 %d", report.Results[0].Rounds)
	}
	if report.Results[0].TotalTokens != 240 {
		t.Errorf("token 应跨轮累计为 240，实际 %d", report.Results[0].TotalTokens)
	}
	if report.Results[0].DurationUS != 1000 {
		t.Errorf("耗时应跨轮累计为 1000 µs，实际 %d", report.Results[0].DurationUS)
	}
	// 末轮未中断，终态应为「未中断」。
	if report.Results[0].Interrupted {
		t.Error("末轮未中断时终态不应标记为中断")
	}
}

func TestLatencyPercentilesUseActualObservations(t *testing.T) {
	// 分位数必须取自真实观测值。用最近秩法而非插值：
	// 小样本下插值会造出比任何实际观测都快的虚构数字。
	runner := newStubRunner()
	dataset := &TrajectoryDataset{Version: 1}
	for i := 0; i < 10; i++ {
		observation := obs("ok")
		observation.DurationUS = (i + 1) * 100 // 100..1000 µs
		runner.byConversation[int64(i)] = []TurnObservation{observation}
		dataset.Cases = append(dataset.Cases, TrajectoryCase{
			ID: "c" + itoa(i), ConversationID: int64(i), Messages: []string{"x"},
		})
	}

	report := Run(dataset, runner)
	if report.Latency.Max != 1000 {
		t.Errorf("最大延迟应为 1000 µs，实际 %d", report.Latency.Max)
	}
	// 观测值必须来自样本集合本身。
	valid := map[int]bool{100: true, 200: true, 300: true, 400: true, 500: true,
		600: true, 700: true, 800: true, 900: true, 1000: true}
	if !valid[report.Latency.P50] {
		t.Errorf("P50 应取自真实观测值，实际 %d", report.Latency.P50)
	}
	if !valid[report.Latency.P95] {
		t.Errorf("P95 应取自真实观测值，实际 %d", report.Latency.P95)
	}
}

func TestRunnerErrorFailsCaseWithoutPanic(t *testing.T) {
	runner := newStubRunner()
	runner.err = errors.New("上游不可用")

	dataset := trajectoryDataset(t, TrajectoryCase{
		ID: "c1", ConversationID: 1, Messages: []string{"x"},
	})

	report := Run(dataset, runner)
	if report.Passed != 0 {
		t.Fatal("执行失败应判该用例未通过")
	}
	if len(report.Failures[0].Failures) == 0 {
		t.Error("应记录执行失败原因")
	}
	// 单个用例失败不应中断整轮评测。
	if report.Total != 1 {
		t.Errorf("应仍统计样本总数，实际 %d", report.Total)
	}
}

func TestReportTextIncludesAllAxes(t *testing.T) {
	runner := newStubRunner()
	runner.byConversation[1] = []TurnObservation{obs("ok", ToolRAGSearchConst)}
	dataset := trajectoryDataset(t, TrajectoryCase{
		ID: "c1", Scenario: "s1", ConversationID: 1, Messages: []string{"x"},
		Expect: Expected{Tools: []string{ToolRAGSearchConst}},
	})

	text := Run(dataset, runner).Text()
	// 四个评测轴都必须在报告里可见，否则无法据此写结论。
	for _, needle := range []string{
		"整体通过率", "工具选择准确率", "端到端延迟 P95", "prompt token",
		"安全越界率", "预算超限率", "按场景分解",
	} {
		if !strings.Contains(text, needle) {
			t.Errorf("报告应包含 %q\n实际输出：\n%s", needle, text)
		}
	}
}

func TestLoadRealTrajectoryDataset(t *testing.T) {
	path := filepath.Join("..", "..", "eval", "datasets", "trajectory.json")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("未找到轨迹评测集: %v", err)
	}

	dataset, err := LoadTrajectoryDataset(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if len(dataset.Cases) < 16 {
		t.Fatalf("评测集样本过少：%d", len(dataset.Cases))
	}

	// 结构与语义校验：内容不合格的评测集会给出误导性的指标。
	seen := make(map[string]bool, len(dataset.Cases))
	scenarios := make(map[string]int)
	for _, item := range dataset.Cases {
		if item.ID == "" {
			t.Fatal("存在缺失 ID 的样本")
		}
		if seen[item.ID] {
			t.Fatalf("样本 ID 重复: %s", item.ID)
		}
		seen[item.ID] = true

		if item.ConversationID <= 0 {
			t.Errorf("%s: 必须指定会话 ID（多轮用例依赖它共享会话）", item.ID)
		}
		if len(item.Messages) == 0 {
			t.Errorf("%s: 至少需要一条消息", item.ID)
		}
		// 空期望等于没有断言，这类样本只会虚高通过率。
		if len(item.Expect.Tools) == 0 && len(item.Expect.OrderedTools) == 0 &&
			len(item.Expect.ForbiddenTools) == 0 && item.Expect.MaxRounds == 0 &&
			item.Expect.MinRounds == 0 && item.Expect.ExpectTicketCreated == nil &&
			item.Expect.ExpectInterrupted == nil && item.Expect.MaxPromptTokens == 0 {
			t.Errorf("%s: 未声明任何期望，该样本无断言意义", item.ID)
		}
		scenarios[item.Scenario]++
	}

	// 必须覆盖安全维度，否则「越界率」恒为 0，指标没有意义。
	safety := scenarios["safety_forbidden_write"]
	if safety == 0 {
		t.Error("评测集必须包含安全越界类样本，否则安全指标恒为 0")
	}
}

// 数据集里使用的工具编码，与 agent 包保持一致。
// 在此处定义为常量而非常量导入 agent 包：评测框架不应依赖业务实现。
const (
	ToolRAGSearchConst = "rag_search"
	ToolFindConst      = "ticket_find_open_by_topic"
	ToolConfirmConst   = "ticket_create_confirm"
)
