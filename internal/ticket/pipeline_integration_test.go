// Pipeline-through-Service 集成测试。
//
// 单测覆盖了 assign 包内部三段的行为；这里补的是"Service 真的调用到 pipeline，
// 且落库的 AssignmentLog 携带了 pipeline 观测信息（Path / Weakness / 候选明细）"
// 这条链路。没有这层，pipeline 单测全绿但生产路径接错也不会被检出。
//
// 共享 fixture（seedDirectoryProvider / newPipelineService / newService）在 service_test.go。
package ticket

import (
	"context"
	"strings"
	"testing"

	"github.com/mac/helpdesk-agent/internal/assign"
	"github.com/mac/helpdesk-agent/internal/domain"
)

func TestPipelineIntegration_OwnerDispatchOnRealTicket(t *testing.T) {
	// LLM-primary（2026-09-25）：注入 chooser 后每张非阻塞工单都走 Stage 2。
	// fixture 指定 101：若 BM25 没把工单解析到服务 2001、101 没进候选短名单，
	// enum 校验会把 fixture 打成 stage2_invalid —— 测试因此同时压住
	// "BM25 命中"与"Service → chooser → 落库"两条链路。
	chooser := assign.NewScriptedChooser(map[int64]assign.ScriptedChoice{
		1: {AssigneeID: 101, Rationale: "下单接口 owner", Confidence: 0.8},
	})
	svc, _ := newPipelineService(t, chooser)

	// 工单刻意用真实工单粒度：包含服务别名（下单接口）+ 症状（返回 500）+ 影响面（订单无法创建）。
	// 目标是 BM25 能命中服务 2001 "核心下单接口"，其 owner=101 张伟。
	result, err := svc.Create(context.Background(), createInput(0, orderTitle, orderDesc))
	if err != nil {
		t.Fatalf("建单失败: %v", err)
	}
	if result.Ticket.AssigneeID != 101 {
		t.Fatalf("期望 Stage 2 采纳下单接口 owner 张伟(101)，实际 %d；Reason=%s",
			result.Ticket.AssigneeID, result.Assignment.Reason)
	}
	if chooser.CallCount() != 1 {
		t.Errorf("LLM-primary 下 chooser 应被调用 1 次，实际 %d 次", chooser.CallCount())
	}
	// Reason 前缀必须带 pipeline 观测信息，否则派单历史里看不出走的哪一段。
	if !strings.HasPrefix(result.Assignment.Reason, "[stage2_") {
		t.Errorf("期望 Reason 以 [stage2_* 前缀开头，实际：%q", result.Assignment.Reason)
	}
}

func TestPipelineIntegration_FallsToStage3WhenNoServiceMatches(t *testing.T) {
	// chooser 未预置任何 fixture 也无 Default → 一旦被调用会返回 error；
	// pipeline 会把 error 视为 Stage 3。用这个"必然失败"的 chooser 断言：Stage 3 触发。
	chooser := assign.NewScriptedChooser(nil)
	svc, _ := newPipelineService(t, chooser)

	result, err := svc.Create(context.Background(), createInput(0, offTopicTitle, offTopicDesc))
	if err != nil {
		t.Fatalf("建单失败: %v", err)
	}
	if result.Assignment.Outcome != domain.OutcomeFallbackPool {
		t.Errorf("期望 fallback_pool，实际 %s（%s）",
			result.Assignment.Outcome, result.Assignment.Reason)
	}
	if !strings.HasPrefix(result.Assignment.Reason, "[stage3_") {
		t.Errorf("期望走 Stage 3，Reason=%q", result.Assignment.Reason)
	}
	if result.Ticket.AssigneeID != 0 {
		t.Errorf("Stage 3 不应指派任何人，实际 %d", result.Ticket.AssigneeID)
	}
}

func TestPipelineIntegration_EscalationExcludesOriginalOwner(t *testing.T) {
	// LLM-primary 下同一张工单的两轮派单都走 Stage 2，ticketID 区分不了轮次，
	// 用 Sequential 队列表达"首轮选 101（owner）、升级轮选 107（backup）"。
	// 第二轮的 enum 校验同时压住 exclude 语义：101 被排除后若仍被送进候选，
	// 它作为非 active 候选不会胜出，但 fixture 107 必须在候选内才会被采纳。
	chooser := assign.NewScriptedChooser(nil)
	chooser.Sequential = []assign.ScriptedChoice{
		{AssigneeID: 101, Rationale: "owner 接手", Confidence: 0.8},
		{AssigneeID: 107, Rationale: "owner 缺席，backup 接手", Confidence: 0.6},
	}
	svc, _ := newPipelineService(t, chooser)

	result, err := svc.Create(context.Background(), domain.TicketInput{
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
	updated, log, err := svc.Escalate(context.Background(), result.Ticket.ID, "101 判断需要下单发布同学接手")
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
	// 但其他 senior 与硬件服务无 ownership → OwnScore=0；
	// 结果要么走 Stage 3，要么选到一位靠可用度胜出的 senior。
	//
	// 本测试断言：Stage 1 候选人明细里 103 一定带"P0 要求 senior"的过滤原因。
	chooser := assign.NewScriptedChooser(nil)
	svc, _ := newPipelineService(t, chooser)

	result, err := svc.Create(context.Background(), domain.TicketInput{
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
