package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/mac/helpdesk-agent/internal/conversation"
	"github.com/mac/helpdesk-agent/internal/domain"
	"github.com/mac/helpdesk-agent/internal/llm"
	"github.com/mac/helpdesk-agent/internal/ticket"
)

// 建单交互三支柱（进展驱动追问 / 可追溯判定 / forceCommit 出口）的回归覆盖。
//
// 分两层：
//   - decideIntake 是纯函数，先把"问到没进展就建单"这套规则单测打透；
//   - 端到端 8 例走 conversation.Service.Send —— 只有这条路会把用户原话落进
//     store，可追溯判定的语料才有内容（修 D1 结构盲区，见 TICKET_INTAKE §8）。

// ---------- 测试辅助 ----------

var intakeReqSeq int64

// intakeHarness 组装一套启用建单交互（规则级判定器）的离线装置。
func intakeHarness(t *testing.T, responses []*llm.Response, gating bool) *harness {
	t.Helper()
	return newHarness(t, responses,
		withIntake(IntakeConfig{Checker: NewScriptedTraceabilityChecker(), Gating: gating}))
}

func (h *harness) mustStart(t *testing.T) int64 {
	t.Helper()
	conv, err := h.conversations.Start(conversation.StartInput{Title: "建单交互测试"})
	if err != nil {
		t.Fatalf("新建会话失败: %v", err)
	}
	return conv.ID
}

// send 走真实会话编排：客户消息先落库（供可追溯判定），再触发 Agent 回合。
func (h *harness) send(t *testing.T, convID int64, content string) *conversation.SendResult {
	t.Helper()
	intakeReqSeq++
	res, err := h.conversations.Send(context.Background(), convID, content, fmt.Sprintf("intake-req-%d", intakeReqSeq))
	if err != nil {
		t.Fatalf("Send %q 失败: %v", content, err)
	}
	return &res
}

// lastTurnToolCalls 返回最近一次回合的工具调用记录（经 Send 路径抓取）。
func (h *harness) lastTurnToolCalls() []ToolCallRecord {
	if h.turns == nil || h.turns.last == nil {
		return nil
	}
	return h.turns.last.ToolCalls
}

// awaiting 报告会话当前是否仍停在可确认状态。
func (h *harness) awaiting(convID int64) bool {
	return h.agent.awaitingConfirmation(convID)
}

// confirmArgs 生成一次 ticket_create_confirm 的参数 JSON，slots 为 extractedSlots。
func confirmArgs(title, desc, category string, slots ...[2]string) string {
	parts := make([]string, 0, len(slots))
	for _, s := range slots {
		parts = append(parts, fmt.Sprintf(`{"name":%q,"quote":%q}`, s[0], s[1]))
	}
	extracted := "[]"
	if len(parts) > 0 {
		extracted = "[" + strings.Join(parts, ",") + "]"
	}
	return fmt.Sprintf(`{"title":%q,"description":%q,"category":%q,"priority":"P1","extractedSlots":%s}`,
		title, desc, category, extracted)
}

// pendingMissing 读取会话当前待确认草案的 MissingInfo（无草案则失败）。
func pendingMissing(t *testing.T, h *harness, convID int64) []string {
	t.Helper()
	p, ok := h.store.FindPendingInterrupt(convID)
	if !ok {
		t.Fatalf("会话 %d 应有待确认草案", convID)
	}
	return p.Payload.MissingInfo
}

func hasNote(items []string, want string) bool {
	for _, it := range items {
		if it == want || strings.HasPrefix(it, want) {
			return true
		}
	}
	return false
}

// ---------- 纯函数：decideIntake ----------

func TestDecideIntakeFreshEmptyTriggersAsk(t *testing.T) {
	got := decideIntake([]string{"target_service", "planned_window"}, domain.IntakeProgress{}, 10)
	if got.Action != intakeAsk {
		t.Fatalf("从没问过的空槽应触发追问，实际 %v", got.Action)
	}
	if strings.Join(got.AskSlots, ",") != "target_service,planned_window" {
		t.Errorf("应一次性列出全部缺失槽位，实际 %v", got.AskSlots)
	}
	if !got.Progress.Asked("target_service") || !got.Progress.Asked("planned_window") {
		t.Error("被追问的槽位必须标记为已问，否则下一轮会重复追问（挤牙膏）")
	}
	if got.Progress.AskRounds != 1 {
		t.Errorf("追问轮次应推进到 1，实际 %d", got.Progress.AskRounds)
	}
}

