package domain

import (
	"errors"
	"testing"
	"time"
)

func TestParseConfirmationDecision(t *testing.T) {
	cases := []struct {
		text string
		want ConfirmationDecision
	}{
		// 明确确认。
		{"确认", DecisionConfirm},
		{"确定", DecisionConfirm},
		{"好的", DecisionConfirm},
		{"可以", DecisionConfirm},
		{"同意", DecisionConfirm},
		{"继续", DecisionConfirm},
		{"ok", DecisionConfirm},
		{"OK 就这样", DecisionConfirm},
		{"yes please", DecisionConfirm},

		// 明确取消。
		{"取消", DecisionCancel},
		{"不用了", DecisionCancel},
		{"不需要", DecisionCancel},
		{"算了", DecisionCancel},
		{"放弃", DecisionCancel},
		{"cancel", DecisionCancel},

		// 混合表述：取消词优先于弱确认词。
		// "不用了，确认吧" 若按确认优先会被误判为确认，从而在用户
		// 明显犹豫时建单——这是有副作用的误判，必须偏保守。
		{"不用了，确认吧", DecisionCancel},
		{"算了不用建了", DecisionCancel},

		// 拟人化改写后的评测集实际表述（eval/datasets/trajectory.json）。
		// 数据集与解析器在此双向锁定：改任何一侧都必须重跑这里，
		// 否则消息改写可能悄悄破坏确认/取消解析。
		{"嗯，确认建吧，越快越好", DecisionConfirm},
		{"好的，确认，麻烦尽快", DecisionConfirm},
		{"好的，确认，尽快安排人看下", DecisionConfirm},
		{"嗯，确认，就这样", DecisionConfirm},
		{"确认，麻烦加急处理", DecisionConfirm},
		{"好的，确认登记，端口的事我照文档先试试", DecisionConfirm},
		{"算了，先不建了，我再看看", DecisionCancel},
		{"不用了，算了吧，我等会儿再说", DecisionCancel},
		{"算了，先不建了，我再自己排查一下", DecisionCancel},
		{"先不用建了，我自己重启了一下，好像恢复了", DecisionCancel},

		// 语义不明：不得猜测，交由编排层重新提问。
		{"", DecisionUnknown},
		{"   ", DecisionUnknown},
		{"我想想", DecisionUnknown},
		{"这是什么意思", DecisionUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			if got := ParseConfirmationDecision(tc.text); got != tc.want {
				t.Fatalf("输入 %q 期望 %s，实际 %s", tc.text, tc.want, got)
			}
		})
	}
}

func TestParseConfirmationDecisionAvoidsSubstringTraps(t *testing.T) {
	// 取消词表刻意不放 "no"：它会命中 notice / nobody / normal 等无关词，
	// 把中性表述误判为取消。本用例守住这个决定。
	for _, text := range []string{"notice", "nobody", "normal", "knowledge"} {
		if got := ParseConfirmationDecision(text); got == DecisionCancel {
			t.Errorf("%q 含 \"no\" 子串，但不应被判定为取消（实际 %s）", text, got)
		}
	}
	// "yes" 是确认词，但含 "yes" 的无关词同样要警惕。
	if got := ParseConfirmationDecision("yesterday"); got == DecisionConfirm {
		t.Errorf("yesterday 不应被判定为确认（实际 %s）", got)
	}
}

// TestParseConfirmationDecisionForceCommit 锁住"永远可达的建单出口"及其优先级。
//
// 阶梯顺序：newDemand → cancel → forceCommit → confirm。forceCommit 会建单（有副作用），
// 因此取消必须排在它前面；纯祈使句（"别问了"）才判 forceCommit。
func TestParseConfirmationDecisionForceCommit(t *testing.T) {
	cases := []struct {
		text string
		want ConfirmationDecision
	}{
		// 明确的主动建单祈使句。
		{"别问了", DecisionForceCommit},
		{"直接建单", DecisionForceCommit},
		{"提交吧", DecisionForceCommit},
		{"先建单吧，细节我后面补", DecisionForceCommit},
		// 「不用再问了」以取消词「不用」开头，取消优先 → 判 cancel，
		// 这也正是 forceCommit 词表刻意不含「不用再问」的原因。
		{"不用再问了", DecisionCancel},

		// 取消优先：犹豫表述里同时有取消词时，取无副作用解释。
		{"别问了，算了吧", DecisionCancel},
		{"直接建单？算了不建了", DecisionCancel},

		// 新建单诉求优先：既像 forceCommit 又像新登记时，交回模型带上下文判断。
		{"帮我建个单，别问了", DecisionHasNewDemand},

		// 「就这样」刻意不收进 forceCommit：它偏"弱确认"，与既有确认用例
		// （"嗯，确认，就这样"）同义；单独出现时不授权建单，落 unknown 交澄清。
		{"就这样", DecisionUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			if got := ParseConfirmationDecision(tc.text); got != tc.want {
				t.Fatalf("输入 %q 期望 %s，实际 %s", tc.text, tc.want, got)
			}
		})
	}
}

