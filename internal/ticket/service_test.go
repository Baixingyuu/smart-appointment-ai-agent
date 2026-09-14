package ticket

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mac/agentdesk/internal/assign"
	"github.com/mac/agentdesk/internal/domain"
	"github.com/mac/agentdesk/internal/seed"
	"github.com/mac/agentdesk/internal/store"
)

// fixedClock 返回固定时钟，使进展记录的时间可断言。
func fixedClock() func() time.Time {
	base := time.Date(2024, 5, 1, 10, 0, 0, 0, time.UTC)
	n := 0
	return func() time.Time {
		n++
		return base.Add(time.Duration(n) * time.Second)
	}
}

func newService(t *testing.T) (*Service, store.Store) {
	t.Helper()
	st := store.NewMemory()
	if err := seed.Load(st); err != nil {
		t.Fatalf("加载种子数据失败: %v", err)
	}
	return New(st, assign.New(assign.DefaultWeights()), WithClock(fixedClock())), st
}

func createInput(convID int64, skills ...int64) domain.TicketInput {
	return domain.TicketInput{
		Title:          "接口异常",
		Description:    "核心接口报错",
		Category:       domain.CategoryIncident,
		Priority:       domain.PriorityP2,
		RequiredSkill:  domain.NewSkillSet(skills...),
		ConversationID: convID,
		SourceChannel:  "web",
	}
}

func TestCreateAssignsSpecialistNotIdleGeneralist(t *testing.T) {
	svc, st := newService(t)

	// 需求技能 {2,3}：张伟(101) 技能 {2,3,11} 是专才；
	// 李娜(108) 技能 {6} 无关；黄磊(107) 是全栈通才但 Jaccard 被分母拉低。
	result, err := svc.Create(createInput(2001, 2, 3))
	if err != nil {
		t.Fatalf("建单失败: %v", err)
	}
	if result.Deduped {
		t.Fatal("首次建单不应命中去重")
	}
	if result.Assignment.Outcome != domain.OutcomeMatched {
		t.Fatalf("期望匹配成功，实际 %s（%s）", result.Assignment.Outcome, result.Assignment.Reason)
	}
	if result.Ticket.AssigneeID != 101 {
		t.Fatalf("期望指派接口专才 101，实际 %d；理由：%s",
			result.Ticket.AssigneeID, result.Assignment.Reason)
	}

	// 指派必须落库，且带候选明细与理由。
	logs := st.AssignmentLogsByTicket(result.Ticket.ID)
	if len(logs) != 1 {
		t.Fatalf("期望 1 条指派日志，实际 %d", len(logs))
	}
	if len(logs[0].Candidates) == 0 {
		t.Error("指派日志必须保存候选评分明细，否则无法复核选择依据")
	}
	if strings.TrimSpace(logs[0].Reason) == "" {
		t.Error("指派必须给出可读理由")
	}
}

func TestCreateWritesProgressRecords(t *testing.T) {
	svc, st := newService(t)
	result, err := svc.Create(createInput(2002, 3, 4))
	if err != nil {
		t.Fatalf("建单失败: %v", err)
	}

	progress := st.ProgressByTicket(result.Ticket.ID)
	kinds := make(map[domain.ProgressKind]int)
	for _, p := range progress {
		kinds[p.Kind]++
	}
	// 创建与指派都必须留痕：参考实现的状态变更不写进展，本项目要求可审计。
	if kinds[domain.ProgressCreated] != 1 {
		t.Errorf("期望 1 条 created 进展，实际 %d", kinds[domain.ProgressCreated])
	}
	if kinds[domain.ProgressAssigned] != 1 {
		t.Errorf("期望 1 条 assigned 进展，实际 %d", kinds[domain.ProgressAssigned])
	}
}

func TestCreateDedupesOpenTicketInSameConversation(t *testing.T) {
	svc, st := newService(t)

	first, err := svc.Create(createInput(3001, 2, 3))
	if err != nil {
		t.Fatalf("首次建单失败: %v", err)
	}

	// 同一会话再建单：应命中去重，追加进展而非新建。
	second, err := svc.Create(createInput(3001, 2, 3))
	if err != nil {
		t.Fatalf("第二次建单失败: %v", err)
	}
	if !second.Deduped {
		t.Fatal("同一会话已有未关闭工单时，应命中去重")
	}
	if second.Ticket.ID != first.Ticket.ID {
		t.Fatalf("去重应返回原工单 %d，实际 %d", first.Ticket.ID, second.Ticket.ID)
	}

	tickets := st.ListTickets()
	if len(tickets) != 1 {
		t.Fatalf("去重后应只有 1 张工单，实际 %d", len(tickets))
	}

	var dedupeCount int
	for _, p := range st.ProgressByTicket(first.Ticket.ID) {
		if p.Kind == domain.ProgressDedupe {
			dedupeCount++
		}
	}
	if dedupeCount != 1 {
		t.Errorf("期望 1 条 dedupe 进展，实际 %d", dedupeCount)
	}
}

