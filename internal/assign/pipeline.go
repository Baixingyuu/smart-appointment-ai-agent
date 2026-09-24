// Package assign 的三段流水线（Stage 1 规则+检索 / Stage 2 LLM / Stage 3 人工）。
//
// 本文件只放共享类型、配置与编排。Stage 1 五个子段各自的文件见
// resolve.go / candidates.go / eligibility.go / score.go / weakness.go；
// Stage 2 的 LLM 实现见 llm_chooser.go / scripted_chooser.go。
//
// 设计文档：docs/DISPATCH_PIPELINE.md。
//
// 与现有 Assigner 的关系：pipeline 不复用 Assigner.Assign。原因见 §设计文档 5，
// 摘要：ownership 特征是三档不同权重（owner / backup / team），
// 用 Jaccard 硬套会引入"虚拟技能"的权宜 hack；从 Assigner 借的只有
// tiebreak 顺序与 nearlyEqual 误差容忍两件确定性资产，在 score.go 重实现。
package assign

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mac/helpdesk-agent/internal/domain"
	"github.com/mac/helpdesk-agent/internal/llm"
)

// Path 记录一次派单最终走的哪一段。
//
// 必须显式建模：不同段的失败模式完全不同（Stage 1 是规则不覆盖、Stage 2 是模型能力、
// Stage 3 是运营负载），混在 Outcome 里就无法分派给不同 owner。
type Path string

const (
	PathStage1Owner   Path = "stage1_owner"      // 主负责人直接命中
	PathStage1Backup  Path = "stage1_backup"     // 备份负责人命中
	PathStage1Team    Path = "stage1_team"       // 同团队内匹配（非 owner/backup）
	PathStage1KNN     Path = "stage1_knn"        // 靠相似历史工单命中
	PathStage1Generic Path = "stage1_generic"    // 无 ownership 无相似，靠可用度胜出
	PathStage2LLM     Path = "stage2_llm"        // LLM 在 top-N 中确认
	PathStage2Below   Path = "stage2_below_top3" // LLM 选了 top-3 之外（可观测降级信号）
	PathStage2Invalid Path = "stage2_invalid"    // LLM 输出候选集外 ID，强制走 Stage 3
	PathStage3Human   Path = "stage3_human"      // 进待认领池
	PathStage3Blocked Path = "stage3_blocked"    // 全员不可用，硬阻塞
)

// WeaknessReason 是 Stage 1 → Stage 2/3 的升级原因枚举。
//
// 判弱必须是离散原因，不是连续置信度：这样"为什么升级"可评测、可回归；
// "模型觉得不太像"这类无法归因的原因不能成为升级条件。
type WeaknessReason string

const (
	WeaknessNone            WeaknessReason = ""
	WeaknessLowMargin       WeaknessReason = "low_margin"       // top1 与 top2 总分接近
	WeaknessNoServiceMatch  WeaknessReason = "no_service_match" // 服务归属解析置信不足
	WeaknessEmptyCandidates WeaknessReason = "empty_candidates" // 过滤后无候选人，直送 Stage 3
)

// FeatureWeights 是 Stage 1 打分器的五档权重。
//
// 命名带 Feature 前缀以避开与同包 Weights（旧 Assigner 用）冲突。
// Skill 权重不在此：Stage 1 不用 Jaccard 打技能分，见 §设计文档 1.4。
// 保留字段扩展位是因为真实场景里技能仍是弱特征，一期设 0 但接口先留。
type FeatureWeights struct {
	Ownership float64
	Similar   float64
	Avail     float64
	Seniority float64
	Recency   float64
}

// DefaultFeatureWeights 返回一期默认权重（0.40 / 0.20 / 0.20 / 0.10 / 0.10）。
//
// 为什么 ownership 一家独大：真实业务里"是不是这个系统的负责人"压倒其他信号，
// 其他四项是次级调节。等权会让"最闲的人"频繁盖过"最懂的人"——
// 这与现有 Assigner 的 DefaultWeights 走过的坑同源。
func DefaultFeatureWeights() FeatureWeights {
	return FeatureWeights{Ownership: 0.40, Similar: 0.20, Avail: 0.20, Seniority: 0.10, Recency: 0.10}
}

