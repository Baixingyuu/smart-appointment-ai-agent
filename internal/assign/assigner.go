// Package assign 实现确定性派单器。
//
// 设计要点：
//  1. 纯函数，无 I/O、无时间依赖、无随机 —— 同一输入必然得到同一输出，
//     因此可以直接单元测试，也进得了评测。
//  2. 候选过滤先于评分 —— 达到并发上限的员工不参与竞争，
//     否则「技能最匹配但已满载」的人会抢走工单。
//  3. 每一层结果都落进 AssignmentLog —— 让「为什么选了他」可被逐步复核，
//     而不是只给一个总分。
//  4. 技能匹配使用 Jaccard，完全确定性，不使用 LLM ——
//     派单结果必须可复现、可解释，且「指派准确率」这个指标才有意义。
package assign

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mac/helpdesk-agent/internal/domain"
)

// Weights 三项评分的权重。
type Weights struct {
	Skill   float64
	Load    float64
	Recency float64
}

// DefaultWeights 返回一期默认权重。
//
// 技能占主导（0.6）：本期目标是「派给有相关处理经验的人」，
// 而负载与响应只是次级调节项。
//
// 改为不等权的原因是一次实测发现：等权下负载分差可达 1.0，
// 而技能分差受 Jaccard 封顶同为 1.0，两者量纲相同却让
// 「最闲的人」频繁盖过「最懂的人」；更严重的是，实测证明
// 把技能权重置零后评测仍全部通过 —— 即等权下技能项对结果
// 几乎没有区分力，评测也就无法证明技能匹配真的在工作。
// 提高技能权重后，新增的对抗样本（见 eval/datasets/assignment_hard.json）
// 能稳定检出「忽略技能」这一回归。
func DefaultWeights() Weights {
	return Weights{Skill: 0.60, Load: 0.20, Recency: 0.20}
}

// Assigner 派单器。无状态，可并发使用。
type Assigner struct {
	weights Weights

	// 以下开关仅供评测回归验证使用，生产路径全部取默认值。
	// 它们让「关键不变量被破坏时评测能否检出」可被直接证明，
	// 而不是靠人工构造一个脆弱的实现副本。
	skipCapacityCheck bool
	invertSelection   bool
}

// Option 调整派单器行为。
type Option func(*Assigner)

// WithCapacityCheckDisabled 关闭并发上限过滤。
//
// 仅用于评测自检：关闭后满载员工会参与竞争，评测必须能检出该回归。
// 生产代码不得使用。
func WithCapacityCheckDisabled() Option {
	return func(a *Assigner) { a.skipCapacityCheck = true }
}

// WithInvertedSelection 反转选择顺序，取总分最低的可用候选。
//
// 仅用于评测自检，验证排序方向被写反时可被检出。生产代码不得使用。
func WithInvertedSelection() Option {
	return func(a *Assigner) { a.invertSelection = true }
}

// WithSkillWeightZeroed 将技能权重置零，只按负载与响应评分。
//
// 仅用于评测自检，验证技能匹配失效时可被检出。生产代码不得使用。
func WithSkillWeightZeroed() Option {
	return func(a *Assigner) { a.weights.Skill = 0 }
}

// New 构造派单器。权重非法时回退到默认等权。
func New(weights Weights, opts ...Option) *Assigner {
	if weights.sum() <= 0 {
		weights = DefaultWeights()
	}
	assigner := &Assigner{weights: weights}
	for _, opt := range opts {
		opt(assigner)
	}
	return assigner
}

// NewWithCapacityFilterDisabled 构造关闭并发过滤的派单器（评测自检用）。
func NewWithCapacityFilterDisabled() *Assigner {
	return New(DefaultWeights(), WithCapacityCheckDisabled())
}

// NewWithInvertedSelection 构造反转选择顺序的派单器（评测自检用）。
func NewWithInvertedSelection() *Assigner {
	return New(DefaultWeights(), WithInvertedSelection())
}

// NewWithoutSkillWeight 构造忽略技能权重的派单器（评测自检用）。
func NewWithoutSkillWeight() *Assigner {
	return New(DefaultWeights(), WithSkillWeightZeroed())
}

