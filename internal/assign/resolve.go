// Stage 1.1：工单 → 服务节点解析。
//
// 复用 rag.BM25Embedder 而不引入新的检索栈：中文 bigram + 英文分词 +
// 归一化打分这套规则已在知识库轴上验证过，服务字典同形态复用成本最低。
// 触发条件升级到 hybrid + cross-encoder 见 docs/DISPATCH_PIPELINE.md §6。
package assign

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mac/helpdesk-agent/internal/domain"
	"github.com/mac/helpdesk-agent/internal/rag"
)

// ServiceResolver 是 Stage 1.1 的抽象。
//
// 独立成接口的原因与 llm.ChatModel 同源：让 pipeline 单测不需要真实 BM25 索引，
// 也让真实部署时（如接内部 CMDB API）能替换实现而不改流水线。
type ServiceResolver interface {
	Resolve(ctx context.Context, ticket domain.Ticket, dir Directory) ([]ServiceMatch, error)
}

// BM25ServiceResolver 用 BM25 在服务字典上做排序召回。
//
// 无状态：每次 Resolve 现场从 Directory.Services 建索引。
// 服务字典规模 < 30 条时这没有性能问题，字典若长期不变可改为构造期预建索引 ——
// 但那会把 Directory 从"参数"降级为"绑定"，评测 fixture 反而更麻烦。一期保持现场建。
type BM25ServiceResolver struct {
	TopK int
	// scorer 用 BM25 归一化分数（0..1）。见 §Score 的警告：
	// 归一化分数的绝对值对短查询会虚高，但服务字典里字段拼接较长，实测影响可控；
	// Stage 1 只用它做排序与弱判定阈值，不用于精确置信度。
	scorer *rag.BM25Embedder
}

// NewBM25ServiceResolver 构造解析器。TopK <= 0 时回退为 3。
func NewBM25ServiceResolver(topK int) *BM25ServiceResolver {
	if topK <= 0 {
		topK = 3
	}
	return &BM25ServiceResolver{TopK: topK}
}

// Resolve 把 ticket 的 Title + Description 拼接作为查询。
//
// 只用这两个字段：Category 与 Priority 是"业务分类"，与"哪个服务出问题"正交，
// 加入查询会引入噪声（"incident"这个词每个服务的文档都可能提到）。
func (r *BM25ServiceResolver) Resolve(
	ctx context.Context,
	ticket domain.Ticket,
	dir Directory,
) ([]ServiceMatch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(dir.Services) == 0 {
		return nil, nil // 无字典不是错误；Stage 1 会因 serviceFloor 未达判弱
	}
	query := strings.TrimSpace(ticket.Title + " " + ticket.Description)
	if query == "" {
		return nil, nil
	}

	// 排序 services 保证 document[i] 与 serviceID[i] 一一对应且跨调用稳定。
	// map 迭代顺序随机是 Go 的常识，忽略它就会把 pipeline 的确定性排序毁于一旦。
	ids := make([]int64, 0, len(dir.Services))
	for id := range dir.Services {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	docs := make([]string, 0, len(ids))
	for _, id := range ids {
		docs = append(docs, dir.Services[id].SearchableText())
	}

	// scorer 每次现建；BM25Embedder 构造 = 分词 + 建倒排索引，
	// 字典 <30 条时开销 <1ms。
	scorer := rag.NewBM25Embedder(docs)
	ranked := scorer.RankBM25(query, docs)
	limit := r.TopK
	if limit > len(ranked) {
		limit = len(ranked)
	}
	matches := make([]ServiceMatch, 0, limit)
	for i := 0; i < limit; i++ {
		item := ranked[i]
		matches = append(matches, ServiceMatch{
			ServiceID: ids[item.Index],
			Score:     item.Score,
			Reason:    "bm25",
		})
	}
	return matches, nil
}

// StaticServiceResolver 是测试用的桩实现：按 ticket.ExternalRef 或关键词表返回固定服务。
//
// 名字里"Static"是刻意的：它不是"假的"，是"输入决定输出的查表版"。
// 用于 pipeline 单测里把 resolve 段与 score 段解耦，让 score 单测能精准指定
// "服务 1 的 owner 是 101"这种 fixture 关系。
type StaticServiceResolver struct {
	// ByTicket 按 ticket.ID 返回服务命中；命中缺失时回退到 Default。
	ByTicket map[int64][]ServiceMatch
	// Default 是无匹配时的兜底输出。
	Default []ServiceMatch
	// Err 若不为 nil，Resolve 直接返回该错误，用于测试错误传播。
	Err error
}

// Resolve 实现 ServiceResolver。
func (r StaticServiceResolver) Resolve(_ context.Context, ticket domain.Ticket, _ Directory) ([]ServiceMatch, error) {
	if r.Err != nil {
		return nil, r.Err
	}
	if matches, ok := r.ByTicket[ticket.ID]; ok {
		return append([]ServiceMatch(nil), matches...), nil
	}
	return append([]ServiceMatch(nil), r.Default...), nil
}

// trustedMatches 保留分数达到下限的服务命中，用于 ownership 归属。
//
// 为什么必须过滤：resolve 返回的 top-K 里可能混入误命中。实测一条接口工单，
// 正确服务 score≈0.34、紧随的网络服务 score≈0.22。若把两者都送进 ownershipOf，
// 误命中服务的负责人会拿到与正确负责人同样的 owner 分（各 1.0），
// 把"最懂的人"和"最像的人"拉平 —— 判弱段接着因 margin 不足把整单踢去 Stage 2/3。
// 归属只认可信命中，弱命中即便进了 top-K 也不给 ownership 信用。
func trustedMatches(matches []ServiceMatch, floor float64) []ServiceMatch {
	out := make([]ServiceMatch, 0, len(matches))
	for _, m := range matches {
		if m.Score >= floor {
			out = append(out, m)
		}
	}
	return out
}

// ownershipOf 计算一位员工对一组服务命中的最高 ownership 档位。
//
// 遍历顺序必须确定：matches 上游已按 score 降序，直接线性扫描即可。
// 不 map 迭代是为了让"两个服务同分"时始终选第一个 —— 排序稳定性依赖这点。
func ownershipOf(ext domain.EmployeeExtension, matches []ServiceMatch, dir Directory) domain.OwnershipKind {
	best := domain.OwnershipNone
	for _, m := range matches {
		kind := ext.DirectOwnership(m.ServiceID)
		if kind == domain.OwnershipNone {
			if svc, ok := dir.ServiceOf(m.ServiceID); ok && ext.InTeam(svc) {
				kind = domain.OwnershipTeam
			}
		}
		if kind > best {
			best = kind
		}
	}
	return best
}

// resolveServices 是 pipeline.Run 的 Stage 1.1 入口。
// nil resolver 视为"无服务字典"，返回空命中让 weakness 段判 NoServiceMatch。
func (p *Pipeline) resolveServices(ctx context.Context, ticket domain.Ticket, dir Directory) ([]ServiceMatch, error) {
	if p.resolver == nil {
		return nil, nil
	}
	return p.resolver.Resolve(ctx, ticket, dir)
}

// DescribeResolver 返回 resolver 的人类可读说明，用于日志与评测报告的元信息。
func DescribeResolver(r ServiceResolver) string {
	switch r.(type) {
	case *BM25ServiceResolver:
		return "bm25_service_resolver"
	case StaticServiceResolver:
		return "static_service_resolver"
	case nil:
		return "none"
	default:
		return fmt.Sprintf("%T", r)
	}
}