func (w FeatureWeights) sum() float64 {
	return w.Ownership + w.Similar + w.Avail + w.Seniority + w.Recency
}

// PipelineConfig 三段流水线的可调参数。
//
// 阈值类字段当前全部是"拍的"，见 §设计文档 1.5。之所以仍然全部外置，
// 是为了让 sabotage 与阈值扫描能在不改代码的前提下完成。
type PipelineConfig struct {
	MarginThreshold   float64 // top1-top2 的最小差距；低于此判为 WeaknessLowMargin
	ServiceMatchFloor float64 // 服务解析最高分下限；低于此判为 WeaknessNoServiceMatch
	TopKServices      int     // resolve 返回的服务候选条数
	TopKCandidates    int     // 送给 Stage 2 的候选人条数
	Stage2Enabled     bool    // 关闭后所有弱判定直送 Stage 3
}

// DefaultPipelineConfig 返回一期默认配置。
//
// ServiceMatchFloor=0.25 的实测依据：BM25 归一化分数在 14 条服务字典上，
// 真实强命中落在 0.30~0.40，误命中约 0.22，噪声约 0.03。
// 曾用 0.40 会拒掉"确实命中"的样本；0.25 是"至少高过误命中一档"的下界。
// 仍需用 v2 派单数据集重新校准。
func DefaultPipelineConfig() PipelineConfig {
	return PipelineConfig{
		MarginThreshold:   0.15,
		ServiceMatchFloor: 0.25,
		TopKServices:      3,
		TopKCandidates:    5,
		Stage2Enabled:     true,
	}
}

// Validate 校验参数在合理范围。
//
// 显式校验而不是 clamp：阈值配错多半是代码 bug 或运维事故，
// 静默修正会让"这个指标怎么变了"变成一场考古。
func (c PipelineConfig) Validate() error {
	if c.MarginThreshold < 0 || c.MarginThreshold > 1 {
		return fmt.Errorf("MarginThreshold %v out of [0, 1]", c.MarginThreshold)
	}
	if c.ServiceMatchFloor <= 0 || c.ServiceMatchFloor > 1 {
		return fmt.Errorf("ServiceMatchFloor %v out of (0, 1]", c.ServiceMatchFloor)
	}
	if c.TopKServices <= 0 {
		return fmt.Errorf("TopKServices must be positive, got %d", c.TopKServices)
	}
	if c.TopKCandidates <= 0 {
		return fmt.Errorf("TopKCandidates must be positive, got %d", c.TopKCandidates)
	}
	return nil
}

// Directory 一次派单查询所需的人员与服务目录。
//
// 独立于 store 层，是为了让 pipeline 单测无需 I/O：
// 直接构造一份内存 Directory 就能跑通三段。
type Directory struct {
	Employees  []domain.Employee
	Extensions map[int64]domain.EmployeeExtension // key = EmployeeID；可为 nil 表示零 ownership 特征
	Services   map[int64]domain.Service           // key = ServiceID；可为 nil 表示无服务字典
}

// ExtensionOf 查询员工扩展；不存在时返回零值 + false。
func (d Directory) ExtensionOf(employeeID int64) (domain.EmployeeExtension, bool) {
	if d.Extensions == nil {
		return domain.EmployeeExtension{}, false
	}
	ext, ok := d.Extensions[employeeID]
	return ext, ok
}

// ServiceOf 查询服务定义。
func (d Directory) ServiceOf(serviceID int64) (domain.Service, bool) {
	if d.Services == nil {
		return domain.Service{}, false
	}
	s, ok := d.Services[serviceID]
	return s, ok
}

// Candidate 是 Stage 1 打分器的输出条目，一条对应一位员工。
//
// 不叫 CandidateScore 是为了避免与 domain.CandidateScore 混淆：
// 后者字段是 SkillScore/LoadScore/RecencyScore 三段旧模型，
// 前者是新的五特征模型。二者通过 Decision.ToAssignmentLog 单向映射。
type Candidate struct {
	EmployeeID     int64
	Name           string
	OwnScore       float64
	SimScore       float64
	AvailScore     float64
	SeniorityScore float64
	RecentScore    float64
	Total          float64
	Filtered       bool
	FilterReason   string
	// OwnershipHit 记录该候选人最终匹配的 ownership 档位（未命中为 OwnershipNone）。
	OwnershipHit domain.OwnershipKind
	// SimilarFrom 记录 s_sim 来自哪个历史工单 ID（0 表示无相似命中）。
	SimilarFrom int64
}

