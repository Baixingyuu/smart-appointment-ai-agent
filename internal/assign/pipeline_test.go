package assign

import (
	"context"
	"strings"
	"testing"

	"github.com/mac/helpdesk-agent/internal/domain"
)

// ---------- 测试夹具 ----------

// fixtureDirectory 构造一份 5 员工 + 2 服务的最小可复现目录。
//
// 员工分布刻意做成：
//   - 11：senior + 服务 201 owner（本 fixture 里"应该被选中"的靶点）
//   - 12：senior + 服务 201 backup（次选）
//   - 13：mid   + 服务 202 owner（跨服务，与 201 无关）
//   - 14：junior（用于测 P0 gate 与 seniority 权重）
//   - 15：senior + inactive（用于测 empty candidates 与 Active 过滤）
//
// Load 全部 0/5 使 avail 相同；这样测试断言的是"其他特征不变时，ownership 是否决定结果"。
func fixtureDirectory() Directory {
	emps := []domain.Employee{
		{ID: 11, Name: "员工11", Active: true, MaxConcurrent: 5, Recency: 0.80},
		{ID: 12, Name: "员工12", Active: true, MaxConcurrent: 5, Recency: 0.75},
		{ID: 13, Name: "员工13", Active: true, MaxConcurrent: 5, Recency: 0.70},
		{ID: 14, Name: "员工14", Active: true, MaxConcurrent: 5, Recency: 0.60},
		{ID: 15, Name: "员工15", Active: false, MaxConcurrent: 5, Recency: 0.90},
	}
	ext11 := domain.NewEmployeeExtension(11, domain.LevelSenior, 1)
	ext11 = *ext11.GrantOwnership(201, domain.OwnershipOwner)
	ext12 := domain.NewEmployeeExtension(12, domain.LevelSenior, 1)
	ext12 = *ext12.GrantOwnership(201, domain.OwnershipBackup)
	ext13 := domain.NewEmployeeExtension(13, domain.LevelMid, 2)
	ext13 = *ext13.GrantOwnership(202, domain.OwnershipOwner)
	ext14 := domain.NewEmployeeExtension(14, domain.LevelJunior, 2)
	ext15 := domain.NewEmployeeExtension(15, domain.LevelSenior, 2)
	ext15 = *ext15.GrantOwnership(202, domain.OwnershipBackup)

	return Directory{
		Employees: emps,
		Extensions: map[int64]domain.EmployeeExtension{
			11: ext11, 12: ext12, 13: ext13, 14: ext14, 15: ext15,
		},
		Services: map[int64]domain.Service{
			201: {ID: 201, Name: "服务201", OwnerID: 11, BackupOwnerID: 12, TeamID: 1},
			202: {ID: 202, Name: "服务202", OwnerID: 13, BackupOwnerID: 15, TeamID: 2},
		},
	}
}

func fixtureTicket(id int64, priority domain.Priority) domain.Ticket {
	return domain.Ticket{
		ID:       id,
		Title:    "测试工单",
		Category: domain.CategoryIncident,
		Priority: priority,
		Status:   domain.TicketStatusPending,
	}
}

// strongServiceResolver 返回服务 201 高分命中（0.9），保证 NoServiceMatch 分支不触发。
func strongServiceResolver() StaticServiceResolver {
	return StaticServiceResolver{
		Default: []ServiceMatch{{ServiceID: 201, Score: 0.9, Reason: "fixture"}},
	}
}

// ---------- Stage 1 直出路径 ----------

func TestPipeline_Stage1OwnerWins(t *testing.T) {
	dir := fixtureDirectory()
	// 无 chooser 模式（LLM-primary 化后的 Stage 1 直出路径）：
	// 离线评测 / 无模型部署走的就是这条；强命中时 owner 直派。
	p := NewPipeline(strongServiceResolver(), NoopSimilarIndex{}, nil)

	d, err := p.Run(context.Background(), fixtureTicket(1, domain.PriorityP2), dir)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if d.AssigneeID != 11 {
		t.Fatalf("期望指派 owner 11，实际 %d（理由：%s）", d.AssigneeID, d.Reason)
	}
	if d.Outcome != domain.OutcomeMatched {
		t.Errorf("期望 matched，实际 %s", d.Outcome)
	}
	if d.Path != PathStage1Owner {
		t.Errorf("期望 path=stage1_owner，实际 %s", d.Path)
	}
	if d.Weakness != WeaknessNone {
		t.Errorf("期望 weakness=none，实际 %s", d.Weakness)
	}
}

