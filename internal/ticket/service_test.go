package ticket

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mac/helpdesk-agent/internal/assign"
	"github.com/mac/helpdesk-agent/internal/domain"
	"github.com/mac/helpdesk-agent/internal/seed"
	"github.com/mac/helpdesk-agent/internal/store"
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

// seedDirectoryProvider 从 seed 包构造一份完整 Directory：Employees + Extensions + Services。
//
// 与真实部署里"从 store / CMDB 拉"同形态 —— 换成 store-backed provider 时
// 只需替换这一个函数体，Service 侧无感。
func seedDirectoryProvider() DirectoryProvider {
	return DirectoryProviderFunc(func(emps []domain.Employee) assign.Directory {
		return assign.Directory{
			Employees:  emps,
			Extensions: seed.ExtensionsByEmployeeID(),
			Services:   seed.ServicesByID(),
		}
	})
}

// newPipelineService 建一份 store + pipeline + DirectoryProvider 齐全的 Service。
// resolver 固定用 BM25、similar 用 Noop，与真实生产一致；chooser 由调用方决定
// （传 nil 即"Stage 2 未注入"，判弱样本直落 Stage 3）。
func newPipelineService(t *testing.T, chooser assign.LLMChooser) (*Service, store.Store) {
	t.Helper()
	st := store.NewMemory()
	if err := seed.Load(st); err != nil {
		t.Fatalf("加载种子数据失败: %v", err)
	}
	pipeline := assign.NewPipeline(
		assign.NewBM25ServiceResolver(3),
		assign.NoopSimilarIndex{},
		chooser,
	)
	return New(st, pipeline,
		WithClock(fixedClock()),
		WithDirectoryProvider(seedDirectoryProvider()),
	), st
}

// newService 建一份 store + pipeline + DirectoryProvider 齐全的 Service。
//
// chooser 固定不注入：这些测试要的是确定性的 Stage 1 / Stage 3 行为，
// Stage 2 的分支由 newPipelineService（pipeline_integration_test.go）单独覆盖。
func newService(t *testing.T) (*Service, store.Store) {
	t.Helper()
	return newPipelineService(t, nil)
}

// 工单 fixture。
//
// 派单依据是「文本 → 服务解析 → 归属人」，所以措辞本身就是测试输入的一部分：
// 改服务字典（别名、关键词）时这些 fixture 必须同步复核，否则 BM25 打不到分，
// 测试会因"没命中服务"而不是"逻辑错了"失败。这类耦合是真实的，不试图掩盖。
const (
	// 命中服务 2001「核心下单接口」：owner 101 张伟（senior），backup 107 黄磊（senior）。
	orderTitle = "核心下单接口持续返回 500，订单无法创建"
	orderDesc  = "自今日 10:20 起，下单接口错误率升至 35%，全部线上用户受影响。已排查网络与机房链路正常。"

	// 命中服务 2002「报表与 BI 查询」：owner 102 王强（senior）。
	reportTitle = "月度报表查询超时，看板打不开"
	reportDesc  = "运营反馈月报生成超过 60 秒后失败，看板页面空白，导出也未完成。"

	// 刻意不含服务字典里的任何别名/关键词：应判 no_service_match → 待认领池。
	// 取自评测集 dp-v2-034，那条就是用来压 ServiceMatchFloor 的。
	offTopicTitle = "预约会议室"
	offTopicDesc  = "行政帮忙订一个明天下午 3 点的会议室。"
)

func createInput(conversationID int64, title, description string) domain.TicketInput {
	return domain.TicketInput{
		Title:          title,
		Description:    description,
		Category:       domain.CategoryIncident,
		Priority:       domain.PriorityP2,
		ConversationID: conversationID,
		SourceChannel:  "web",
	}
}

