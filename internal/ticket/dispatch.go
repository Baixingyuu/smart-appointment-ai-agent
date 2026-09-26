// 派单装配：Service 每次派单看到的「候选人 + 服务字典 + 员工扩展」从哪来。
//
// 派单实现只有一个：三段流水线（`assign.Pipeline`）。曾与它并存的
// 「工单技能集合 × 员工技能集合做 Jaccard」的旧派单器已整体删除，
// 因此这里也不再需要 Dispatcher 那层多实现抽象——它当初只是为了
// 让迁移期十几处 `ticket.New(...)` 调用点不改签名。
//
// 见 docs/DISPATCH_PIPELINE.md §5。
package ticket

import (
	"context"

	"github.com/mac/helpdesk-agent/internal/assign"
	"github.com/mac/helpdesk-agent/internal/domain"
)

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
//
// 缺服务字典时 pipeline 会判 WeaknessNoServiceMatch，若无 Stage 2 chooser 就落 Stage 3。
// 这是"没接数据"的正确反应，不是 bug——所以默认值刻意不猜数据。
func emptyDirectoryProvider() DirectoryProvider {
	return DirectoryProviderFunc(func(emps []domain.Employee) assign.Directory {
		return assign.Directory{Employees: emps}
	})
}

// runAssignment 执行一次派单并折算成可落库的 AssignmentLog。
//
// Pipeline 的内部观测（Path / Weakness / Stage 2 用量）在 AssignmentLog 结构里放不下，
// 一期把 path 与 weakness 拼进 Reason 前缀（"[path=stage1_owner|weakness=none] …"），
// 便于在派单历史里 grep 出流水线行为；正式观测请另建 pipeline_run 表。
func runAssignment(ctx context.Context, pipeline *assign.Pipeline, t domain.Ticket, dir assign.Directory) (domain.AssignmentLog, error) {
	decision, err := pipeline.Run(ctx, t, dir)
	if err != nil {
		return domain.AssignmentLog{}, err
	}
	log := decision.ToAssignmentLog()
	log.TicketID = t.ID
	log.Reason = "[" + string(decision.Path) + "|" + decision.Weakness.String() + "] " + log.Reason
	// 派单完成事件（若上下文里注入了观察者，如 SSE 外显）。
	assign.EmitDispatchDone(ctx, decision)
	return log, nil
}