// Active 判断候选人是否通过过滤。
func (c Candidate) Active() bool { return !c.Filtered }

// ServiceMatch 一条服务解析结果。
type ServiceMatch struct {
	ServiceID int64
	Score     float64
	Reason    string
}

// SimilarHit 一条相似历史工单命中。
type SimilarHit struct {
	TicketID   int64
	ResolverID int64
	Score      float64
}

// Decision 一次派单的完整结论。
//
// 与 domain.AssignmentLog 的关系：Decision 是 pipeline 内部的第一等产物，
// AssignmentLog 是持久化层的存储格式。ToAssignmentLog 单向映射，
// 反过来不成立 —— AssignmentLog 装不下 Path / Weakness / LLM 用量这些字段。
type Decision struct {
	AssigneeID int64
	Outcome    domain.AssignmentOutcome
	Path       Path
	Weakness   WeaknessReason
	Reason     string
	Score      float64

	Candidates []Candidate
	Services   []ServiceMatch

	// Stage 2 观测字段。Stage 1 直出时全部为零值。
	Stage2Used      bool
	Stage2Rationale string
	Stage2Confid    float64
	Stage2Usage     llm.Usage
	Stage2LatencyMS int64

	GeneratedAt time.Time
}

// ToAssignmentLog 折算为存储层可写的 AssignmentLog。
//
// 有意丢弃 Path / Weakness / Stage2 三项：这些是 pipeline 的内部观测，
// 存储层不感知。业务侧要观测 pipeline 走的是哪段的话，另建一张 pipeline_run 表，
// 比塞进 AssignmentLog.Reason 字符串更好查。
func (d Decision) ToAssignmentLog() domain.AssignmentLog {
	log := domain.AssignmentLog{
		TicketID:   0, // 由调用方在拿到 ticket 后回填
		AssigneeID: d.AssigneeID,
		Outcome:    d.Outcome,
		Reason:     d.Reason,
		Score:      d.Score,
		CreatedAt:  d.GeneratedAt,
	}
	log.Candidates = make([]domain.CandidateScore, 0, len(d.Candidates))
	for _, c := range d.Candidates {
		log.Candidates = append(log.Candidates, domain.CandidateScore{
			EmployeeID:   c.EmployeeID,
			Name:         c.Name,
			SkillScore:   c.OwnScore, // 语义近似："是否匹配需求"→ "是否是负责人"
			LoadScore:    c.AvailScore,
			RecencyScore: c.RecentScore,
			Total:        c.Total,
			Filtered:     c.Filtered,
			FilterReason: c.FilterReason,
		})
	}
	return log
}

// Pipeline 三段流水线的编排器。无状态，可并发使用。
type Pipeline struct {
	resolver ServiceResolver
	similar  SimilarTicketIndex
	chooser  LLMChooser
	weights  FeatureWeights
	config   PipelineConfig

	// sabotage 开关，仅供评测回归使用，生产保持默认 false。
	// 与现有 Assigner 的三个开关同一设计动机：直接证明"关键不变量破坏时评测能否检出"。
	forceStage2        bool
	skipOwnershipBoost bool
	allowAnyAssigneeID bool // 关闭 LLM 输出的 enum 校验
}

// PipelineOption 调整 Pipeline 行为。
type PipelineOption func(*Pipeline)

// WithFeatureWeights 覆盖 Stage 1 打分权重。
func WithFeatureWeights(w FeatureWeights) PipelineOption {
	return func(p *Pipeline) {
		if w.sum() > 0 {
			p.weights = w
		}
	}
}

// WithPipelineConfig 覆盖 Pipeline 配置。
func WithPipelineConfig(c PipelineConfig) PipelineOption {
	return func(p *Pipeline) { p.config = c }
}

// WithForcedStage2 强制所有派单走 Stage 2，用于 Stage 2 单独评测。
// 仅供评测使用；生产代码不得使用。
func WithForcedStage2() PipelineOption {
	return func(p *Pipeline) { p.forceStage2 = true }
}