func TestDecideIntakeAskedOnceStillEmptyAbandonsAndCreates(t *testing.T) {
	progress := domain.IntakeProgress{AskedSlots: []string{"affected_system"}, AskRounds: 1}
	got := decideIntake([]string{"affected_system"}, progress, 10)
	if got.Action != intakeCreate {
		t.Fatalf("问过仍空应建单，实际 %v", got.Action)
	}
	if strings.Join(got.AbandonSlots, ",") != "affected_system" {
		t.Errorf("应把该槽位记为放弃追问，实际 %v", got.AbandonSlots)
	}
	if got.Progress.AskRounds != 1 {
		t.Errorf("建单路径不应再推进轮次，实际 %d", got.Progress.AskRounds)
	}
}

func TestDecideIntakeMixedAsksOnlyFresh(t *testing.T) {
	progress := domain.IntakeProgress{AskedSlots: []string{"target_service"}, AskRounds: 1}
	got := decideIntake([]string{"target_service", "planned_window"}, progress, 10)
	if got.Action != intakeAsk {
		t.Fatalf("存在未问过的新空槽应继续追问，实际 %v", got.Action)
	}
	if strings.Join(got.AskSlots, ",") != "planned_window" {
		t.Errorf("只应追问没问过的槽位，实际 %v", got.AskSlots)
	}
}

func TestDecideIntakeSafetyValve(t *testing.T) {
	progress := domain.IntakeProgress{AskedSlots: []string{"affected_system"}, AskRounds: 3}
	got := decideIntake([]string{"affected_system"}, progress, 3)
	if got.Action != intakeCreate || !got.SafetyValve {
		t.Fatalf("触顶安全阀应无条件建单并打标，实际 action=%v valve=%v", got.Action, got.SafetyValve)
	}
	if strings.Join(got.AbandonSlots, ",") != "affected_system" {
		t.Errorf("安全阀下所有空槽都算放弃，实际 %v", got.AbandonSlots)
	}
}

func TestDecideIntakeNoEmptyCreates(t *testing.T) {
	got := decideIntake(nil, domain.IntakeProgress{AskRounds: 1}, 10)
	if got.Action != intakeCreate || len(got.AbandonSlots) != 0 || got.SafetyValve {
		t.Fatalf("无缺失应干净建单，实际 %+v", got)
	}
}

func TestDecideIntakeDedupesAndKeepsOrder(t *testing.T) {
	got := decideIntake([]string{"b", "a", "b", "a"}, domain.IntakeProgress{}, 10)
	if strings.Join(got.AskSlots, ",") != "b,a" {
		t.Errorf("应去重并保持 required 顺序，实际 %v", got.AskSlots)
	}
}

// ---------- 端到端交互轴（8 例，走 conversation.Service.Send）----------

// Case 1：一句话信息全 → 单轮起草、零追问、建单后无缺失/忠实度标注。
func TestIntakeCase1CompleteInfoNoAsk(t *testing.T) {
	h := intakeHarness(t, []*llm.Response{
		toolCall("c1", ToolCreateConfirm, confirmArgs("支付系统报错", "支付系统一直报500错误", "incident",
			[2]string{"affected_system", "支付系统一直报500错误"})),
	}, true)
	conv := h.mustStart(t)

	res := h.send(t, conv, "支付系统一直报500错误，帮我建个工单")
	if !res.Turn.Interrupted {
		t.Fatal("信息齐全应直接起草并征询确认")
	}
	if got := len(h.store.ListTickets()); got != 0 {
		t.Fatalf("确认前不应建单，实际 %d", got)
	}
	for _, tc := range h.lastTurnToolCalls() {
		if tc.Status == "awaiting_user_info" {
			t.Error("信息齐全不应触发追问")
		}
	}
	missing := pendingMissing(t, h, conv)
	if hasNote(missing, "missing_slot:") || hasNote(missing, missingFidelityUnverified) {
		t.Errorf("齐全工单不应有缺失/忠实度标注，实际 %v", missing)
	}

	h.send(t, conv, "确认")
	if got := len(h.store.ListTickets()); got != 1 {
		t.Fatalf("确认后应建单 1 张，实际 %d", got)
	}
}