func TestPipeline_Stage1EmptyCandidatesGoesToBlocked(t *testing.T) {
	dir := fixtureDirectory()
	// 让所有员工都不可用：清空 Active 与并发。
	for i := range dir.Employees {
		dir.Employees[i].Active = false
	}
	p := NewPipeline(strongServiceResolver(), NoopSimilarIndex{}, NewScriptedChooser(nil))

	d, err := p.Run(context.Background(), fixtureTicket(1, domain.PriorityP2), dir)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if d.Path != PathStage3Blocked {
		t.Errorf("期望 path=stage3_blocked，实际 %s", d.Path)
	}
	if d.Outcome != domain.OutcomeNoCandidate {
		t.Errorf("期望 no_candidate，实际 %s", d.Outcome)
	}
	if d.AssigneeID != 0 {
		t.Errorf("期望 assignee 为 0，实际 %d", d.AssigneeID)
	}
}

func TestPipeline_P0GateExcludesJuniorAndMid(t *testing.T) {
	dir := fixtureDirectory()
	// 让 owner 之外的员工负载全为 0，保证他们本可以因负载/响应胜出；
	// 若 P0 gate 未生效，13/14 里至少有一位能挤进候选。
	p := NewPipeline(strongServiceResolver(), NoopSimilarIndex{}, nil)

	d, err := p.Run(context.Background(), fixtureTicket(1, domain.PriorityP0), dir)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	for _, c := range d.Candidates {
		if c.EmployeeID == 13 || c.EmployeeID == 14 {
			if !c.Filtered || !strings.Contains(c.FilterReason, "senior") {
				t.Errorf("员工 %d 应被 P0 级别门槛过滤且理由含 senior，实际 filtered=%v reason=%q",
					c.EmployeeID, c.Filtered, c.FilterReason)
			}
		}
	}
	// 员工 15 已离职，被"不在职"过滤；11/12 是 senior 保留。
	if d.AssigneeID != 11 {
		t.Errorf("P0 情况下 owner 11 仍应胜出，实际 %d", d.AssigneeID)
	}
}

// ---------- 判弱 → Stage 2 ----------

func TestPipeline_NoServiceMatchEscalates(t *testing.T) {
	dir := fixtureDirectory()
	empty := StaticServiceResolver{Default: nil} // 没有服务命中
	chooser := NewScriptedChooser(map[int64]ScriptedChoice{
		1: {AssigneeID: 11, Rationale: "fixture 选择 owner", Confidence: 0.7},
	})
	p := NewPipeline(empty, NoopSimilarIndex{}, chooser)

	d, err := p.Run(context.Background(), fixtureTicket(1, domain.PriorityP2), dir)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if d.Weakness != WeaknessNoServiceMatch {
		t.Errorf("期望 weakness=no_service_match，实际 %s", d.Weakness)
	}
	if !d.Stage2Used {
		t.Errorf("期望 Stage 2 被触发，实际未触发")
	}
	if d.Path != PathStage2LLM {
		t.Errorf("期望 path=stage2_llm，实际 %s", d.Path)
	}
	if d.AssigneeID != 11 {
		t.Errorf("期望采纳 chooser 结果 11，实际 %d", d.AssigneeID)
	}
}

func TestPipeline_LowMarginEscalatesToStage2(t *testing.T) {
	dir := fixtureDirectory()
	// 演示"margin 是评测可扫描的旋钮"：ownership 重标定后 11(owner 1.0) 与
	// 12(backup 0.3) 总分差为 0.285（0.78 vs 0.495）。把 margin 抬到 0.30
	// 才判低并升级到 Stage 2；默认 0.15 下这条强命中会直派，正是重标定想要的。
	chooser := NewScriptedChooser(map[int64]ScriptedChoice{
		1: {AssigneeID: 12, Rationale: "backup 更靠近当前负载分布", Confidence: 0.55},
	})
	cfg := DefaultPipelineConfig()
	cfg.MarginThreshold = 0.30
	p := NewPipeline(strongServiceResolver(), NoopSimilarIndex{}, chooser, WithPipelineConfig(cfg))

	d, err := p.Run(context.Background(), fixtureTicket(1, domain.PriorityP2), dir)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if d.Weakness != WeaknessLowMargin {
		t.Fatalf("期望 weakness=low_margin，实际 %s（Reason=%s）", d.Weakness, d.Reason)
	}
	if d.AssigneeID != 12 {
		t.Errorf("期望 Stage 2 采纳 12，实际 %d", d.AssigneeID)
	}
}