// WithoutOwnershipBoost 将 ownership 权重置零。
// 仅供评测自检：破坏后必须能在指派准确率上检出。生产代码不得使用。
func WithoutOwnershipBoost() PipelineOption {
	return func(p *Pipeline) { p.skipOwnershipBoost = true }
}

// WithoutLLMEnumGuard 关闭 LLM 输出的候选集校验。
// 仅供评测自检：验证"幻觉 ID"这一失败模式的检出能力。生产代码不得使用。
func WithoutLLMEnumGuard() PipelineOption {
	return func(p *Pipeline) { p.allowAnyAssigneeID = true }
}

// NewPipeline 构造流水线。
//
// resolver 与 chooser 是硬依赖；similar 可以为 nil，视为"无相似历史索引"，
// s_sim 恒为 0 —— 一期就是这个状态（历史派单数据为空），接口先立起来。
func NewPipeline(resolver ServiceResolver, similar SimilarTicketIndex, chooser LLMChooser, opts ...PipelineOption) *Pipeline {
	p := &Pipeline{
		resolver: resolver,
		similar:  similar,
		chooser:  chooser,
		weights:  DefaultFeatureWeights(),
		config:   DefaultPipelineConfig(),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Run 执行三段流水线，永远返回非 nil Decision（除非 ctx 取消）。
//
// 出错条件严格限定：只有 ctx 取消 / resolver 报错 / chooser 报错才返回 error。
// LLM 幻觉（返回不在候选集里的 ID）不视为 error，走 PathStage2Invalid → Stage 3。
// 这是流水线设计的一部分：模型失败不该让整次派单失败，兜底永远在。
func (p *Pipeline) Run(
	ctx context.Context,
	ticket domain.Ticket,
	dir Directory,
) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	if p.config.MarginThreshold < 0 || p.config.ServiceMatchFloor <= 0 {
		return Decision{}, fmt.Errorf("pipeline config invalid: %+v", p.config)
	}
	decision := Decision{
		Outcome:     domain.OutcomeNoCandidate,
		GeneratedAt: time.Now().UTC(),
	}

	// ---- Stage 1.1 服务解析 ----
	matches, err := p.resolveServices(ctx, ticket, dir)
	if err != nil {
		return Decision{}, fmt.Errorf("resolve services: %w", err)
	}
	decision.Services = matches

	// ---- Stage 1.2 相似历史 ----
	hits, err := p.findSimilar(ctx, ticket)
	if err != nil {
		return Decision{}, fmt.Errorf("similar index: %w", err)
	}

	// ---- Stage 1.3 + 1.4 过滤 + 打分 ----
	candidates := p.scoreAll(ticket, dir, matches, hits)
	decision.Candidates = candidates

	// ---- Stage 1.5 判弱 ----
	weakness := p.judgeWeakness(candidates, matches)
	decision.Weakness = weakness

	// 无候选人：直送 Stage 3 阻塞；不走 Stage 2 也不救得了。
	if weakness == WeaknessEmptyCandidates {
		decision.Outcome = domain.OutcomeNoCandidate
		decision.Path = PathStage3Blocked
		decision.Reason = "Stage 1 无可用候选人：全员不在职或已达并发上限，直送人工阻塞"
		return decision, nil
	}

	// 强判定命中：Stage 1 直出。
	if weakness == WeaknessNone && !p.forceStage2 {
		winner := pickTop(candidates)
		decision.AssigneeID = winner.EmployeeID
		decision.Score = winner.Total
		decision.Outcome = domain.OutcomeMatched
		decision.Path = pathForWinner(winner)
		decision.Reason = explainStage1(ticket, winner, candidates, matches)
		return decision, nil
	}

	// 弱判定 → Stage 2；若关闭则直落 Stage 3。
	if !p.config.Stage2Enabled {
		return p.escalateToHuman(decision, candidates, "Stage 2 已关闭，判弱样本直落人工"), nil
	}
	if p.chooser == nil {
		return p.escalateToHuman(decision, candidates, "Stage 2 未注入 chooser，判弱样本直落人工"), nil
	}

	started := time.Now()
	req := Stage2Request{
		Ticket:       ticket,
		Matches:      matches,
		Candidates:   topCandidates(candidates, p.config.TopKCandidates),
		Weakness:     weakness,
		DirectoryRef: dir,
	}
	choice, err := p.chooser.Choose(ctx, req)
	decision.Stage2Used = true
	decision.Stage2LatencyMS = time.Since(started).Milliseconds()
	if err != nil {
		return p.escalateToHuman(decision, candidates, fmt.Sprintf("Stage 2 调用失败：%v", err)), nil
	}
	decision.Stage2Usage = choice.Usage
	decision.Stage2Rationale = choice.Rationale
	decision.Stage2Confid = choice.Confidence

	// 显式升级人工：不是错误，是模型的合法输出。
	if choice.AssigneeID == EscalateHumanID {
		decision = p.escalateToHuman(decision, candidates, "Stage 2 判定为证据不足，主动升级人工")
		decision.Stage2Rationale = choice.Rationale
		decision.Stage2Confid = choice.Confidence
		return decision, nil
	}

	// 幻觉 ID 检测：不在候选集内即视为模型失效，不重试。
	// 关闭校验（WithoutLLMEnumGuard）时不跳过检测，而是跳过保护 —— 让"失控"直接
	// 体现在 Outcome / AssigneeID 上，供评测检出；sabotage 检出的不是"错误发生"，
	// 而是"我们失去了阻止错误的能力"。
	if !p.allowAnyAssigneeID && !containsEmployee(candidates, choice.AssigneeID) {
		decision.Outcome = domain.OutcomeFallbackPool
		decision.Path = PathStage2Invalid
		decision.Reason = fmt.Sprintf(
			"Stage 2 输出了候选集外 ID %d（rationale=%q），强制转人工", choice.AssigneeID, truncate(choice.Rationale, 80))
		return decision, nil
	}

	// 采纳 Stage 2 结果。
	winner := findCandidate(candidates, choice.AssigneeID)
	if winner.EmployeeID == 0 {
		// 只有关闭 enum 校验才可能到这里：模型输出的 ID 无对应候选，
		// 但流水线按"匹配成功"处理 —— 这正是 WithoutLLMEnumGuard 要暴露的失控。
		decision.AssigneeID = choice.AssigneeID
		decision.Outcome = domain.OutcomeMatched
		decision.Path = PathStage2LLM
		decision.Reason = fmt.Sprintf(
			"Stage 2 采纳了候选集外 ID %d（enum 校验被主动关闭，无候选人明细）", choice.AssigneeID)
		return decision, nil
	}
	decision.AssigneeID = winner.EmployeeID
	decision.Score = winner.Total
	decision.Outcome = domain.OutcomeMatched
	decision.Path = pathForStage2(winner, req.Candidates)
	decision.Reason = explainStage2(ticket, winner, choice, weakness)
	return decision, nil
}

// escalateToHuman 把当前 decision 转成待认领池出口。
// 覆盖 AssigneeID = 0；Candidates 明细保留供审计。
func (p *Pipeline) escalateToHuman(d Decision, candidates []Candidate, reason string) Decision {
	d.AssigneeID = 0
	d.Outcome = domain.OutcomeFallbackPool
	d.Path = PathStage3Human
	d.Reason = reason
	d.Candidates = candidates
	return d
}

// topCandidates 取按 Total 降序的前 k 位（过滤者不参与）。
func topCandidates(candidates []Candidate, k int) []Candidate {
	active := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		if c.Active() {
			active = append(active, c)
		}
	}
	sortCandidatesStable(active)
	if len(active) <= k {
		return active
	}
	return active[:k]
}