// Case 2：缺阻塞槽位 → 追问 1 次，且一次性列全所有缺失。
func TestIntakeCase2AskOnceListsAll(t *testing.T) {
	// 模型对 target_service / planned_window 都给了用户没说过的编造 quote → 两者皆判空。
	h := intakeHarness(t, []*llm.Response{
		toolCall("c1", ToolCreateConfirm, confirmArgs("变更工单", "", "change",
			[2]string{"target_service", "不存在的服务名"},
			[2]string{"planned_window", "不存在的窗口"})),
		finalReply("请问这次变更针对哪个服务、计划在什么时间窗口执行？"),
	}, true)
	conv := h.mustStart(t)

	h.send(t, conv, "帮我提一个变更工单")
	if got := len(h.store.ListTickets()); got != 0 {
		t.Fatalf("缺关键信息应追问而非起草，已建单 %d", got)
	}
	if _, ok := h.store.FindPendingInterrupt(conv); ok {
		t.Error("追问轮不应留下待确认草案")
	}
	calls := h.lastTurnToolCalls()
	if len(calls) == 0 || calls[0].Status != "awaiting_user_info" {
		t.Fatalf("应记录为等待用户补充信息，实际 %+v", calls)
	}
	obs := calls[0].Result
	for _, slot := range ticket.BlockingSlots(domain.CategoryChange) {
		if !strings.Contains(obs, ticket.SlotLabel(slot)) {
			t.Errorf("追问观察应一次性列出缺失项 %q，实际 %q", slot, obs)
		}
	}
}

// Case 3：追问后用户补齐 → 第 2 轮据其原话起草，无缺失/忠实度标注。
func TestIntakeCase3FillAfterAsk(t *testing.T) {
	h := intakeHarness(t, []*llm.Response{
		toolCall("c1", ToolCreateConfirm, confirmArgs("系统问题", "", "incident",
			[2]string{"affected_system", "查无此系统"})),
		finalReply("请问是哪个系统出的问题？"),
		toolCall("c2", ToolCreateConfirm, confirmArgs("登录报错", "登录页一直打不开", "incident",
			[2]string{"affected_system", "登录页一直打不开"})),
	}, true)
	conv := h.mustStart(t)

	h.send(t, conv, "帮我建个工单")
	h.send(t, conv, "登录页一直打不开，帮我建单")
	if !h.awaiting(conv) {
		t.Fatal("补齐后第 2 轮应就新草案征询确认")
	}
	missing := pendingMissing(t, h, conv)
	if hasNote(missing, "missing_slot:") || hasNote(missing, missingFidelityUnverified) {
		t.Errorf("补齐的槽位不应再被标注，实际 %v", missing)
	}
}

// Case 4：补充后仍缺同一字段 → 停止追问、建单并把缺失记进 MissingInfo。
func TestIntakeCase4StopAfterRepeatedEmpty(t *testing.T) {
	h := intakeHarness(t, []*llm.Response{
		toolCall("c1", ToolCreateConfirm, confirmArgs("工单", "", "incident",
			[2]string{"affected_system", "数据库集群"})),
		finalReply("方便说下是哪个系统吗？"),
		toolCall("c2", ToolCreateConfirm, confirmArgs("工单", "很卡", "incident",
			[2]string{"affected_system", "数据库集群"})),
	}, true)
	conv := h.mustStart(t)

	h.send(t, conv, "帮我建个工单")
	h.send(t, conv, "就是很卡，你看着办")

	if !h.awaiting(conv) {
		t.Fatal("问过仍空应放弃追问并起草（不再挤牙膏）")
	}
	missing := pendingMissing(t, h, conv)
	if !hasNote(missing, "missing_slot:affected_system") {
		t.Errorf("放弃追问的阻塞槽位应记入 MissingInfo，实际 %v", missing)
	}

	h.send(t, conv, "确认")
	tickets := h.store.ListTickets()
	if len(tickets) != 1 {
		t.Fatalf("应建单 1 张，实际 %d", len(tickets))
	}
	if !hasNote(tickets[0].MissingInfo, "missing_slot:affected_system") {
		t.Errorf("建出的单应携带缺失标注，实际 %v", tickets[0].MissingInfo)
	}
}

