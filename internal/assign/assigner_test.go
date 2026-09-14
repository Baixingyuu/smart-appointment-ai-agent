package assign

import (
	"math"
	"testing"

	"github.com/mac/agentdesk/internal/domain"
)

// approx 比较浮点数，容忍浮点累加误差。
func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func ticket(id int64, required ...int64) domain.Ticket {
	return domain.Ticket{
		ID:            id,
		Title:         "测试工单",
		Category:      domain.CategoryIncident,
		Priority:      domain.PriorityP2,
		Status:        domain.TicketStatusPending,
		RequiredSkill: domain.NewSkillSet(required...),
	}
}

func employee(id int64, load, maxConcurrent int, recency float64, skills ...int64) domain.Employee {
	return domain.Employee{
		ID:            id,
		Name:          "员工" + string(rune('A'+id-1)),
		Active:        true,
		Skills:        domain.NewSkillSet(skills...),
		CurrentLoad:   load,
		MaxConcurrent: maxConcurrent,
		Recency:       recency,
	}
}

func TestAssignPrefersSkillMatch(t *testing.T) {
	// 专才（技能完全覆盖）对上通才（部分覆盖），负载与响应相同。
	// 期望专才胜出：技能项必须真正参与评分。
	emps := []domain.Employee{
		employee(1, 1, 5, 0.8, 1, 2),
		employee(2, 1, 5, 0.8, 1, 3),
	}
	log := New(DefaultWeights()).Assign(ticket(100, 1, 2), emps)

	if log.Outcome != domain.OutcomeMatched {
		t.Fatalf("期望 matched，实际 %s（理由：%s）", log.Outcome, log.Reason)
	}
	if log.AssigneeID != 1 {
		t.Fatalf("期望指派员工 1（技能完全匹配），实际 %d；理由：%s", log.AssigneeID, log.Reason)
	}
}

func TestAssignSkipsFullEmployeeBeforeScoring(t *testing.T) {
	// 技能最匹配的员工已达并发上限，必须被过滤，由次优但可用者承接。
	// 这验证「过滤先于评分」，否则满载专才会抢走工单。
	busy := employee(1, 5, 5, 1.0, 1, 2, 3)
	busy.Name = "满载专才"
	available := employee(2, 0, 5, 0.5, 1)
	available.Name = "可用通才"

	log := New(DefaultWeights()).Assign(ticket(101, 1, 2), []domain.Employee{busy, available})

	if log.AssigneeID != 2 {
		t.Fatalf("期望指派可用的员工 2，实际 %d；理由：%s", log.AssigneeID, log.Reason)
	}
	// 被过滤者仍需出现在审计明细里，且带上过滤原因。
	var found bool
	for _, c := range log.Candidates {
		if c.EmployeeID == 1 {
			found = true
			if c.Active() {
				t.Error("员工 1 应被标记为已过滤")
			}
			if c.FilterReason == "" {
				t.Error("被过滤的候选人必须有过滤原因")
			}
		}
	}
	if !found {
		t.Error("被过滤的候选人仍须记录在 Candidates 中以便审计")
	}
}

func TestAssignTieBrokenByLowerLoad(t *testing.T) {
	// 技能与响应完全相同，负载更低者必须胜出。
	emps := []domain.Employee{
		employee(1, 2, 4, 0.5, 1, 2), // load ratio 0.5
		employee(2, 1, 4, 0.5, 1, 2), // load ratio 0.25
	}
	log := New(DefaultWeights()).Assign(ticket(102, 1, 2), emps)
	if log.AssigneeID != 2 {
		t.Fatalf("期望由负载更低的员工 2 胜出，实际 %d；理由：%s", log.AssigneeID, log.Reason)
	}
}

func TestAssignTieBrokenByRecency(t *testing.T) {
	// 技能与负载完全相同，最近响应更优者必须胜出。
	emps := []domain.Employee{
		employee(1, 1, 4, 0.3, 1, 2),
		employee(2, 1, 4, 0.9, 1, 2),
	}
	log := New(DefaultWeights()).Assign(ticket(103, 1, 2), emps)
	if log.AssigneeID != 2 {
		t.Fatalf("期望由最近响应更优的员工 2 胜出，实际 %d；理由：%s", log.AssigneeID, log.Reason)
	}
}