// pickTop 返回按 tiebreak 排序后的第一位（过滤者不参与）。
// candidates 全被过滤时返回零值 —— 调用方通过 Weakness 已经先走开了这条路径。
func pickTop(candidates []Candidate) Candidate {
	active := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		if c.Active() {
			active = append(active, c)
		}
	}
	if len(active) == 0 {
		return Candidate{}
	}
	sortCandidatesStable(active)
	return active[0]
}

// sortCandidatesStable 排序规则与 Assigner.sortCandidates 一致：
// 总分降序 → 负载分降序 → 响应分降序 → 员工 ID 升序。
//
// 复制而非共享是为了避免改动 Assigner.go（并发 session 会踩到）；
// 若两侧规则漂移，pipeline 测试的 sabotage 会检出（Stage 1 排序稳定性断言）。
func sortCandidatesStable(cs []Candidate) {
	sort.SliceStable(cs, func(i, j int) bool {
		left, right := cs[i], cs[j]
		if !nearlyEqual(left.Total, right.Total) {
			return left.Total > right.Total
		}
		if !nearlyEqual(left.AvailScore, right.AvailScore) {
			return left.AvailScore > right.AvailScore
		}
		if !nearlyEqual(left.RecentScore, right.RecentScore) {
			return left.RecentScore > right.RecentScore
		}
		return left.EmployeeID < right.EmployeeID
	})
}