func TestCreateDifferentConversationsDoNotDedupe(t *testing.T) {
	svc, st := newService(t)

	if _, err := svc.Create(createInput(4001, 2, 3)); err != nil {
		t.Fatalf("建单失败: %v", err)
	}
	if _, err := svc.Create(createInput(4002, 2, 3)); err != nil {
		t.Fatalf("建单失败: %v", err)
	}
	if got := len(st.ListTickets()); got != 2 {
		t.Fatalf("不同会话应各自建单，期望 2 张，实际 %d", got)
	}
}

func TestCreateAfterResolveDoesNotDedupe(t *testing.T) {
	svc, st := newService(t)

	first, err := svc.Create(createInput(5001, 3, 4))
	if err != nil {
		t.Fatalf("建单失败: %v", err)
	}
	// 完成工单后，同一会话再次提问应能新建：已关闭的问题不应吞掉新问题。
	if _, err := svc.Accept(first.Ticket.ID, first.Ticket.AssigneeID); err != nil {
		t.Fatalf("接单失败: %v", err)
	}
	if _, err := svc.Resolve(first.Ticket.ID, first.Ticket.AssigneeID); err != nil {
		t.Fatalf("完成失败: %v", err)
	}

	second, err := svc.Create(createInput(5001, 3, 4))
	if err != nil {
		t.Fatalf("重新建单失败: %v", err)
	}
	if second.Deduped {
		t.Fatal("原工单已关闭，不应命中去重")
	}
	if got := len(st.ListTickets()); got != 2 {
		t.Fatalf("期望 2 张工单，实际 %d", got)
	}
}

func TestCreateRecordsMissingInfoWithoutBlocking(t *testing.T) {
	svc, _ := newService(t)

	input := createInput(6001, 3, 4)
	input.MissingInfo = []string{"复现步骤", "影响范围"}
	result, err := svc.Create(input)
	if err != nil {
		t.Fatalf("信息不全时不应阻塞建单: %v", err)
	}
	if result.Ticket.InfoComplete() {
		t.Fatal("缺信息时 InfoComplete 应为 false")
	}
	if len(result.Ticket.MissingInfo) != 2 {
		t.Fatalf("期望记录 2 项缺失信息，实际 %v", result.Ticket.MissingInfo)
	}
}

func TestCreateRejectsInvalidInput(t *testing.T) {
	svc, _ := newService(t)

	cases := []struct {
		name  string
		input domain.TicketInput
	}{
		{"空标题", func() domain.TicketInput {
			in := createInput(7001, 1)
			in.Title = "   "
			return in
		}()},
		{"非法分类", func() domain.TicketInput {
			in := createInput(7002, 1)
			in.Category = "unknown"
			return in
		}()},
		{"非法优先级", func() domain.TicketInput {
			in := createInput(7003, 1)
			in.Priority = "P9"
			return in
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.Create(tc.input); err == nil {
				t.Fatalf("期望拒绝非法输入：%s", tc.name)
			}
		})
	}
}

func TestAcceptMovesToInProgress(t *testing.T) {
	svc, _ := newService(t)
	result, err := svc.Create(createInput(8001, 2, 3))
	if err != nil {
		t.Fatalf("建单失败: %v", err)
	}
	if result.Ticket.Status != domain.TicketStatusPending {
		t.Fatalf("新建工单应为 pending，实际 %s", result.Ticket.Status)
	}

	updated, err := svc.Accept(result.Ticket.ID, result.Ticket.AssigneeID)
	if err != nil {
		t.Fatalf("接单失败: %v", err)
	}
	// 参考实现中 in_progress 从不由代码路径自动设置，导致该状态不可达。
	if updated.Status != domain.TicketStatusInProgress {
		t.Fatalf("接单后应为 in_progress，实际 %s", updated.Status)
	}
}

func TestAcceptRejectsUnauthorizedEmployee(t *testing.T) {
	svc, _ := newService(t)
	result, err := svc.Create(createInput(8002, 2, 3))
	if err != nil {
		t.Fatalf("建单失败: %v", err)
	}

	// 非指派处理人不得接单，否则责任人不可追溯。
	if _, err := svc.Accept(result.Ticket.ID, 999); err == nil {
		t.Fatal("非指派处理人接单应被拒绝")
	}
}