func TestAssignTieFallsBackToLowestID(t *testing.T) {
	// 总分、负载、响应全部相同：平局必须以最小 ID 收敛，
	// 否则排序不是全序，结果会随输入顺序漂移。
	emps := []domain.Employee{
		employee(7, 1, 4, 0.5, 1, 2),
		employee(3, 1, 4, 0.5, 1, 2),
	}
	log := New(DefaultWeights()).Assign(ticket(104, 1, 2), emps)
	if log.AssigneeID != 3 {
		t.Fatalf("完全平局时期望最小 ID（3）胜出，实际 %d", log.AssigneeID)
	}
}

func TestAssignIsOrderIndependent(t *testing.T) {
	// 派单结果不得依赖输入顺序 —— 这是「确定性可复现」的核心断言。
	// 否则同一工单在不同查询顺序下会被派给不同人，评测也失去意义。
	a := employee(1, 1, 4, 0.5, 1, 2)
	b := employee(2, 1, 4, 0.5, 1, 2)
	c := employee(3, 0, 4, 0.8, 2, 3)

	assigner := New(DefaultWeights())
	first := assigner.Assign(ticket(105, 1, 2), []domain.Employee{a, b, c})
	second := assigner.Assign(ticket(105, 1, 2), []domain.Employee{c, b, a})
	third := assigner.Assign(ticket(105, 1, 2), []domain.Employee{b, a, c})

	if first.AssigneeID != second.AssigneeID || second.AssigneeID != third.AssigneeID {
		t.Fatalf("结果依赖输入顺序：%d / %d / %d", first.AssigneeID, second.AssigneeID, third.AssigneeID)
	}
	if first.Score != second.Score || second.Score != third.Score {
		t.Fatalf("总分依赖输入顺序：%v / %v / %v", first.Score, second.Score, third.Score)
	}
}

func TestAssignNoSkillOverlapGoesToFallbackPool(t *testing.T) {
	// 无人具备需求技能时，不得把工单派给「最闲的无关员工」，
	// 而应退回待认领池，同时给出建议人选供人工参考。
	emps := []domain.Employee{
		employee(1, 0, 5, 0.9, 7, 8),
		employee(2, 1, 5, 0.5, 9),
	}
	log := New(DefaultWeights()).Assign(ticket(106, 1, 2), emps)

	if log.Outcome != domain.OutcomeFallbackPool {
		t.Fatalf("期望 fallback_pool，实际 %s；理由：%s", log.Outcome, log.Reason)
	}
	if log.AssigneeID == 0 {
		t.Error("兜底时仍应给出建议人选，便于人工决策")
	}
	if log.Reason == "" {
		t.Error("兜底必须有可读理由")
	}
}

func TestAssignNoCandidateWhenAllUnavailable(t *testing.T) {
	offline := employee(1, 0, 5, 0.9, 1, 2)
	offline.Active = false
	full := employee(2, 5, 5, 0.9, 1, 2)

	log := New(DefaultWeights()).Assign(ticket(107, 1, 2), []domain.Employee{offline, full})

	if log.Outcome != domain.OutcomeNoCandidate {
		t.Fatalf("期望 no_candidate，实际 %s", log.Outcome)
	}
	if log.AssigneeID != 0 {
		t.Fatalf("无候选人时不应指派任何人，实际 %d", log.AssigneeID)
	}
	if len(log.Candidates) != 2 {
		t.Fatalf("被过滤者仍须记录，期望 2 条候选明细，实际 %d", len(log.Candidates))
	}
}

func TestAssignUnlimitedCapacityIsAvailable(t *testing.T) {
	// MaxConcurrent <= 0 表示不限并发，此时不应因负载被过滤。
	unlimited := employee(1, 99, 0, 0.5, 1, 2)
	log := New(DefaultWeights()).Assign(ticket(108, 1), []domain.Employee{unlimited})
	if log.Outcome != domain.OutcomeMatched || log.AssigneeID != 1 {
		t.Fatalf("不限并发的员工应可承接，实际 outcome=%s assignee=%d", log.Outcome, log.AssigneeID)
	}
}

func TestScoreArithmeticAndWeights(t *testing.T) {
	// 断言评分公式本身，避免权重被悄悄改动而无人察觉。
	// 需求技能 {1,2}；员工技能 {1,2,3} → Jaccard = 2/3。
	// load 1/4 → loadScore 0.75；recency 0.6。
	emp := employee(1, 1, 4, 0.6, 1, 2, 3)
	log := New(DefaultWeights()).Assign(ticket(109, 1, 2), []domain.Employee{emp})

	var got domain.CandidateScore
	for _, c := range log.Candidates {
		if c.EmployeeID == 1 {
			got = c
		}
	}
	wantSkill := 2.0 / 3.0
	if !approx(got.SkillScore, wantSkill) {
		t.Errorf("技能分：期望 %.6f，实际 %.6f", wantSkill, got.SkillScore)
	}
	if !approx(got.LoadScore, 0.75) {
		t.Errorf("负载分：期望 0.75，实际 %.6f", got.LoadScore)
	}
	w := DefaultWeights()
	wantTotal := w.Skill*wantSkill + w.Load*0.75 + w.Recency*0.6
	if !approx(got.Total, wantTotal) {
		t.Errorf("总分：期望 %.6f，实际 %.6f", wantTotal, got.Total)
	}
}

