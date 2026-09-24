// Stage 1.2：相似历史工单 kNN。
//
// 一期接口先立起来，实现走"空返回"。原因见 docs/DISPATCH_PIPELINE.md §1.2：
// 用假造的"历史工单"跑评测会让 s_sim 段过拟合到造出来的分布，
// 反而把 pipeline 端到端指标变得没意义。等真实工单沉淀出来再灌索引。
package assign

import (
	"context"
	"sort"

	"github.com/mac/helpdesk-agent/internal/domain"
)

// SimilarTicketIndex 提供"给一张工单，找处理过类似工单的人"能力。
//
// 返回值按 Score 降序。ResolverID 允许为 0 表示"这条命中没有指派记录"，
// 打分器会忽略该条而不是把它当成"派给了不存在的员工"。
type SimilarTicketIndex interface {
	FindSimilar(ctx context.Context, ticket domain.Ticket, k int) ([]SimilarHit, error)
}

// NoopSimilarIndex 一期默认实现：恒返回空。
//
// 命名显式化而不是 nil 判空，是为了让 pipeline 单测在需要"有相似命中"这条路径时
// 换成 StaticSimilarTicketIndex，而不用换整个 wiring。
type NoopSimilarIndex struct{}

// FindSimilar 返回 nil, nil。
func (NoopSimilarIndex) FindSimilar(_ context.Context, _ domain.Ticket, _ int) ([]SimilarHit, error) {
	return nil, nil
}

// StaticSimilarIndex 测试用桩：按 ticketID 返回固定命中集合。
//
// 与 StaticServiceResolver 一对：让 pipeline 单测能把 Stage 1 五段中任意一段
// 变成"查表版"，从而精确定位是哪一段退化。
type StaticSimilarIndex struct {
	ByTicket map[int64][]SimilarHit
	Err      error
}

// FindSimilar 返回预置数据（拷贝，避免调用方修改原数据）。
func (s StaticSimilarIndex) FindSimilar(_ context.Context, ticket domain.Ticket, k int) ([]SimilarHit, error) {
	if s.Err != nil {
		return nil, s.Err
	}
	hits, ok := s.ByTicket[ticket.ID]
	if !ok {
		return nil, nil
	}
	sorted := append([]SimilarHit(nil), hits...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Score != sorted[j].Score {
			return sorted[i].Score > sorted[j].Score
		}
		return sorted[i].TicketID < sorted[j].TicketID
	})
	if k > 0 && len(sorted) > k {
		sorted = sorted[:k]
	}
	return sorted, nil
}

// findSimilar 是 pipeline.Run 的 Stage 1.2 入口。
// similar 为 nil 时视为 NoopSimilarIndex。
func (p *Pipeline) findSimilar(ctx context.Context, ticket domain.Ticket) ([]SimilarHit, error) {
	if p.similar == nil {
		return nil, nil
	}
	return p.similar.FindSimilar(ctx, ticket, p.config.TopKCandidates)
}