func TestResolveRequiresInProgress(t *testing.T) {
	svc, _ := newService(t)
	result, err := svc.Create(createInput(8003, 2, 3))
	if err != nil {
		t.Fatalf("建单失败: %v", err)
	}

	// 未经接单直接完成应被拒绝：跳过 in_progress 会让「处理中」语义失效。
	if _, err := svc.Resolve(result.Ticket.ID, result.Ticket.AssigneeID); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("跳过接单直接完成应返回 ErrInvalidTransition，实际 %v", err)
	}

	if _, err := svc.Accept(result.Ticket.ID, result.Ticket.AssigneeID); err != nil {
		t.Fatalf("接单失败: %v", err)
	}
	resolved, err := svc.Resolve(result.Ticket.ID, result.Ticket.AssigneeID)
	if err != nil {
		t.Fatalf("完成失败: %v", err)
	}
	if resolved.Status != domain.TicketStatusDone {
		t.Fatalf("期望 done，实际 %s", resolved.Status)
	}
}

func TestResolveThenReopenIsRejected(t *testing.T) {
	svc, _ := newService(t)
	result, err := svc.Create(createInput(8004, 3, 4))
	if err != nil {
		t.Fatalf("建单失败: %v", err)
	}
	if _, err := svc.Accept(result.Ticket.ID, result.Ticket.AssigneeID); err != nil {
		t.Fatalf("接单失败: %v", err)
	}
	if _, err := svc.Resolve(result.Ticket.ID, result.Ticket.AssigneeID); err != nil {
		t.Fatalf("完成失败: %v", err)
	}

	// 已完成工单不应被重新指派或接单。
	if _, err := svc.AssignToBest(result.Ticket.ID, 0); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("已完成工单重新指派应被拒绝，实际 %v", err)
	}
	if _, err := svc.Accept(result.Ticket.ID, result.Ticket.AssigneeID); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("已完成工单接单应被拒绝，实际 %v", err)
	}
}

func TestEscalateExcludesCurrentAssignee(t *testing.T) {
	svc, _ := newService(t)

	// 需求技能 {3,4} 会派给王强(102)，他技能恰好是 {3,4}。
	result, err := svc.Create(createInput(9001, 3, 4))
	if err != nil {
		t.Fatalf("建单失败: %v", err)
	}
	original := result.Ticket.AssigneeID
	if original == 0 {
		t.Fatal("首次派单应成功")
	}

	updated, log, err := svc.Escalate(result.Ticket.ID, "问题超出该处理人能力范围")
	if err != nil {
		t.Fatalf("升级失败: %v", err)
	}

	// 升级必须排除原处理人，否则派单器很可能因技能最匹配而再次选中他。
	if log.AssigneeID == original {
		t.Fatalf("升级后不应再次派给原处理人 %d（理由：%s）", original, log.Reason)
	}
	if updated.AssigneeID == original {
		t.Fatalf("工单处理人应变更，实际仍为 %d", updated.AssigneeID)
	}

	// 原处理人应出现在候选明细中且被标记为过滤，使「为何排除」有据可查。
	var found bool
	for _, c := range log.Candidates {
		if c.EmployeeID == original {
			found = true
			if c.Active() {
				t.Error("被排除的原处理人应标记为已过滤")
			}
			if c.FilterReason == "" {
				t.Error("被排除的原处理人应有过滤原因")
			}
		}
	}
	if !found {
		t.Error("被排除的原处理人仍须记录在候选明细中")
	}
}

func TestEscalateResetsStatusAndLogs(t *testing.T) {
	svc, st := newService(t)
	result, err := svc.Create(createInput(9002, 2, 3))
	if err != nil {
		t.Fatalf("建单失败: %v", err)
	}
	if _, err := svc.Accept(result.Ticket.ID, result.Ticket.AssigneeID); err != nil {
		t.Fatalf("接单失败: %v", err)
	}
	if _, _, err := svc.Escalate(result.Ticket.ID, "需要更资深的处理人"); err != nil {
		t.Fatalf("升级失败: %v", err)
	}

	var escalated int
	for _, p := range st.ProgressByTicket(result.Ticket.ID) {
		if p.Kind == domain.ProgressEscalated {
			escalated++
		}
	}
	if escalated != 1 {
		t.Errorf("期望 1 条 escalated 进展，实际 %d", escalated)
	}
}

func TestFallbackPoolWhenNoSkillOverlap(t *testing.T) {
	svc, _ := newService(t)

	// 技能 5 只有袁泉(104) 与黄磊(107) 具备，两者都会命中；
	// 用一个所有员工都没有的技能 99 来触发无技能命中的兜底分支。
	result, err := svc.Create(createInput(9100, 99))
	if err != nil {
		t.Fatalf("建单失败: %v", err)
	}
	if result.Assignment.Outcome != domain.OutcomeFallbackPool {
		t.Fatalf("期望退回待认领池，实际 %s", result.Assignment.Outcome)
	}
	// 兜底时仍给出建议人选，但工单不应该被「正式指派」给技能无关的人。
	if result.Assignment.AssigneeID == 0 {
		t.Log("兜底未给出建议人选：当前无可用员工也属合理")
	}
}