func TestPipeline_Stage2BelowTop3(t *testing.T) {
	dir := fixtureDirectory()
	// fixture 的候选人按 Total 降序 = [11, 12, 13, 14]（15 已离职不进候选）。
	// 让 chooser 挑第 4 位（14）触发 PathStage2Below 分支。
	chooser := NewScriptedChooser(map[int64]ScriptedChoice{
		1: {AssigneeID: 14, Rationale: "选到第 4 位测试 below_top3 路径", Confidence: 0.4},
	})
	cfg := DefaultPipelineConfig()
	cfg.MarginThreshold = 0.99 // 强制升级到 Stage 2
	p := NewPipeline(strongServiceResolver(), NoopSimilarIndex{}, chooser, WithPipelineConfig(cfg))

	d, err := p.Run(context.Background(), fixtureTicket(1, domain.PriorityP2), dir)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if d.AssigneeID != 14 {
		t.Fatalf("期望采纳 14，实际 %d", d.AssigneeID)
	}
	if d.Path != PathStage2Below {
		t.Errorf("期望 path=stage2_below_top3，实际 %s", d.Path)
	}
}

// ---------- Stage 3 出口 ----------

func TestPipeline_Stage2ExplicitEscalation(t *testing.T) {
	dir := fixtureDirectory()
	chooser := NewScriptedChooser(map[int64]ScriptedChoice{
		1: {AssigneeID: EscalateHumanID, Rationale: "证据不足", Confidence: 0.2},
	})
	cfg := DefaultPipelineConfig()
	cfg.MarginThreshold = 0.99
	p := NewPipeline(strongServiceResolver(), NoopSimilarIndex{}, chooser, WithPipelineConfig(cfg))

	d, err := p.Run(context.Background(), fixtureTicket(1, domain.PriorityP2), dir)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if d.Path != PathStage3Human {
		t.Errorf("期望 path=stage3_human，实际 %s", d.Path)
	}
	if d.Outcome != domain.OutcomeFallbackPool {
		t.Errorf("期望 fallback_pool，实际 %s", d.Outcome)
	}
	if d.AssigneeID != 0 {
		t.Errorf("期望 assignee 为 0（进待认领池），实际 %d", d.AssigneeID)
	}
}

func TestPipeline_Stage2HallucinationDetected(t *testing.T) {
	dir := fixtureDirectory()
	chooser := NewScriptedChooser(map[int64]ScriptedChoice{
		1: {AssigneeID: 999, Rationale: "模型编出来的 ID", Confidence: 0.9},
	})
	cfg := DefaultPipelineConfig()
	cfg.MarginThreshold = 0.99
	p := NewPipeline(strongServiceResolver(), NoopSimilarIndex{}, chooser, WithPipelineConfig(cfg))

	d, err := p.Run(context.Background(), fixtureTicket(1, domain.PriorityP2), dir)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if d.Path != PathStage2Invalid {
		t.Errorf("期望 path=stage2_invalid，实际 %s", d.Path)
	}
	if d.Outcome != domain.OutcomeFallbackPool {
		t.Errorf("期望 fallback_pool，实际 %s", d.Outcome)
	}
	if d.AssigneeID != 0 {
		t.Errorf("幻觉 ID 被拒绝时 assignee 应为 0，实际 %d", d.AssigneeID)
	}
}

// ---------- Sabotage：证明关键不变量被破坏时评测能检出 ----------

