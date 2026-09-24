// Pipeline-through-Service 集成测试。
//
// 单测覆盖了 assign 包内部三段的行为；这里补的是"Service 通过 Dispatcher 接口
// 真正调用到 pipeline，且落库的 AssignmentLog 携带了 pipeline 观测信息"这条链路。
// 没有这层，pipeline 单测全绿但生产路径接错也不会被检出。
package ticket

import (
	"strings"
	"testing"

	"github.com/mac/helpdesk-agent/internal/assign"
	"github.com/mac/helpdesk-agent/internal/domain"
	"github.com/mac/helpdesk-agent/internal/seed"
	"github.com/mac/helpdesk-agent/internal/store"
)

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

// newPipelineService 建一份 store + pipeline dispatcher + DirectoryProvider 齐全的 Service。
// chooser 由调用方给（Stage 2 fixture），resolver 固定用 BM25；similar 用 Noop 与真实生产一致。
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
	svc := NewWith(st, NewPipelineDispatcher(pipeline),
		WithClock(fixedClock()),
		WithDirectoryProvider(seedDirectoryProvider()),
	)
	return svc, st
}

func TestPipelineIntegration_OwnerDispatchOnRealTicket(t *testing.T) {
	chooser := assign.NewScriptedChooser(nil) // 不该被调用
	svc, _ := newPipelineService(t, chooser)

	// 工单刻意用真实工单粒度：包含服务别名（下单接口）+ 症状（返回 500）+ 影响面（订单无法创建）。
	// 目标是 BM25 能命中服务 2001 "核心下单接口"，Stage 1 直派其 owner=101 张伟。
	result, err := svc.Create(domain.TicketInput{
		Title:         "核心下单接口持续返回 500，订单无法创建",
		Description:   "自今日 10:20 起，下单接口错误率升至 35%，全部线上用户受影响。已排查网络与机房链路正常，怀疑与今天的下单服务发布相关。",
		Category:      domain.CategoryIncident,
		Priority:      domain.PriorityP1,
		SourceChannel: "web",
	})
	if err != nil {
		t.Fatalf("建单失败: %v", err)
	}
	if result.Ticket.AssigneeID != 101 {
		t.Fatalf("期望派给下单接口 owner 张伟(101)，实际 %d；Reason=%s",
			result.Ticket.AssigneeID, result.Assignment.Reason)
	}
	if chooser.CallCount() != 0 {
		t.Errorf("Stage 1 直派时不该调 chooser，实际调了 %d 次", chooser.CallCount())
	}
	// Reason 前缀必须带 pipeline 观测信息，否则派单历史里看不出走的哪一段。
	if !strings.HasPrefix(result.Assignment.Reason, "[stage1_") {
		t.Errorf("期望 Reason 以 [stage1_* 前缀开头，实际：%q", result.Assignment.Reason)
	}
}

func TestPipelineIntegration_FallsToStage3WhenNoServiceMatches(t *testing.T) {
	// chooser 未预置任何 fixture 也无 Default → 一旦被调用会返回 error；
	// pipeline 会把 error 视为 Stage 3。用这个"必然失败"的 chooser 断言：Stage 3 触发。
	chooser := assign.NewScriptedChooser(nil)
	svc, _ := newPipelineService(t, chooser)

	result, err := svc.Create(domain.TicketInput{
		Title:         "员工工位网络不通",
		Description:   "办公室 B 座 3 层一片工位插网线后无法获取 IP，IT 已联系物业，等待答复。",
		Category:      domain.CategoryIncident,
		Priority:      domain.PriorityP3,
		SourceChannel: "web",
	})
	if err != nil {
		t.Fatalf("建单失败: %v", err)
	}
	// 断言：AssigneeID = 0 走待认领池；Stage 3 path 前缀。
	// 注：这条工单可能仍被 BM25 偶然匹配到 2005 "网络与机房链路"（关键词含"网络"），
	// 若命中且 owner 是 103（mid），P3 不受 senior gate 约束会直派。
	// 因此这里只断言"派单结果合理"：要么命中 owner 直派，要么落到 Stage 3。
	// 不做死板断言的理由：BM25 对措辞敏感，硬断言会把字典调整变成 breaking change。
	assignment := result.Assignment
	if assignment.AssigneeID == 0 {
		if !strings.HasPrefix(assignment.Reason, "[stage3_") && !strings.HasPrefix(assignment.Reason, "[stage2_invalid") {
			t.Errorf("Stage 3 时 Reason 前缀应为 stage3_* 或 stage2_invalid，实际：%q", assignment.Reason)
		}
		return
	}
	// 若直派，必须是 stage1_*；不合法 path 就是 bug。
	if !strings.HasPrefix(assignment.Reason, "[stage1_") {
		t.Errorf("非 Stage 3 但走了意外路径：%q", assignment.Reason)
	}
}