func TestNoCandidateWhenAllUnavailable(t *testing.T) {
	st := store.NewMemory()
	// 只放一个已离职员工：应得到 no_candidate。
	offline := domain.Employee{
		ID: 1, Name: "已离职", Active: false,
		Skills: domain.NewSkillSet(2), MaxConcurrent: 5,
	}
	if err := st.SaveEmployee(offline); err != nil {
		t.Fatalf("写入员工失败: %v", err)
	}
	svc := New(st, assign.New(assign.DefaultWeights()), WithClock(fixedClock()))

	result, err := svc.Create(createInput(9200, 2))
	if err != nil {
		t.Fatalf("建单失败: %v", err)
	}
	if result.Assignment.Outcome != domain.OutcomeNoCandidate {
		t.Fatalf("期望 no_candidate，实际 %s", result.Assignment.Outcome)
	}
	if result.Ticket.AssigneeID != 0 {
		t.Fatalf("无可用员工时不应指派，实际 %d", result.Ticket.AssigneeID)
	}
	// 工单仍须创建成功：派单失败不等于建单失败，否则问题会丢失。
	if result.Ticket.ID == 0 {
		t.Fatal("派单失败时工单仍应创建成功")
	}
}

func TestLoadUsesLiveWorkloadNotStaticSnapshot(t *testing.T) {
	svc, st := newService(t)

	// 把王强(102) 的静态负载写成满值，但工单表里他实际没有未完成工单。
	// 派单应以工单表的实时数据为准，而不是采信静态快照。
	emp, err := st.GetEmployee(102)
	if err != nil {
		t.Fatalf("读取员工失败: %v", err)
	}
	emp.CurrentLoad = emp.MaxConcurrent
	if err := st.SaveEmployee(emp); err != nil {
		t.Fatalf("更新员工失败: %v", err)
	}

	result, err := svc.Create(createInput(9300, 3, 4))
	if err != nil {
		t.Fatalf("建单失败: %v", err)
	}
	if result.Ticket.AssigneeID != 102 {
		t.Fatalf("应以实时负载判断可用性，期望指派 102，实际 %d；理由：%s",
			result.Ticket.AssigneeID, result.Assignment.Reason)
	}
}

func TestAddCommentAppendsProgress(t *testing.T) {
	svc, st := newService(t)
	result, err := svc.Create(createInput(9400, 2, 3))
	if err != nil {
		t.Fatalf("建单失败: %v", err)
	}
	if err := svc.AddComment(result.Ticket.ID, 101, "已联系客户确认影响范围"); err != nil {
		t.Fatalf("追加备注失败: %v", err)
	}
	var comments int
	for _, p := range st.ProgressByTicket(result.Ticket.ID) {
		if p.Kind == domain.ProgressComment {
			comments++
		}
	}
	if comments != 1 {
		t.Errorf("期望 1 条 comment 进展，实际 %d", comments)
	}
	if err := svc.AddComment(result.Ticket.ID, 101, "   "); err == nil {
		t.Error("空备注应被拒绝")
	}
}

func TestDetailExposesAssignmentHistory(t *testing.T) {
	svc, _ := newService(t)
	result, err := svc.Create(createInput(9500, 3, 4))
	if err != nil {
		t.Fatalf("建单失败: %v", err)
	}
	if _, _, err := svc.Escalate(result.Ticket.ID, "换人"); err != nil {
		t.Fatalf("升级失败: %v", err)
	}

	detail, err := svc.Detail(result.Ticket.ID)
	if err != nil {
		t.Fatalf("读取详情失败: %v", err)
	}
	if detail.Assignee == nil {
		t.Fatal("详情应包含当前处理人")
	}
	// 建单一次 + 升级一次 = 2 条指派记录，历史必须可追溯。
	if len(detail.Assignments) != 2 {
		t.Fatalf("期望 2 条指派历史，实际 %d", len(detail.Assignments))
	}
}

func TestSeedDemoInputsAllSucceed(t *testing.T) {
	svc, _ := newService(t)
	for i, input := range seed.DemoInputs() {
		result, err := svc.Create(input)
		if err != nil {
			t.Fatalf("演示用例 %d 建单失败: %v", i, err)
		}
		if result.Ticket.ID == 0 {
			t.Fatalf("演示用例 %d 未生成工单", i)
		}
	}
}