func TestCreateAssignsServiceOwnerNotBackup(t *testing.T) {
	svc, st := newService(t)

	// 下单接口的 owner 101 与 backup 107 都是 senior 且都有空余并发，
	// 归属档位必须让 owner 胜出。
	result, err := svc.Create(context.Background(), createInput(2001, orderTitle, orderDesc))
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
		t.Fatalf("期望指派服务负责人 101，实际 %d；理由：%s",
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
	result, err := svc.Create(context.Background(), createInput(2002, reportTitle, reportDesc))
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

	first, err := svc.Create(context.Background(), createInput(3001, orderTitle, orderDesc))
	if err != nil {
		t.Fatalf("首次建单失败: %v", err)
	}

	// 同一会话再建单：应命中去重，追加进展而非新建。
	second, err := svc.Create(context.Background(), createInput(3001, orderTitle, orderDesc))
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

	if _, err := svc.Create(context.Background(), createInput(4001, orderTitle, orderDesc)); err != nil {
		t.Fatalf("建单失败: %v", err)
	}
	if _, err := svc.Create(context.Background(), createInput(4002, orderTitle, orderDesc)); err != nil {
		t.Fatalf("建单失败: %v", err)
	}
	if got := len(st.ListTickets()); got != 2 {
		t.Fatalf("不同会话应各自建单，期望 2 张，实际 %d", got)
	}
}

func TestCreateAfterResolveDoesNotDedupe(t *testing.T) {
	svc, st := newService(t)

	first, err := svc.Create(context.Background(), createInput(5001, reportTitle, reportDesc))
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

	second, err := svc.Create(context.Background(), createInput(5001, reportTitle, reportDesc))
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

	input := createInput(6001, reportTitle, reportDesc)
	input.MissingInfo = []string{"复现步骤", "影响范围"}
	result, err := svc.Create(context.Background(), input)
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
			in := createInput(7001, orderTitle, orderDesc)
			in.Title = "   "
			return in
		}()},
		{"非法分类", func() domain.TicketInput {
			in := createInput(7002, orderTitle, orderDesc)
			in.Category = "unknown"
			return in
		}()},
		{"非法优先级", func() domain.TicketInput {
			in := createInput(7003, orderTitle, orderDesc)
			in.Priority = "P9"
			return in
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.Create(context.Background(), tc.input); err == nil {
				t.Fatalf("期望拒绝非法输入：%s", tc.name)
			}
		})
	}
}

func TestAcceptMovesToInProgress(t *testing.T) {
	svc, _ := newService(t)
	result, err := svc.Create(context.Background(), createInput(8001, orderTitle, orderDesc))
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
	result, err := svc.Create(context.Background(), createInput(8002, orderTitle, orderDesc))
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
	result, err := svc.Create(context.Background(), createInput(8003, orderTitle, orderDesc))
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
	result, err := svc.Create(context.Background(), createInput(8004, reportTitle, reportDesc))
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
	if _, err := svc.AssignToBest(context.Background(), result.Ticket.ID, 0); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("已完成工单重新指派应被拒绝，实际 %v", err)
	}
	if _, err := svc.Accept(result.Ticket.ID, result.Ticket.AssigneeID); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("已完成工单接单应被拒绝，实际 %v", err)
	}
}

func TestEscalateExcludesCurrentAssignee(t *testing.T) {
	svc, _ := newService(t)

	result, err := svc.Create(context.Background(), createInput(9001, orderTitle, orderDesc))
	if err != nil {
		t.Fatalf("建单失败: %v", err)
	}
	original := result.Ticket.AssigneeID
	if original == 0 {
		t.Fatal("首次派单应成功")
	}

	updated, log, err := svc.Escalate(context.Background(), result.Ticket.ID, "问题超出该处理人能力范围")
	if err != nil {
		t.Fatalf("升级失败: %v", err)
	}

	// 升级必须排除原处理人：他是这个服务的归属人，不排除就会又派回他。
	// 排除后落到谁身上取决于剩余候选的边际（可能直派也可能转人工），
	// 所以这里只断言"没有派回原处理人"，具体 winner 由集成测试用 fixture 钉死。
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
			if c.Filtered {
				if c.FilterReason == "" {
					t.Error("被排除的原处理人应有过滤原因")
				}
			} else {
				t.Error("被排除的原处理人应标记为已过滤")
			}
		}
	}
	if !found {
		t.Error("被排除的原处理人仍须记录在候选明细中")
	}
}