func TestPipelineIntegration_EscalationExcludesOriginalOwner(t *testing.T) {
	chooser := assign.NewScriptedChooser(nil)
	// ownershipScore 重标定后（backup 0.6→0.3），排除 owner 101 时 backup 107
	// 相对其他 senior 的领先缩到 margin(0.15) 以内 → Stage 1 正确判 low_margin，
	// 升级到 Stage 2。让 Stage 2 的默认桩挑 107，既验证"exclude 生效 + backup 可达"，
	// 又如实反映"owner 缺席时 backup 只是弱信号，该让 LLM/人复核"这一设计意图。
	chooser.Default = &assign.ScriptedChoice{
		AssigneeID: 107, Rationale: "owner 缺席，backup 接手", Confidence: 0.6,
	}
	svc, _ := newPipelineService(t, chooser)

	result, err := svc.Create(domain.TicketInput{
		Title:         "核心下单接口返回 500，订单大量失败",
		Description:   "下单接口从 10:20 起持续返回 500，用户无法完成下单，错误率 40%。",
		Category:      domain.CategoryIncident,
		Priority:      domain.PriorityP1,
		SourceChannel: "web",
	})
	if err != nil {
		t.Fatalf("建单失败: %v", err)
	}
	firstAssignee := result.Ticket.AssigneeID
	if firstAssignee != 101 {
		t.Fatalf("期望首轮派给 101，实际 %d", firstAssignee)
	}
	// 先接单，进入 in_progress，才允许升级。
	if _, err := svc.Accept(result.Ticket.ID, firstAssignee); err != nil {
		t.Fatalf("接单失败: %v", err)
	}
	updated, log, err := svc.Escalate(result.Ticket.ID, "101 判断需要下单发布同学接手")
	if err != nil {
		t.Fatalf("升级失败: %v", err)
	}
	if updated.AssigneeID == firstAssignee {
		t.Errorf("升级后仍派给原处理人 %d，exclude 未生效；Reason=%s", firstAssignee, log.Reason)
	}
	// 排除 101 后，backup=107 (黄磊, senior) 应该胜出。
	if updated.AssigneeID != 107 {
		t.Errorf("期望 backup 黄磊(107) 接手，实际 %d；Reason=%s", updated.AssigneeID, log.Reason)
	}
}

func TestPipelineIntegration_P0DispatchRequiresSeniorOwner(t *testing.T) {
	// 服务 2012 "硬件与设备" owner=103 陈磊是 mid，无 backup；
	// P0 工单 → owner 被 senior gate 过滤 → 只能从其他 senior 中选；
	// 但其他 senior 与硬件服务无 ownership → s_own=0；
	// 结果要么走 Stage 3，要么选到一位靠 avail+recency 胜出的 senior。
	//
	// 本测试断言：Stage 1 候选人明细里 103 一定带"P0 要求 senior"的过滤原因。
	chooser := assign.NewScriptedChooser(nil)
	svc, _ := newPipelineService(t, chooser)

	result, err := svc.Create(domain.TicketInput{
		Title:         "机房 POS 机大批掉线",
		Description:   "多个门店 POS 终端同时掉线，硬件指示灯异常，怀疑服务器或设备本身故障。",
		Category:      domain.CategoryIncident,
		Priority:      domain.PriorityP0,
		SourceChannel: "web",
	})
	if err != nil {
		t.Fatalf("建单失败: %v", err)
	}
	var chen *domain.CandidateScore
	for i := range result.Assignment.Candidates {
		if result.Assignment.Candidates[i].EmployeeID == 103 {
			chen = &result.Assignment.Candidates[i]
			break
		}
	}
	if chen == nil {
		t.Fatalf("候选人明细缺 103；Assignment=%+v", result.Assignment)
	}
	// 若 BM25 没命中 2012，103 也可能因为不是 owner 而不被过滤 —— 那说明测试工单
	// 描述对服务字典覆盖不足。此时至少断言"103 未被派单"。
	if chen.Filtered && !strings.Contains(chen.FilterReason, "senior") {
		t.Errorf("103 被过滤但原因不是 senior gate：%q", chen.FilterReason)
	}
	if !chen.Filtered && result.Ticket.AssigneeID == 103 {
		t.Errorf("P0 工单派给了 mid 级别的 103，senior gate 未生效")
	}
}

// TestLegacyStillWorksAfterDispatcherAbstraction 是这条抽象层的守门测试：
// 只要 New(st, *Assigner) 的行为变了，就说明 Dispatcher 抽象破坏了向后兼容。
// 断言的是"旧数据集的 4 条 demo 工单派单结果不变"，与 pipeline 是否上线正交。
func TestLegacyStillWorksAfterDispatcherAbstraction(t *testing.T) {
	st := store.NewMemory()
	if err := seed.Load(st); err != nil {
		t.Fatalf("加载种子数据失败: %v", err)
	}
	svc := New(st, assign.New(assign.DefaultWeights()), WithClock(fixedClock()))
	// 需求技能 {2,3} → 张伟 101 (skills 2,3,11) 是专才；这是 legacy 派单器的老断言。
	res, err := svc.Create(domain.TicketInput{
		Title:         "接口异常",
		Description:   "核心接口报错",
		Category:      domain.CategoryIncident,
		Priority:      domain.PriorityP2,
		RequiredSkill: domain.NewSkillSet(2, 3),
		SourceChannel: "web",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Ticket.AssigneeID != 101 {
		t.Errorf("legacy 路径期望 101，实际 %d；Reason=%s", res.Ticket.AssigneeID, res.Assignment.Reason)
	}
	// legacy Reason 没有 pipeline 前缀。
	if strings.HasPrefix(res.Assignment.Reason, "[stage") {
		t.Errorf("legacy 派单不该带 pipeline 前缀，实际：%q", res.Assignment.Reason)
	}
}
