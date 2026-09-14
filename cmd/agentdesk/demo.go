package main

import (
	"fmt"
	"strings"

	"github.com/mac/agentdesk/internal/assign"
	"github.com/mac/agentdesk/internal/domain"
	"github.com/mac/agentdesk/internal/seed"
	"github.com/mac/agentdesk/internal/store"
	"github.com/mac/agentdesk/internal/ticket"
)

// runDemo 跑一遍完整工单链路，用于人工验证与演示。
//
// 覆盖：建单 → 按技能派单 → 接单 → 升级换人 → 完成，
// 以及去重与「缺信息不阻塞建单」两个边界。
func runDemo() error {
	st := store.NewMemory()
	if err := seed.Load(st); err != nil {
		return err
	}
	svc := ticket.New(st, assign.New(assign.DefaultWeights()))

	fmt.Println("处理人名册（技能 → 说明）")
	fmt.Println(strings.Repeat("-", 68))
	for _, emp := range st.ListEmployees() {
		state := "在职"
		if !emp.Active {
			state = "离职"
		}
		fmt.Printf("%-4d %-22s %-4s 并发上限 %d  技能 %v\n",
			emp.ID, emp.Name, state, emp.MaxConcurrent, emp.Skills.IDs())
	}

	inputs := seed.DemoInputs()
	for i, input := range inputs {
		fmt.Printf("\n【场景 %d】%s\n", i+1, input.Title)
		fmt.Println(strings.Repeat("-", 68))

		result, err := svc.Create(input)
		if err != nil {
			return fmt.Errorf("场景 %d 建单失败: %w", i+1, err)
		}
		printAssignment(result.Assignment)
		printTicketState(st, result.Ticket)

		if i == 0 {
			// 对首个场景演示去重：同会话再次建单应追加而非新建。
			fmt.Println("\n  ↳ 同一会话再次提交同类问题：")
			again, err := svc.Create(input)
			if err != nil {
				return err
			}
			fmt.Printf("     去重=%v，复用工单 T%d，工单总数 %d\n",
				again.Deduped, again.Ticket.ID, len(st.ListTickets()))
		}
		if i == 1 {
			// 对第二个场景演示升级：应排除原处理人。
			fmt.Println("\n  ↳ 升级该工单（排除原处理人）：")
			updated, log, err := svc.Escalate(result.Ticket.ID, "问题超出该处理人能力范围")
			if err != nil {
				return err
			}
			printAssignment(log)
			fmt.Printf("     处理人 %d → %d\n", result.Ticket.AssigneeID, updated.AssigneeID)
		}
	}

	// 演示完整流转：接单 → 完成。
	first := st.ListTickets()[0]
	fmt.Printf("\n【流转演示】工单 T%d\n", first.ID)
	fmt.Println(strings.Repeat("-", 68))
	accepted, err := svc.Accept(first.ID, first.AssigneeID)
	if err != nil {
		return err
	}
	fmt.Printf("接单   → 状态 %s\n", accepted.Status)
	resolved, err := svc.Resolve(first.ID, first.AssigneeID)
	if err != nil {
		return err
	}
	fmt.Printf("完成   → 状态 %s\n", resolved.Status)

	detail, err := svc.Detail(first.ID)
	if err != nil {
		return err
	}
	fmt.Printf("\n工单 T%d 完整时间线（%d 条进展，%d 次指派）\n",
		first.ID, len(detail.Progress), len(detail.Assignments))
	fmt.Println(strings.Repeat("-", 68))
	for _, p := range detail.Progress {
		fmt.Printf("  [%-9s] %s\n", p.Kind, p.Content)
	}
	return nil
}

func printAssignment(log domain.AssignmentLog) {
	outcome := map[domain.AssignmentOutcome]string{
		domain.OutcomeMatched:      "匹配成功",
		domain.OutcomeFallbackPool: "退回待认领池",
		domain.OutcomeNoCandidate:  "无可用处理人",
	}[log.Outcome]

	assignee := "（未指派）"
	if log.AssigneeID > 0 {
		assignee = fmt.Sprintf("员工 %d", log.AssigneeID)
	}
	fmt.Printf("  派单：%s → %s\n", outcome, assignee)
	fmt.Printf("  理由：%s\n", log.Reason)
}

func printTicketState(st store.Store, t domain.Ticket) {
	assignee := "未指派"
	if t.AssigneeID > 0 {
		if emp, err := st.GetEmployee(t.AssigneeID); err == nil {
			assignee = emp.Name
		}
	}
	info := "信息完整"
	if !t.InfoComplete() {
		info = fmt.Sprintf("缺失信息 %v", t.MissingInfo)
	}
	fmt.Printf("  工单 T%d 状态=%s 处理人=%s 需求技能=%v %s\n",
		t.ID, t.Status, assignee, t.RequiredSkill.IDs(), info)
}