// 关闭 ownership 加分后，owner 11 不再必然胜出；backup 12 或更低分的候选人因
// avail+recency 差异可能翻转。这条测试断言"至少 owner 不再稳赢"。
func TestSabotage_OwnershipDisabledChangesWinner(t *testing.T) {
	dir := fixtureDirectory()
	// 关键：调整 recency 让无 ownership 的员工能盖过 owner。
	// 11 是 owner（+0.4 ownership × 权重），把 13 的 recency 拉到 1.0，
	// 且 13 与 11 同 avail；这样关闭 ownership 后 13 会赢，开启时 11 稳赢。
	dir.Employees[2].Recency = 1.00 // 员工 13
	// 11 与 13 的 seniority 差：11=1.0, 13=0.5 → +0.05 给 11
	// ownership：11=1.0, 13=0 → +0.40
	// recency：11=0.8, 13=1.0 → +0.02 给 13
	// avail：都 1.0 → 打平
	// sim：都 0
	// 开启 ownership 时：11=0.4+0.2+0.1+0.08=0.78；13=0+0.2+0.05+0.1=0.35 → 11 胜
	// 关闭 ownership 时：11=0.2+0.1+0.08=0.38；13=0.2+0.05+0.1=0.35 → 11 仍胜
	// 为了让翻转必须触发，再加一位无 ownership 但 recency 高的员工。
	// 简单起见把 14 也拉到 recency=1.0，且 14 是 mid 而非 junior；
	// 但 14 是 junior 会被 P0 gate 拦，P2 不受影响。
	dir.Employees[3].Recency = 1.00 // 员工 14

	// 无 chooser 模式：本测的是 Stage 1 打分特征是否承重，直出路径才反映排序变化。
	normal := NewPipeline(strongServiceResolver(), NoopSimilarIndex{}, nil)
	normalWinner := mustRun(t, normal, dir)

	sab := NewPipeline(strongServiceResolver(), NoopSimilarIndex{}, nil,
		WithoutOwnershipBoost())
	sabWinner := mustRun(t, sab, dir)

	if normalWinner.AssigneeID != 11 {
		t.Fatalf("基线：期望 11 胜出，实际 %d（Reason=%s）", normalWinner.AssigneeID, normalWinner.Reason)
	}
	if sabWinner.AssigneeID == 11 {
		t.Fatalf("sabotage 未改变结果：关闭 ownership 后仍指派 11 —— 说明该特征在测试数据里不承重")
	}
}

// Margin 阈值自 2026-09-25 起不再承重（降级为观测标注）：注入 chooser 后无论
// margin 高低，每张非阻塞工单都走 Stage 2。这条测试守住该降级不被悄悄回退——
// 若有人把 margin 重新变成"闸门"（margin=0 时跳过 Stage 2），这里会失败。
func TestPipeline_MarginNoLongerGatesStage2(t *testing.T) {
	dir := fixtureDirectory()
	chooser := NewScriptedChooser(map[int64]ScriptedChoice{
		1: {AssigneeID: 11, Rationale: "owner", Confidence: 0.7},
	})
	// margin=0（旧语义下"从不升级"）+ 强命中样本：Stage 2 仍必须被调用。
	cfg := DefaultPipelineConfig()
	cfg.MarginThreshold = 0
	p := NewPipeline(strongServiceResolver(), NoopSimilarIndex{}, chooser, WithPipelineConfig(cfg))

	d := mustRun(t, p, dir)
	if !d.Stage2Used {
		t.Fatalf("margin 已降级为观测：注入 chooser 时强命中也须走 Stage 2，实际 path=%s", d.Path)
	}
	if chooser.CallCount() != 1 {
		t.Errorf("期望 chooser 被调用 1 次，实际 %d 次", chooser.CallCount())
	}
	if d.Weakness != WeaknessNone {
		t.Errorf("强命中样本的 weakness 观测应为 none，实际 %s", d.Weakness)
	}
}

// 关闭 enum 校验后，模型幻觉 ID 会被"成功采纳"，Outcome 被误报为 matched。
// 与 HallucinationDetected 成对：证明这条守卫是承重的。
func TestSabotage_DisableEnumGuardLeedsToBadOutcome(t *testing.T) {
	dir := fixtureDirectory()
	chooser := NewScriptedChooser(map[int64]ScriptedChoice{
		1: {AssigneeID: 999, Rationale: "编出来的 ID", Confidence: 0.9},
	})
	cfg := DefaultPipelineConfig()
	cfg.MarginThreshold = 0.99
	p := NewPipeline(strongServiceResolver(), NoopSimilarIndex{}, chooser,
		WithPipelineConfig(cfg), WithoutLLMEnumGuard())

	d := mustRun(t, p, dir)
	if d.Outcome != domain.OutcomeMatched {
		t.Errorf("sabotage：期望 matched（失控路径），实际 %s", d.Outcome)
	}
	if d.AssigneeID != 999 {
		t.Errorf("sabotage：期望 999 被直接落到 assignee（无校验），实际 %d", d.AssigneeID)
	}
	// 关键断言：Candidates 明细里根本没有 999 —— 这就是"Outcome=matched 但指派对象不存在"
	// 的失控形态。若未来重构让这条不成立，说明 pipeline 加了隐式校验，测试应更新。
	for _, c := range d.Candidates {
		if c.EmployeeID == 999 {
			t.Fatalf("sabotage 语义漂移：Candidates 明细里出现了 999，说明校验未真正关闭")
		}
	}
}

// ---------- 决策 → 存储映射 ----------