// TestParseConfirmationDecisionRejectsNonApproval 锁住一类具体缺陷：
// 「确认」二字出现在中文里并不总表示批准——它常作为自述核实
// （"我确认一下再说"）或否定式（"无法确认"）的一部分出现。
// 按词表子串匹配会被判成确认，在有副作用的建单场景下即"用户没同意就建了单"。
//
// 另一类是明确的新一轮建单诉求（实测样本
// eval/samples/manual-20260920-中断吞掉新诉求与确认误判.jsonl 第 3 轮
// 「还是没弄好，帮我建个单跟进吧」）：它表达的是"要登记一件新事"，
// 而不是"批准上一轮那份草案"，因此判为 has_new_demand 交回模型带上下文重拟。
func TestParseConfirmationDecisionRejectsNonApproval(t *testing.T) {
	cases := []struct {
		text string
		want ConfirmationDecision
	}{
		// 含确认词但语义是"我自己再去核实/再想想"：不是批准。
		{"我确认一下影响范围再说", DecisionUnknown},
		{"我确认一下再回复你", DecisionUnknown},
		{"等我核实一下再说", DecisionUnknown},
		{"这个我还不确定", DecisionUnknown},
		{"不能确认，风险太大", DecisionUnknown},
		// 「同意」是确认词，但整句是否定式确认，不能被判成批准。
		{"无法确认，需要审批人同意", DecisionUnknown},
		{"没法确认，我先问问领导", DecisionUnknown},
		{"你稍等，我看看", DecisionUnknown},

		// 明确的新建单诉求：交回模型，而不是拿旧草案直接建单。
		{"还是没弄好，帮我建个单跟进", DecisionHasNewDemand},
		{"帮我建个单跟进吧", DecisionHasNewDemand},
		{"这个问题没解决，再提个工单跟进", DecisionHasNewDemand},
		{"帮我登记个工单跟进吧", DecisionHasNewDemand},
		// 连"确认"都说了，但仍夹带新的建单诉求：整句不是对旧草案的干净批准。
		{"确认。另外网络端口的问题帮我建个单", DecisionHasNewDemand},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			if got := ParseConfirmationDecision(tc.text); got != tc.want {
				t.Fatalf("输入 %q 期望 %s，实际 %s", tc.text, tc.want, got)
			}
		})
	}
}

// TestGrantsApprovalInvariant 是一条口径不变式而非逐例断言：
// 写操作（建单）的许可证只有 DecisionConfirm 与 DecisionForceCommit，
// 因此除这两者以外的任何判定都不得等同于"用户批准了这份草案"。
// 新增枚举值时必须落在这条不变式之内（见 ConfirmationDecision.GrantsApproval）。
func TestGrantsApprovalInvariant(t *testing.T) {
	probes := []string{
		"", "   ", "我想想", "我确认一下影响范围再说", "无法确认",
		"帮我建个单跟进吧", "取消", "不用了", "好的", "确认",
		"别问了", "直接建单",
	}
	for _, text := range probes {
		got := ParseConfirmationDecision(text)
		// 批准 = 判定落在 {confirm, forceCommit}；与 GrantsApproval() 必须一致。
		wantApproval := got == DecisionConfirm || got == DecisionForceCommit
		if got.GrantsApproval() != wantApproval {
			t.Errorf("%q 判定 %s：GrantsApproval()=%v 与实际不符", text, got, got.GrantsApproval())
		}
		// 且只有"好的/确认/别问了/直接建单"这几条明确表态才授权。
		approved := text == "好的" || text == "确认" || text == "别问了" || text == "直接建单"
		if wantApproval != approved {
			t.Errorf("%q 判定为 %s，批准与否不符预期", text, got)
		}
	}
}

func TestInterruptValidate(t *testing.T) {
	valid := Interrupt{
		ConversationID: 1,
		Kind:           InterruptTicketCreation,
		CheckPointID:   "confirm:1:1",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("合法中断不应报错: %v", err)
	}

	cases := []struct {
		name string
		item Interrupt
	}{
		{"缺会话", Interrupt{Kind: InterruptTicketCreation, CheckPointID: "x"}},
		{"缺 CheckPointID", Interrupt{ConversationID: 1, Kind: InterruptTicketCreation}},
		{"缺类型", Interrupt{ConversationID: 1, CheckPointID: "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.item.Validate(); err == nil {
				t.Fatalf("应拒绝：%s", tc.name)
			}
		})
	}
}