func (w Weights) sum() float64 { return w.Skill + w.Load + w.Recency }

// Assign 为工单选择处理人。
//
// 返回的 AssignmentLog 永远非 nil，且 Candidates 覆盖全部输入员工
// （含被过滤者及其过滤原因），便于审计。
func (a *Assigner) Assign(ticket domain.Ticket, employees []domain.Employee) domain.AssignmentLog {
	log := domain.AssignmentLog{
		TicketID:   ticket.ID,
		Outcome:    domain.OutcomeNoCandidate,
		Candidates: make([]domain.CandidateScore, 0, len(employees)),
	}

	scored := make([]domain.CandidateScore, 0, len(employees))
	for _, emp := range employees {
		emp := emp
		if err := emp.Validate(); err != nil {
			log.Candidates = append(log.Candidates, domain.CandidateScore{
				EmployeeID: emp.ID, Name: emp.Name,
				Filtered: true, FilterReason: "invalid: " + err.Error(),
			})
			continue
		}
		candidate := a.score(ticket, emp)
		log.Candidates = append(log.Candidates, candidate)
		if candidate.Active() {
			scored = append(scored, candidate)
		}
	}

	if len(scored) == 0 {
		log.Outcome = domain.OutcomeNoCandidate
		log.Reason = "没有可用处理人：全部离线或已达并发上限"
		return log
	}

	// 先判断是否存在技能命中。全员零技能命中时退回待认领池，
	// 而不是把工单发给「最不忙的无关员工」——后者会制造错误分派。
	hasSkillHit := false
	for _, c := range scored {
		if c.SkillScore > 0 {
			hasSkillHit = true
			break
		}
	}

	winner := bestCandidate(scored, a.invertSelection)
	if !hasSkillHit {
		log.Outcome = domain.OutcomeFallbackPool
		log.AssigneeID = winner.EmployeeID
		log.Score = winner.Total
		log.Reason = fmt.Sprintf(
			"无技能命中：%d 名可用处理人均与需求技能无交集，退回待认领池（建议 %s）",
			len(scored), winner.Name)
		return log
	}

	log.Outcome = domain.OutcomeMatched
	log.AssigneeID = winner.EmployeeID
	log.Score = winner.Total
	log.Reason = explainWinner(ticket, winner, scored, skillsByEmployee(employees))
	return log
}

// score 计算单个员工的三项得分与总分。
func (a *Assigner) score(ticket domain.Ticket, emp domain.Employee) domain.CandidateScore {
	candidate := domain.CandidateScore{EmployeeID: emp.ID, Name: emp.Name}

	if !emp.Active {
		candidate.Filtered = true
		candidate.FilterReason = "不在职"
		return candidate
	}
	if !emp.HasCapacity() && !a.skipCapacityCheck {
		candidate.Filtered = true
		candidate.FilterReason = fmt.Sprintf("已达并发上限 (%d/%d)", emp.CurrentLoad, emp.MaxConcurrent)
		return candidate
	}

	// 技能匹配度：工单需求与员工技能的 Jaccard 相似度。
	if !ticket.RequiredSkill.IsEmpty() {
		candidate.SkillScore = ticket.RequiredSkill.Jaccard(emp.Skills)
	}
	// 负载得分：越空闲越高。用 1-loadRatio 而非直接负载，
	// 使其与其余两项同为「越大越好」，权重才可比较。
	candidate.LoadScore = 1 - emp.LoadRatio()
	candidate.RecencyScore = emp.Recency
	candidate.Total = a.weights.Skill*candidate.SkillScore +
		a.weights.Load*candidate.LoadScore +
		a.weights.Recency*candidate.RecencyScore
	return candidate
}

// bestCandidate 按总分降序选择最优候选人。
//
// 平局时依次比较负载得分、最近响应、最后比较员工 ID 升序，
// 保证排序是全序，从而整体结果确定可复现。
// invert 为 true 时取总分最低者，仅用于评测自检。
func bestCandidate(candidates []domain.CandidateScore, invert bool) domain.CandidateScore {
	sorted := append([]domain.CandidateScore(nil), candidates...)
	sortCandidates(sorted)
	if invert {
		return sorted[len(sorted)-1]
	}
	return sorted[0]
}