// Case 5：模型给不出可追溯 quote → 该槽判空触发追问；对照可追溯 quote 则直接起草。
// 证明承重的是"可追溯性"，而非"模型有没有提到这个槽位"。
func TestIntakeCase5UntracedQuoteTriggersAsk(t *testing.T) {
	hA := intakeHarness(t, []*llm.Response{
		toolCall("c1", ToolCreateConfirm, confirmArgs("工单", "", "incident",
			[2]string{"affected_system", "订单中台"})),
		finalReply("是哪个系统呢？"),
	}, true)
	convA := hA.mustStart(t)
	hA.send(t, convA, "帮我建个工单")
	if _, ok := hA.store.FindPendingInterrupt(convA); ok {
		t.Error("quote 不可追溯时不应起草")
	}
	if c := hA.lastTurnToolCalls(); len(c) == 0 || c[0].Status != "awaiting_user_info" {
		t.Errorf("不可追溯的 quote 应触发追问，实际 %+v", c)
	}

	hB := intakeHarness(t, []*llm.Response{
		toolCall("c1", ToolCreateConfirm, confirmArgs("工单", "订单中台超时", "incident",
			[2]string{"affected_system", "订单中台超时"})),
	}, true)
	convB := hB.mustStart(t)
	resB := hB.send(t, convB, "订单中台超时了，帮我建单")
	if !resB.Turn.Interrupted {
		t.Error("可追溯的 quote 应直接起草，不应追问")
	}
}

// Case 6：用户中途"直接建单" → forceCommit 生效，用当前草案立即建单、不调模型。
func TestIntakeCase6ForceCommit(t *testing.T) {
	h := intakeHarness(t, []*llm.Response{
		toolCall("c1", ToolCreateConfirm, confirmArgs("工单", "打印机连不上", "incident",
			[2]string{"affected_system", "打印机连不上"})),
	}, true)
	conv := h.mustStart(t)
	h.send(t, conv, "打印机连不上，帮我建个工单")
	if !h.awaiting(conv) {
		t.Fatal("应先起草待确认")
	}

	callsBefore := h.model.requestCount()
	res := h.send(t, conv, "直接建单")
	if res.Turn.TicketID == 0 {
		t.Fatal("forceCommit 应立即建单并返回工单 ID")
	}
	if after := h.model.requestCount(); after != callsBefore {
		t.Errorf("forceCommit 是确定性判定，不应调用模型，调用次数 %d → %d", callsBefore, after)
	}
	tickets := h.store.ListTickets()
	if len(tickets) != 1 {
		t.Fatalf("应建单 1 张，实际 %d", len(tickets))
	}
	if !hasNote(tickets[0].MissingInfo, missingUserForcedCommit) {
		t.Errorf("forceCommit 应记 %q，实际 %v", missingUserForcedCommit, tickets[0].MissingInfo)
	}
}

// Case 7：描述含用户没说过的声明 → 标注 fidelity_unverified，但仍照常建单。
func TestIntakeCase7FidelityAnnotatesButStillCreates(t *testing.T) {
	// Gating 关闭：忠实度只标注、不拦单，也不受追问门影响。
	h := intakeHarness(t, []*llm.Response{
		toolCall("c1", ToolCreateConfirm, confirmArgs("网络问题", "我已重启服务器。已联系财务确认预算。", "incident",
			[2]string{"affected_system", "网络时不时断开"})),
	}, false)
	conv := h.mustStart(t)
	res := h.send(t, conv, "网络时不时断开，帮我建个单")
	if !res.Turn.Interrupted {
		t.Fatal("忠实度问题不应拦单，应照常起草")
	}
	if !strings.Contains(res.Turn.Reply, "请核对是否属实") {
		t.Errorf("确认文案应含忠实度提示，实际 %q", res.Turn.Reply)
	}
	if !strings.Contains(res.Turn.Reply, "重启服务器") {
		t.Errorf("确认文案应点出编造声明，实际 %q", res.Turn.Reply)
	}
	missing := pendingMissing(t, h, conv)
	if !hasNote(missing, missingFidelityUnverified) {
		t.Errorf("应记 %q，实际 %v", missingFidelityUnverified, missing)
	}

	h.send(t, conv, "确认")
	if got := len(h.store.ListTickets()); got != 1 {
		t.Fatalf("标注忠实度问题后仍应建单，实际 %d 张", got)
	}
}

// Case 8：中途换需求 → newDemand 交回模型重拟，不拿旧草案建单（回归，不新增判据）。
func TestIntakeCase8NewDemandRegression(t *testing.T) {
	h := intakeHarness(t, []*llm.Response{
		toolCall("c1", ToolCreateConfirm, confirmArgs("第一份草案", "系统A出问题", "incident",
			[2]string{"affected_system", "系统A出问题"})),
		toolCall("c2", ToolCreateConfirm, confirmArgs("登录一直转圈", "登录页一直转圈", "incident",
			[2]string{"affected_system", "登录页一直转圈"})),
	}, true)
	conv := h.mustStart(t)
	h.send(t, conv, "系统A出问题了，帮我建个工单")

	h.send(t, conv, "还是没弄好，帮我建个单跟进登录页一直转圈")
	if got := len(h.store.ListTickets()); got != 0 {
		t.Fatalf("newDemand 消息本身不应直接建单，实际 %d", got)
	}
	p, ok := h.store.FindPendingInterrupt(conv)
	if !ok {
		t.Fatal("newDemand 应由模型重新起草并留下待确认草案")
	}
	if p.Payload.Title != "登录一直转圈" {
		t.Errorf("待确认草案必须是新拟的那份，实际《%s》", p.Payload.Title)
	}
}