// pathForWinner 根据 winner 的 ownership/similar 命中情况选择 Path 常量。
// 顺序：owner > backup > similar > team > generic。
func pathForWinner(winner Candidate) Path {
	switch winner.OwnershipHit {
	case domain.OwnershipOwner:
		return PathStage1Owner
	case domain.OwnershipBackup:
		return PathStage1Backup
	}
	if winner.SimilarFrom > 0 {
		return PathStage1KNN
	}
	if winner.OwnershipHit == domain.OwnershipTeam {
		return PathStage1Team
	}
	return PathStage1Generic
}

// pathForStage2 Stage 2 内区分 top-3 与 top-4..5 的命中，作为可观测降级信号。
func pathForStage2(winner Candidate, sent []Candidate) Path {
	for i, c := range sent {
		if c.EmployeeID != winner.EmployeeID {
			continue
		}
		if i < 3 {
			return PathStage2LLM
		}
		return PathStage2Below
	}
	return PathStage2LLM
}

func containsEmployee(candidates []Candidate, employeeID int64) bool {
	for _, c := range candidates {
		if c.EmployeeID == employeeID {
			return true
		}
	}
	return false
}

func findCandidate(candidates []Candidate, employeeID int64) Candidate {
	for _, c := range candidates {
		if c.EmployeeID == employeeID {
			return c
		}
	}
	return Candidate{}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// nearlyEqual 定义在 assigner.go；同包内共享，pipeline 侧的排序与 margin 判定复用同一实现。

// explainStage1 Stage 1 直出时的可读理由。
func explainStage1(ticket domain.Ticket, winner Candidate, all []Candidate, matches []ServiceMatch) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Stage 1 直出：%s（own %.2f / sim %.2f / avail %.2f / senior %.2f / recent %.2f，总 %.3f）",
		winner.Name, winner.OwnScore, winner.SimScore, winner.AvailScore, winner.SeniorityScore, winner.RecentScore, winner.Total)
	if len(matches) > 0 && matches[0].Score > 0 {
		fmt.Fprintf(&b, "；服务归属 top1=%d（score %.2f）", matches[0].ServiceID, matches[0].Score)
	}
	if runner := runnerUp(winner, all); runner != nil {
		fmt.Fprintf(&b, "；与次位 %s 差 %.3f", runner.Name, winner.Total-runner.Total)
	}
	return b.String()
}

// explainStage2 Stage 2 采纳后的可读理由，把 weakness 与 rationale 一起写进去。
func explainStage2(ticket domain.Ticket, winner Candidate, choice LLMChoice, weakness WeaknessReason) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Stage 2 采纳（因 %s 升级）：%s，总分 %.3f",
		weakness, winner.Name, winner.Total)
	if choice.Rationale != "" {
		fmt.Fprintf(&b, "；LLM 理由：%s", truncate(choice.Rationale, 120))
	}
	if choice.Confidence > 0 {
		fmt.Fprintf(&b, "；LLM 自报置信度 %.2f", choice.Confidence)
	}
	return b.String()
}

// runnerUp 返回除 winner 外分数最高的 active candidate。
func runnerUp(winner Candidate, all []Candidate) *Candidate {
	var best *Candidate
	for i := range all {
		c := all[i]
		if !c.Active() || c.EmployeeID == winner.EmployeeID {
			continue
		}
		if best == nil || c.Total > best.Total {
			local := c
			best = &local
		}
	}
	return best
}