// sortCandidates 对候选人做确定性排序，最优在前。
func sortCandidates(candidates []domain.CandidateScore) {
	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if left.Total != right.Total {
			return left.Total > right.Total
		}
		if left.LoadScore != right.LoadScore {
			return left.LoadScore > right.LoadScore
		}
		if left.RecencyScore != right.RecencyScore {
			return left.RecencyScore > right.RecencyScore
		}
		return left.EmployeeID < right.EmployeeID
	})
}

// explainWinner 生成可读的指派理由，含各项分数与是否因技能胜出。
//
// skillsByEmployee 提供候选人技能集合，用于计算需求覆盖率。
// 技能不放进 CandidateScore：那是评分明细的载体，不应兼作数据存储。
func explainWinner(ticket domain.Ticket, winner domain.CandidateScore, scored []domain.CandidateScore, skillsByEmployee map[int64]domain.SkillSet) string {
	var b strings.Builder
	fmt.Fprintf(&b, "指派 %s（技能 %.2f / 负载 %.2f / 响应 %.2f，总分 %.3f）",
		winner.Name, winner.SkillScore, winner.LoadScore, winner.RecencyScore, winner.Total)

	if cov := ticket.RequiredSkill.CoveredBy(skillsByEmployee[winner.EmployeeID]); cov > 0 {
		fmt.Fprintf(&b, "；需求技能覆盖 %.0f%%", cov*100)
	}

	runnerUp := runnerUpOf(winner, scored)
	if runnerUp == nil {
		fmt.Fprintf(&b, "；无其他可用候选人")
		return b.String()
	}
	// 说明胜出原因，便于业务方复核是否存在「因负载而非技能胜出」的情况。
	//
	// 分项比较必须容忍浮点误差：1/3 这类权重在二进制下无法精确表示，
	// 直接比较会让「技能实际相同」被误报为「技能更优」，理由因此失真。
	switch {
	case !nearlyEqual(winner.SkillScore, runnerUp.SkillScore):
		fmt.Fprintf(&b, "；技能匹配优于 %s（%.2f vs %.2f）", runnerUp.Name, winner.SkillScore, runnerUp.SkillScore)
	case !nearlyEqual(winner.LoadScore, runnerUp.LoadScore):
		fmt.Fprintf(&b, "；技能相当（%.2f），因负载更低胜出 %s（%.2f vs %.2f）",
			winner.SkillScore, runnerUp.Name, winner.LoadScore, runnerUp.LoadScore)
	case !nearlyEqual(winner.RecencyScore, runnerUp.RecencyScore):
		fmt.Fprintf(&b, "；技能与负载相当，因最近响应更优胜出 %s（%.2f vs %.2f）",
			runnerUp.Name, winner.RecencyScore, runnerUp.RecencyScore)
	default:
		fmt.Fprintf(&b, "；技能、负载与响应均相当，按员工 ID 升序收敛于 %d（对比 %s id=%d）",
			winner.EmployeeID, runnerUp.Name, runnerUp.EmployeeID)
	}
	return b.String()
}

// nearlyEqual 判断两个分数是否可视为相同，容忍浮点累加误差。
func nearlyEqual(a, b float64) bool {
	const epsilon = 1e-9
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff < epsilon
}

// runnerUpOf 返回除 winner 外总分最高的候选人。
func runnerUpOf(winner domain.CandidateScore, scored []domain.CandidateScore) *domain.CandidateScore {
	var best *domain.CandidateScore
	for i := range scored {
		candidate := scored[i]
		if candidate.EmployeeID == winner.EmployeeID {
			continue
		}
		if best == nil || candidate.Total > best.Total {
			best = &candidate
		}
	}
	return best
}

// skillsByEmployee 建立员工 ID 到技能集合的索引，供生成指派理由时查覆盖率。
func skillsByEmployee(employees []domain.Employee) map[int64]domain.SkillSet {
	ret := make(map[int64]domain.SkillSet, len(employees))
	for _, emp := range employees {
		ret[emp.ID] = emp.Skills
	}
	return ret
}
