// Stage 1.4：五特征加权打分。
//
// 特征与权重见 docs/DISPATCH_PIPELINE.md §1.4。核心决策：
// ownership 一家独大（0.40），其余四项是次级调节。
// Skill 权重不在这里 —— 一期设 0；现有 Jaccard 归旧 Assigner 承担"排序段回归"。
package assign

import (
	"github.com/mac/helpdesk-agent/internal/domain"
)

// scoreAll 对全部员工执行"过滤 + 打分"，产出稳定排序的 Candidate 列表。
//
// 输出顺序按 Candidates 数组内位置 = 输入 dir.Employees 顺序，不做重排。
// 原因：Decision.ToAssignmentLog 会保留全部明细（含被过滤者），
// 明细数组的顺序应当与输入一致，这样 diff 友好、审计友好。
// 打分之后需要"按分排序"的地方由 pickTop / topCandidates 各自做局部排序。
func (p *Pipeline) scoreAll(
	ticket domain.Ticket,
	dir Directory,
	matches []ServiceMatch,
	hits []SimilarHit,
) []Candidate {
	simByEmployee := buildSimilarIndex(hits)
	// 只有达到下限的服务命中才可用于 ownership 归属。
	// 见 trustedMatches：不先过滤的话，误命中服务的负责人会和正确服务的负责人并列满分。
	trusted := trustedMatches(matches, p.config.ServiceMatchFloor)
	out := make([]Candidate, 0, len(dir.Employees))
	for _, emp := range dir.Employees {
		ext, _ := dir.ExtensionOf(emp.ID)
		candidate := Candidate{EmployeeID: emp.ID, Name: emp.Name}

		if filtered, reason := eligibilityCheck(ticket, emp, ext); filtered {
			candidate.Filtered = true
			candidate.FilterReason = reason
			out = append(out, candidate)
			continue
		}

		kind := ownershipOf(ext, trusted, dir)
		candidate.OwnershipHit = kind
		candidate.OwnScore = ownershipScore(kind)
		if p.skipOwnershipBoost {
			candidate.OwnScore = 0
		}

		if top, ok := simByEmployee[emp.ID]; ok {
			candidate.SimScore = clamp01(top.Score)
			candidate.SimilarFrom = top.TicketID
		}

		candidate.AvailScore = 1 - emp.LoadRatio()
		candidate.SeniorityScore = seniorityScore(ext.Level)
		candidate.RecentScore = clamp01(emp.Recency)

		candidate.Total = p.weights.Ownership*candidate.OwnScore +
			p.weights.Similar*candidate.SimScore +
			p.weights.Avail*candidate.AvailScore +
			p.weights.Seniority*candidate.SeniorityScore +
			p.weights.Recency*candidate.RecentScore
		out = append(out, candidate)
	}
	return out
}

// ownershipScore 把 ownership 档位映射到 0..1 分数。
//
// 档位刻意做成 1.0 / 0.3 / 0.1 的陡降而非等差：owner / backup / team 在真实业务里
// 是"必须找这个人 / 他可以顶 / 只是熟"，owner 与其余两档之间应当有决定性落差。
//
// 为什么从 1.0/0.6/0.3 改成 1.0/0.3/0.1：v2 评测暴露出结构性误判——
// owner 是 mid、backup 是 senior 时，ownership 差 0.4×0.6=0.24 抵不过
// seniority 反号带来的 0.1×(1.0-0.5)=0.05 与 recency 差，top1/top2 margin
// 落到 ≈0.125 < MarginThreshold(0.15)，本应直派的强命中被判 low_margin 升级。
// 把 backup/team 压低后，owner 分差重新压得住其余四项的噪声。
func ownershipScore(kind domain.OwnershipKind) float64 {
	switch kind {
	case domain.OwnershipOwner:
		return 1.0
	case domain.OwnershipBackup:
		return 0.3
	case domain.OwnershipTeam:
		return 0.1
	default:
		return 0
	}
}

// seniorityScore 把级别映射到 0..1 分数。
//
// 一期规则：junior=0 / mid=0.5 / senior=1.0 / unknown=0。
// unknown 与 junior 同分是刻意的 —— 未标注级别不该被当作"资深"来加权。
// P0 的 senior 硬约束在 eligibility 段，不重复计入这里。
func seniorityScore(level domain.Level) float64 {
	switch level {
	case domain.LevelSenior:
		return 1.0
	case domain.LevelMid:
		return 0.5
	default:
		return 0
	}
}

// buildSimilarIndex 按 resolver 归并相似命中，只保留每人最高分。
// ResolverID=0 的记录忽略 —— 那是"有相似工单但没派单记录"，不参与打分。
func buildSimilarIndex(hits []SimilarHit) map[int64]SimilarHit {
	ret := make(map[int64]SimilarHit, len(hits))
	for _, h := range hits {
		if h.ResolverID == 0 {
			continue
		}
		prev, ok := ret[h.ResolverID]
		if !ok || h.Score > prev.Score {
			ret[h.ResolverID] = h
		}
	}
	return ret
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