func TestEscalateResetsStatusAndLogs(t *testing.T) {
	svc, st := newService(t)
	result, err := svc.Create(context.Background(), createInput(9002, orderTitle, orderDesc))
	if err != nil {
		t.Fatalf("建单失败: %v", err)
	}
	if _, err := svc.Accept(result.Ticket.ID, result.Ticket.AssigneeID); err != nil {
		t.Fatalf("接单失败: %v", err)
	}
	if _, _, err := svc.Escalate(context.Background(), result.Ticket.ID, "需要更资深的处理人"); err != nil {
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

func TestFallbackPoolWhenNoServiceMatched(t *testing.T) {
	svc, _ := newService(t)

	// 文本完全落在服务字典之外：不能靠"随便找个人"把工单派出去，
	// 必须退回待认领池，并带上可归因的判弱原因。
	result, err := svc.Create(context.Background(), domain.TicketInput{
		Title:          offTopicTitle,
		Description:    offTopicDesc,
		Category:       domain.CategoryConsultation,
		Priority:       domain.PriorityP3,
		ConversationID: 9100,
		SourceChannel:  "web",
	})
	if err != nil {
		t.Fatalf("建单失败: %v", err)
	}
	if result.Assignment.Outcome != domain.OutcomeFallbackPool {
		t.Fatalf("期望退回待认领池，实际 %s（%s）", result.Assignment.Outcome, result.Assignment.Reason)
	}
	if result.Ticket.AssigneeID != 0 {
		t.Errorf("判弱样本不应被正式指派，实际 %d", result.Ticket.AssigneeID)
	}
	if !strings.HasPrefix(result.Assignment.Reason, "[stage3_") {
		t.Errorf("期望走 Stage 3，Reason=%q", result.Assignment.Reason)
	}
}

func TestNoCandidateWhenAllUnavailable(t *testing.T) {
	st := store.NewMemory()
	// 只放一个已离职员工：过滤后候选为空，应得到 no_candidate。
	offline := domain.Employee{ID: 1, Name: "已离职", Active: false, MaxConcurrent: 5}
	if err := st.SaveEmployee(offline); err != nil {
		t.Fatalf("写入员工失败: %v", err)
	}
	pipeline := assign.NewPipeline(
		assign.NewBM25ServiceResolver(3),
		assign.NoopSimilarIndex{},
		nil,
	)
	svc := New(st, pipeline, WithClock(fixedClock()), WithDirectoryProvider(seedDirectoryProvider()))

	result, err := svc.Create(context.Background(), createInput(9200, orderTitle, orderDesc))
	if err != nil {
		t.Fatalf("建单失败: %v", err)
	}
	if result.Assignment.Outcome != domain.OutcomeNoCandidate {
		t.Fatalf("期望 no_candidate，实际 %s（%s）", result.Assignment.Outcome, result.Assignment.Reason)
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

	result, err := svc.Create(context.Background(), createInput(9300, reportTitle, reportDesc))
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
	result, err := svc.Create(context.Background(), createInput(9400, orderTitle, orderDesc))
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
	result, err := svc.Create(context.Background(), createInput(9500, reportTitle, reportDesc))
	if err != nil {
		t.Fatalf("建单失败: %v", err)
	}
	if _, _, err := svc.Escalate(context.Background(), result.Ticket.ID, "换人"); err != nil {
		t.Fatalf("升级失败: %v", err)
	}

	detail, err := svc.Detail(result.Ticket.ID)
	if err != nil {
		t.Fatalf("读取详情失败: %v", err)
	}
	// 建单一次 + 升级一次 = 2 条指派记录，历史必须可追溯。
	if len(detail.Assignments) != 2 {
		t.Fatalf("期望 2 条指派历史，实际 %d", len(detail.Assignments))
	}
}

func TestSeedDemoInputsAllSucceed(t *testing.T) {
	svc, _ := newService(t)
	for i, input := range seed.DemoInputs() {
		input.ConversationID = int64(9600 + i)
		result, err := svc.Create(context.Background(), input)
		if err != nil {
			t.Fatalf("演示用例 %d 建单失败: %v", i, err)
		}
		if result.Ticket.ID == 0 {
			t.Fatalf("演示用例 %d 未生成工单", i)
		}
	}
}