// ---------- sabotage：证明每条判据确实承重 ----------

// Sabotage 1：把词元重叠阈值拉到 1.0（几乎不判 traced）。
// 对照同一"高重叠但非子串"的 claim：默认阈值判 traced，1.0 阈值判 unverified。
// 若阈值不是承重项，二者结论会一致。
func TestSabotage1ThresholdIsLoadBearing(t *testing.T) {
	claim := "支付系统报500错误"
	// 语料覆盖 claim 的大部分词元（重叠率 6/8=0.75），但缺 "错"/"误"，
	// 且归一化后不构成子串 —— 于是结论完全由阈值决定：0.70 判 traced，1.0 判 unverified。
	corpus := []string{"500报系统支付支"}

	loose := NewScriptedTraceabilityChecker()
	strict := NewRuleTraceabilityChecker(TraceabilityConfig{TokenOverlapThreshold: 1.0})

	verdictLoose, _ := loose.CheckAll(context.Background(), []Claim{{ID: "c", Text: claim}}, corpus)
	verdictStrict, _ := strict.CheckAll(context.Background(), []Claim{{ID: "c", Text: claim}}, corpus)

	if verdictLoose[0].Verdict != VerdictTraced {
		t.Fatalf("前置不成立：默认阈值应判 traced，实际 %s", verdictLoose[0].Verdict)
	}
	if verdictStrict[0].Verdict != VerdictUnverified {
		t.Errorf("阈值 1.0 应把该 claim 判 unverified（证明阈值承重），实际 %s", verdictStrict[0].Verdict)
	}
}

// Sabotage 2：去掉 forceCommit 这条出口——用一条不命中 forceCommit 的口语答复，
// 待确认草案不会因"用户似乎想推进"而建单。与 Case 6 的正向形成对照。
func TestSabotage2WithoutForceCommitPhraseDraftDoesNotBuild(t *testing.T) {
	h := intakeHarness(t, []*llm.Response{
		toolCall("c1", ToolCreateConfirm, confirmArgs("工单", "打印机连不上", "incident",
			[2]string{"affected_system", "打印机连不上"})),
		finalReply("嗯，您先看看这份草案对不对？"),
	}, true)
	conv := h.mustStart(t)
	h.send(t, conv, "打印机连不上，帮我建个工单")

	// "嗯，那个啥" 既非确认也非取消，也不含 forceCommit 词 → 保持待确认、不建单。
	h.send(t, conv, "嗯，那个啥")
	if got := len(h.store.ListTickets()); got != 0 {
		t.Fatalf("非授权答复不应建单，实际 %d", got)
	}
	if !h.awaiting(conv) {
		t.Error("草案应仍待确认，供用户之后明确表态")
	}
}

// Sabotage 3：关掉安全阀（maxRounds=0）且模拟"进度从未推进"的 bug——
// 每轮都传零进度进去，则同一空槽会被无限追问。证明"进度推进 + 安全阀"才是终止保证。
func TestSabotage3NoProgressNoValveAsksForever(t *testing.T) {
	const rounds = 6
	asked := 0
	for i := 0; i < rounds; i++ {
		// 缺陷模拟：进度没被跨轮写回（每轮都是空进度）。
		got := decideIntake([]string{"affected_system"}, domain.IntakeProgress{}, 0)
		if got.Action != intakeAsk {
			t.Fatalf("无进度、无安全阀时第 %d 轮本应无限追问（这正是缺陷），实际 %v", i+1, got.Action)
		}
		asked++
	}
	if asked != rounds {
		t.Fatalf("期望被追问 %d 次仍未终止，实际 %d", rounds, asked)
	}
	// 有安全阀（maxRounds 有限）时，同样的"无进度"输入会在触顶后被强制建单。
	got := decideIntake([]string{"affected_system"}, domain.IntakeProgress{AskRounds: 3}, 3)
	if got.Action != intakeCreate || !got.SafetyValve {
		t.Errorf("安全阀应兜住无进展的追问，实际 action=%v valve=%v", got.Action, got.SafetyValve)
	}
}