func TestDefaultWeightsFavorSkillOverLoad(t *testing.T) {
	// 权重必须让技能占主导，否则「派给有相关经验的人」这个目标无法实现。
	// 教训来源：等权时把技能权重置零，评测集仍全部通过 —— 说明技能项
	// 对结果几无区分力（见 internal/eval 的区分力用例）。
	w := DefaultWeights()
	if w.Skill <= w.Load || w.Skill <= w.Recency {
		t.Fatalf("技能权重必须高于负载与响应，实际 skill=%.2f load=%.2f recency=%.2f",
			w.Skill, w.Load, w.Recency)
	}
	if !approx(w.Skill+w.Load+w.Recency, 1.0) {
		t.Errorf("权重之和应为 1，实际 %.6f", w.Skill+w.Load+w.Recency)
	}
}

func TestSkillDominanceOvercomesIdleRival(t *testing.T) {
	// 业务场景：技能匹配者比较忙（负载 4/5），竞争者很闲（0/5）但技能无关。
	// 在技能主导的权重下，必须选技能匹配者 —— 这正是「派给有相关经验的人」
	// 与「派给最闲的人」的分界，也是对抗样本集要守住的性质。
	specialist := employee(1, 4, 5, 0.5, 2, 3)
	specialist.Name = "接口专才"
	idleRival := employee(2, 0, 5, 0.95, 6)
	idleRival.Name = "空闲无关坐席"

	log := New(DefaultWeights()).Assign(ticket(113, 2, 3), []domain.Employee{specialist, idleRival})

	if log.AssigneeID != 1 {
		t.Fatalf("技能匹配者应胜过空闲但技能无关者，实际指派 %d；理由：%s", log.AssigneeID, log.Reason)
	}
}

func TestAssignEmptyRequiredSkillsNeverMatches(t *testing.T) {
	// 工单没有技能需求时，Jaccard 返回 0，应走兜底而不是随机命中。
	// 若此处返回 matched，则「无技能需求」会被伪装成成功匹配。
	log := New(DefaultWeights()).Assign(ticket(110), []domain.Employee{employee(1, 0, 5, 0.9, 1)})
	if log.Outcome == domain.OutcomeMatched {
		t.Fatalf("无技能需求不应被视为匹配成功，实际 outcome=%s 理由=%s", log.Outcome, log.Reason)
	}
}

func TestAssignInvalidEmployeeIsFilteredNotFatal(t *testing.T) {
	// 脏数据（如负数负载）应被过滤并记录原因，不能让整个派单失败。
	bad := employee(1, -1, 5, 0.5, 1)
	good := employee(2, 0, 5, 0.5, 1)

	log := New(DefaultWeights()).Assign(ticket(111, 1), []domain.Employee{bad, good})
	if log.AssigneeID != 2 {
		t.Fatalf("脏数据员工应被跳过，期望指派员工 2，实际 %d", log.AssigneeID)
	}
	for _, c := range log.Candidates {
		if c.EmployeeID == 1 && !c.Filtered {
			t.Error("非法员工必须被标记为已过滤")
		}
	}
}

func TestAssignIsReproducibleAcrossRuns(t *testing.T) {
	// 连续多次调用必须得到完全一致的结果与理由文本。
	// 理由文本也要稳定，否则评测报告无法做 diff 对比。
	emps := []domain.Employee{
		employee(1, 1, 4, 0.5, 1, 2),
		employee(2, 2, 4, 0.7, 1),
		employee(3, 0, 4, 0.6, 2),
	}
	assigner := New(DefaultWeights())
	first := assigner.Assign(ticket(112, 1, 2), emps)
	for i := 0; i < 20; i++ {
		again := assigner.Assign(ticket(112, 1, 2), emps)
		if again.AssigneeID != first.AssigneeID || again.Reason != first.Reason || again.Score != first.Score {
			t.Fatalf("第 %d 次调用结果漂移：assignee %d→%d，reason %q→%q",
				i, first.AssigneeID, again.AssigneeID, first.Reason, again.Reason)
		}
	}
}
