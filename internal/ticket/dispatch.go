// Dispatcher 抽象：把"给工单找人"这件事从具体实现里解出来。
//
// 存在两个实现：
//   - legacyAssigner：包住一期上线时的技能派单器（`assign.Assigner`），
//     只用 Directory.Employees；不感知服务字典、员工扩展、Stage 2。
//   - pipelineDispatcher：包住三段流水线（`assign.Pipeline`）。
//
// 为什么要这层抽象：`ticket.Service` 已经用得非常广（api / agent / demo / eval / cmd），
// 直接把 `assigner` 字段换成 `*Pipeline` 会连锁改动十几处。加一层薄壳，
// 让 `New(st, *Assigner)` 的公开签名保持不变，pipeline 走 `NewWithDispatcher` 显式接入。
//
// 见 docs/DISPATCH_PIPELINE.md §5。
package ticket

import (
	"context"

	"github.com/mac/helpdesk-agent/internal/assign"
	"github.com/mac/helpdesk-agent/internal/domain"
)

// Dispatcher 一次派单的抽象。
//
// ctx 参数是给 pipeline 侧的 Stage 2 用的（模型调用要能取消）。
// legacyAssigner 忽略 ctx 也无所谓，因为它是纯 CPU 打分。
type Dispatcher interface {
	Dispatch(ctx context.Context, ticket domain.Ticket, dir assign.Directory) (domain.AssignmentLog, error)
}

// DirectoryProvider 决定 Service 每次派单时看到的员工扩展与服务字典。
//
// 做成函数而不是接口：一期数据在 seed 里硬编码，closure 直接闭一段查表；
// 未来换成 store-backed 时也是 closure 换内部实现，Service 侧无感。
type DirectoryProvider interface {
	Provide(employees []domain.Employee) assign.Directory
}

// DirectoryProviderFunc 让普通函数满足 DirectoryProvider。
type DirectoryProviderFunc func(employees []domain.Employee) assign.Directory

// Provide 实现 DirectoryProvider。
func (f DirectoryProviderFunc) Provide(employees []domain.Employee) assign.Directory {
	return f(employees)
}

// emptyDirectoryProvider 是默认 provider：只提供 Employees。
// pipeline 会因 Services 为空判 WeaknessNoServiceMatch，若无 Stage 2 chooser 就落 Stage 3；
// legacyAssigner 完全不受影响（它不读 Services / Extensions 两个字段）。
func emptyDirectoryProvider() DirectoryProvider {
	return DirectoryProviderFunc(func(emps []domain.Employee) assign.Directory {
		return assign.Directory{Employees: emps}
	})
}

// ---------- 实现 1：legacyAssigner ----------

// legacyAssigner 让现有 *assign.Assigner 满足 Dispatcher。
//
// 忽略 dir.Extensions / dir.Services 是刻意的：一期 Assigner 只依赖 SkillSet，
// 若它开始读扩展字段，就与 pipeline 变成了"两个模型走同一份数据"，
// 破坏"pipeline 独立、Assigner 只作为旧数据集回归目标"的分层。
type legacyAssigner struct {
	assigner *assign.Assigner
}

// NewLegacyDispatcher 用旧派单器构造一个 Dispatcher。
func NewLegacyDispatcher(a *assign.Assigner) Dispatcher {
	if a == nil {
		a = assign.New(assign.DefaultWeights())
	}
	return &legacyAssigner{assigner: a}
}

// Dispatch 实现 Dispatcher。
func (l *legacyAssigner) Dispatch(_ context.Context, ticket domain.Ticket, dir assign.Directory) (domain.AssignmentLog, error) {
	log := l.assigner.Assign(ticket, dir.Employees)
	log.TicketID = ticket.ID
	return log, nil
}

// ---------- 实现 2：pipelineDispatcher ----------

type pipelineDispatcher struct {
	pipeline *assign.Pipeline
}

// NewPipelineDispatcher 用三段流水线构造 Dispatcher。
//
// pipeline 为 nil 时 panic —— 显式接入 pipeline 的场景下静默退回旧路径
// 会让"以为在跑 pipeline，其实没跑"这种问题极难诊断。
func NewPipelineDispatcher(p *assign.Pipeline) Dispatcher {
	if p == nil {
		panic("NewPipelineDispatcher: pipeline 不能为 nil；如需旧路径请显式用 NewLegacyDispatcher")
	}
	return &pipelineDispatcher{pipeline: p}
}

// Dispatch 实现 Dispatcher。
//
// pipeline.Run 内部返回 Decision；这里折算为 AssignmentLog 让存储层无感。
// Pipeline 内部观测（Path / Weakness / Stage2 usage）在 log 结构里放不下，
// 一期只写进 Reason 字符串前缀（"[path=stage1_owner weakness=none]"）
// 便于在派单历史里 grep 出流水线行为，正式观测请另建 pipeline_run 表。
func (p *pipelineDispatcher) Dispatch(ctx context.Context, ticket domain.Ticket, dir assign.Directory) (domain.AssignmentLog, error) {
	decision, err := p.pipeline.Run(ctx, ticket, dir)
	if err != nil {
		return domain.AssignmentLog{}, err
	}
	log := decision.ToAssignmentLog()
	log.TicketID = ticket.ID
	log.Reason = "[" + string(decision.Path) + "|" + decision.Weakness.String() + "] " + log.Reason
	return log, nil
}
