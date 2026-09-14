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

func TestSkillSetJaccard(t *testing.T) {
	a := NewSkillSet(1, 2, 3)
	b := NewSkillSet(1, 2)

	// |A∩B| / |A∪B| = 2/3
	if got := a.Jaccard(b); got < 0.666 || got > 0.667 {
		t.Errorf("Jaccard 期望 0.667，实际 %f", got)
	}
	// 空集合返回 0 而非 1：空需求不应被视为「完美匹配任何人」，
	// 否则无技能工单会随机命中，掩盖匹配失效。
	if got := NewSkillSet().Jaccard(a); got != 0 {
		t.Errorf("空集合 Jaccard 应为 0，实际 %f", got)
	}
	if got := a.Jaccard(NewSkillSet()); got != 0 {
		t.Errorf("与空集合 Jaccard 应为 0，实际 %f", got)
	}
	// 无交集为 0。
	if got := NewSkillSet(1).Jaccard(NewSkillSet(9)); got != 0 {
		t.Errorf("无交集 Jaccard 应为 0，实际 %f", got)
	}

	// CoveredBy 只以自身为分母，不惩罚技能多的一方。
	if got := b.CoveredBy(a); got != 1 {
		t.Errorf("b 应被 a 完全覆盖，实际 %f", got)
	}
	if got := a.CoveredBy(b); got < 0.666 || got > 0.667 {
		t.Errorf("a 被 b 覆盖 2/3，实际 %f", got)
	}
}

func TestSkillSetIDsAreSorted(t *testing.T) {
	// 稳定输出便于日志比对与测试断言。
	ids := NewSkillSet(3, 1, 2).IDs()
	for i, want := range []int64{1, 2, 3} {
		if ids[i] != want {
			t.Fatalf("ID 应升序，位置 %d 期望 %d 实际 %d", i, want, ids[i])
		}
	}
	// 非正数应被丢弃。
	if ids := NewSkillSet(0, -1, 5).IDs(); len(ids) != 1 || ids[0] != 5 {
		t.Fatalf("非正数应被丢弃，实际 %v", ids)
	}
	if NewSkillSet().IDs() != nil {
		t.Error("空集合应返回 nil")
	}
}

func TestParseSkillIDs(t *testing.T) {
	got := ParseSkillIDs("1, 2 ,x, 0, 3")
	want := []int64{1, 2, 3}
	if len(got) != len(want) {
		t.Fatalf("期望 %v，实际 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("期望 %v，实际 %v", want, got)
		}
	}
	if ParseSkillIDs("   ") != nil {
		t.Error("空白输入应返回 nil")
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