func TestInterruptCanResume(t *testing.T) {
	now := time.Date(2024, 5, 1, 10, 0, 0, 0, time.UTC)
	pending := Interrupt{
		ConversationID: 1, Kind: InterruptTicketCreation,
		CheckPointID: "x", Status: InterruptPending,
		ExpiresAt: now.Add(time.Hour),
	}

	if err := pending.CanResume(now); err != nil {
		t.Fatalf("pending 且未过期应可恢复: %v", err)
	}

	// 非 pending 状态不可恢复：这是防重复建单的最后一道闸。
	// 若已 resolved 的中断还能被恢复，用户重复回复就会重复建单。
	for _, status := range []InterruptStatus{InterruptResolved, InterruptCancelled, InterruptExpired} {
		item := pending
		item.Status = status
		if err := item.CanResume(now); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("状态 %s 不应可恢复，实际错误 %v", status, err)
		}
	}

	// 已过期不可恢复。
	expired := pending
	expired.ExpiresAt = now.Add(-time.Minute)
	if err := expired.CanResume(now); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("已过期中断不应可恢复，实际错误 %v", err)
	}
	if !expired.Expired(now) {
		t.Error("Expired 应判定为真")
	}

	// 零值 ExpiresAt 表示永不过期。
	never := pending
	never.ExpiresAt = time.Time{}
	if never.Expired(now) {
		t.Error("零值过期时间不应被判为已过期")
	}
	if err := never.CanResume(now); err != nil {
		t.Errorf("永不过期的 pending 应可恢复: %v", err)
	}
}

func TestTicketWorkflowTransitions(t *testing.T) {
	w := TicketWorkflow{}

	accepted := Ticket{
		ID: 1, Title: "t", Category: CategoryIncident,
		Priority: PriorityP2, Status: TicketStatusPending,
	}
	if err := w.CanAccept(accepted); !errors.Is(err, ErrInvalidTransition) {
		t.Error("未指派工单不可接单：没有处理人的工单进入处理中会让责任人不可追溯")
	}

	assigned := accepted
	assigned.AssigneeID = 101
	if err := w.CanAccept(assigned); err != nil {
		t.Fatalf("测试前置条件不成立: %v", err)
	}
	if err := w.CanAccept(assigned); err != nil {
		t.Errorf("已指派的 pending 工单应可接单: %v", err)
	}

	inProgress := assigned
	inProgress.Status = TicketStatusInProgress
	if err := w.CanResolve(inProgress); err != nil {
		t.Errorf("处理中工单应可完成: %v", err)
	}
	if err := w.CanAccept(inProgress); !errors.Is(err, ErrInvalidTransition) {
		t.Error("处理中工单不应可再次接单")
	}

	done := inProgress
	done.Status = TicketStatusDone
	if err := w.CanAssign(done); !errors.Is(err, ErrInvalidTransition) {
		t.Error("已完成工单不应可重新指派")
	}
	if err := w.CanEscalate(done); !errors.Is(err, ErrInvalidTransition) {
		t.Error("已完成工单不应可升级")
	}
	if err := w.CanAttachProgress(done); !errors.Is(err, ErrInvalidTransition) {
		t.Error("已完成工单不应可追加进展")
	}

	// 状态机必须显式声明：done 无出边，防止将来误加回退路径。
	if edges := w.Transitions()[TicketStatusDone]; len(edges) != 0 {
		t.Errorf("done 不应有出边，实际 %v", edges)
	}
}

func TestTicketAddMissingInfoDeduplicates(t *testing.T) {
	ticket := Ticket{ID: 1, Title: "t", Category: CategoryIncident, Priority: PriorityP2}

	if !ticket.InfoComplete() {
		t.Error("初始应视为信息完整")
	}
	ticket.AddMissingInfo("复现步骤", "  ", "影响范围")
	if ticket.InfoComplete() {
		t.Error("补充缺失项后不应仍视为完整")
	}
	if len(ticket.MissingInfo) != 2 {
		t.Fatalf("空白项应被忽略，期望 2 项，实际 %v", ticket.MissingInfo)
	}

	// 重复添加不应产生重复项。
	ticket.AddMissingInfo("复现步骤")
	if len(ticket.MissingInfo) != 2 {
		t.Fatalf("重复项应被去重，实际 %v", ticket.MissingInfo)
	}
	if !ticket.HasMissingInfo("影响范围") {
		t.Error("HasMissingInfo 应能命中已记录的字段")
	}
}

func TestPriorityWeightOrdering(t *testing.T) {
	// P0 必须权重最高：SLA 与派单排序都依赖这个顺序。
	if PriorityP0.Weight() <= PriorityP1.Weight() ||
		PriorityP1.Weight() <= PriorityP2.Weight() ||
		PriorityP2.Weight() <= PriorityP3.Weight() {
		t.Fatal("优先级权重必须严格递减")
	}
	if Priority("P9").Weight() != 0 {
		t.Error("非法优先级权重应为 0")
	}
	if Priority("P9").Valid() {
		t.Error("P9 不应是合法优先级")
	}
}