func TestDecisionToAssignmentLogMapsCoreFields(t *testing.T) {
	dir := fixtureDirectory()
	p := NewPipeline(strongServiceResolver(), NoopSimilarIndex{}, nil)
	d := mustRun(t, p, dir)

	log := d.ToAssignmentLog()
	if log.AssigneeID != 11 {
		t.Errorf("AssigneeID 未映射：期望 11，实际 %d", log.AssigneeID)
	}
	if log.Outcome != domain.OutcomeMatched {
		t.Errorf("Outcome 未映射：期望 matched，实际 %s", log.Outcome)
	}
	if len(log.Candidates) != len(d.Candidates) {
		t.Errorf("Candidates 长度不匹配：%d vs %d", len(log.Candidates), len(d.Candidates))
	}
	// 15 已离职，应作为 filtered 出现在明细中。
	var sawFilteredInactive bool
	for _, c := range log.Candidates {
		if c.EmployeeID == 15 && c.Filtered && strings.Contains(c.FilterReason, "不在职") {
			sawFilteredInactive = true
		}
	}
	if !sawFilteredInactive {
		t.Errorf("被过滤的离职员工未出现在 AssignmentLog.Candidates 中")
	}
}

// ---------- 配置校验 ----------

func TestPipelineConfigValidate(t *testing.T) {
	bad := []PipelineConfig{
		{MarginThreshold: -0.1, ServiceMatchFloor: 0.4, TopKServices: 3, TopKCandidates: 5},
		{MarginThreshold: 0.15, ServiceMatchFloor: 0, TopKServices: 3, TopKCandidates: 5},
		{MarginThreshold: 0.15, ServiceMatchFloor: 0.4, TopKServices: 0, TopKCandidates: 5},
		{MarginThreshold: 0.15, ServiceMatchFloor: 0.4, TopKServices: 3, TopKCandidates: 0},
	}
	for i, cfg := range bad {
		if err := cfg.Validate(); err == nil {
			t.Errorf("case %d：期望 Validate 报错，实际通过（cfg=%+v）", i, cfg)
		}
	}
	// MarginThreshold=0 是合法配置（意为"从不因 margin 判弱"），sabotage 依赖它。
	zero := DefaultPipelineConfig()
	zero.MarginThreshold = 0
	if err := zero.Validate(); err != nil {
		t.Errorf("MarginThreshold=0 应合法，实际报 %v", err)
	}
}

// ---------- 相似索引命中 ----------

func TestPipeline_SimilarBoostsResolver(t *testing.T) {
	dir := fixtureDirectory()
	// 员工 12 已经因为 backup 排在 11 之后；把相似历史指向 12，且让 11 未命中相似。
	// 期望：12 分数上升（+0.20 × sim）；若与 11 差距缩到 margin 以内则走 Stage 2。
	similar := StaticSimilarIndex{
		ByTicket: map[int64][]SimilarHit{
			1: {{TicketID: 800, ResolverID: 12, Score: 1.0}},
		},
	}
	chooser := NewScriptedChooser(map[int64]ScriptedChoice{
		1: {AssigneeID: 11, Rationale: "仍选 owner", Confidence: 0.7},
	})
	cfg := DefaultPipelineConfig()
	cfg.MarginThreshold = 0.20
	p := NewPipeline(strongServiceResolver(), similar, chooser, WithPipelineConfig(cfg))

	d := mustRun(t, p, dir)
	// 11: 0.4+0+0.2+0.1+0.08 = 0.78
	// 12: 0.24+0.20+0.2+0.1+0.075 = 0.815
	// 12 反超；margin = 0.815 - 0.78 = 0.035 < 0.20 → 走 Stage 2。
	if d.Weakness != WeaknessLowMargin {
		t.Fatalf("期望通过相似命中拉低差距触发 low_margin，实际 weakness=%s", d.Weakness)
	}
	var found bool
	for _, c := range d.Candidates {
		if c.EmployeeID == 12 {
			found = true
			if c.SimScore <= 0 {
				t.Errorf("12 的 SimScore 应为正，实际 %f", c.SimScore)
			}
			if c.SimilarFrom != 800 {
				t.Errorf("12 的 SimilarFrom 应为 800，实际 %d", c.SimilarFrom)
			}
		}
	}
	if !found {
		t.Fatalf("候选人中缺少 12")
	}
}

// mustRun 是测试便捷函数：调用 pipeline.Run 并在 err 上 fatal。
func mustRun(t *testing.T, p *Pipeline, dir Directory) Decision {
	t.Helper()
	d, err := p.Run(context.Background(), fixtureTicket(1, domain.PriorityP2), dir)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	return d
}
