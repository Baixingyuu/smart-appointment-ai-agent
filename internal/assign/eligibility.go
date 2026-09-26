// Stage 1.3：硬约束过滤（在职 / 并发 / P0 级别门槛）。
//
// 过滤先于打分：见 docs/DISPATCH_PIPELINE.md §1.3。
// 关键不变量：被过滤者也要进 Candidates 明细，否则"为什么没选他"没法复核。
package assign

import (
	"fmt"

	"github.com/mac/helpdesk-agent/internal/domain"
)

// eligibilityCheck 返回 (是否过滤, 过滤原因)。
// 未过滤时 reason 为空字符串。
//
// 一期不启用 OnCall 硬约束：真实值班表要接 HR / IM 才可信，
// 加了假数据只会让评测学到"on-call 是硬约束"这条不存在的规则。
// 触发条件见 docs/DISPATCH_PIPELINE.md §6。
func eligibilityCheck(ticket domain.Ticket, emp domain.Employee, ext domain.EmployeeExtension) (bool, string) {
	if !emp.Active {
		return true, "不在职"
	}
	if !emp.HasCapacity() {
		return true, fmt.Sprintf("已达并发上限 (%d/%d)", emp.CurrentLoad, emp.MaxConcurrent)
	}
	// P0 派单要求 senior。这条规则是分诊语义：P0 出问题时宁可让 senior 半夜上来，
	// 不能让 junior 接了再升级；两次派错的代价大于一次等 senior。
	if ticket.Priority == domain.PriorityP0 {
		if !ext.Level.AtLeast(domain.LevelSenior) {
			return true, fmt.Sprintf("P0 要求 senior 及以上，当前级别 %q", ext.Level)
		}
	}
	return false, ""
}
