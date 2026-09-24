package seed

import (
	"context"
	"testing"

	"github.com/mac/helpdesk-agent/internal/assign"
	"github.com/mac/helpdesk-agent/internal/domain"
)

// TestServiceCatalog_Integrity 锁定服务字典与员工扩展的一致性。
//
// 这些不变量若被破坏，pipeline 会静默给出错误的 ownership 分：
//   - 员工扩展引用了不存在的服务 → 该 ownership 加分从未生效
//   - 服务声明的 owner 未在扩展里出现 → Stage 1 与 Stage 2 的 prompt 会不一致
//   - 服务 / 扩展引用了不存在的 team → team 匹配走空
//   - 服务 owner 与员工表脱节 → 派给了不存在的人
//
// 这一层是数据完整性测试，不涉及算法；出问题时应改 seed，不是改 pipeline。
func TestServiceCatalog_Integrity(t *testing.T) {
	services := Services()
	teams := Teams()
	exts := EmployeeExtensions()
	emps := Employees()

	teamIDs := map[int64]bool{}
	for _, team := range teams {
		if err := team.Validate(); err != nil {
			t.Fatalf("team %d invalid: %v", team.ID, err)
		}
		teamIDs[team.ID] = true
	}

	empIDs := map[int64]bool{}
	for _, emp := range emps {
		empIDs[emp.ID] = true
	}

	serviceIDs := map[int64]domain.Service{}
	for _, s := range services {
		if err := s.Validate(); err != nil {
			t.Fatalf("service %d invalid: %v", s.ID, err)
		}
		if !empIDs[s.OwnerID] {
			t.Errorf("service %d owner=%d 不在 Employees 表中", s.ID, s.OwnerID)
		}
		if s.BackupOwnerID != 0 && !empIDs[s.BackupOwnerID] {
			t.Errorf("service %d backup=%d 不在 Employees 表中", s.ID, s.BackupOwnerID)
		}
		if !teamIDs[s.TeamID] {
			t.Errorf("service %d team=%d 不存在", s.ID, s.TeamID)
		}
		serviceIDs[s.ID] = s
	}

	extByEmp := map[int64]domain.EmployeeExtension{}
	for _, e := range exts {
		if err := e.Validate(); err != nil {
			t.Fatalf("employee extension %d invalid: %v", e.EmployeeID, err)
		}
		if !empIDs[e.EmployeeID] {
			t.Errorf("extension %d 无对应 Employee", e.EmployeeID)
		}
		if !teamIDs[e.TeamID] {
			t.Errorf("extension %d team=%d 不存在", e.EmployeeID, e.TeamID)
		}
		for sid, kind := range e.OwnedServices {
			svc, ok := serviceIDs[sid]
			if !ok {
				t.Errorf("extension %d 引用不存在的服务 %d", e.EmployeeID, sid)
				continue
			}
			// 双向对齐：employee 声称自己是 owner/backup，服务侧的 OwnerID/BackupOwnerID 必须匹配。
			switch kind {
			case domain.OwnershipOwner:
				if svc.OwnerID != e.EmployeeID {
					t.Errorf("extension %d 声称 owns 服务 %d，但服务侧 OwnerID=%d",
						e.EmployeeID, sid, svc.OwnerID)
				}
			case domain.OwnershipBackup:
				if svc.BackupOwnerID != e.EmployeeID {
					t.Errorf("extension %d 声称 backup 服务 %d，但服务侧 BackupOwnerID=%d",
						e.EmployeeID, sid, svc.BackupOwnerID)
				}
			}
		}
		extByEmp[e.EmployeeID] = e
	}

	// 反向：服务侧的 OwnerID / BackupOwnerID 都必须在对应员工扩展里出现。
	for _, s := range services {
		if owner, ok := extByEmp[s.OwnerID]; !ok {
			t.Errorf("service %d owner=%d 无扩展记录", s.ID, s.OwnerID)
		} else if owner.DirectOwnership(s.ID) != domain.OwnershipOwner {
			t.Errorf("service %d owner=%d 扩展中未标记 OwnershipOwner", s.ID, s.OwnerID)
		}
		if s.BackupOwnerID == 0 {
			continue
		}
		if backup, ok := extByEmp[s.BackupOwnerID]; !ok {
			t.Errorf("service %d backup=%d 无扩展记录", s.ID, s.BackupOwnerID)
		} else if backup.DirectOwnership(s.ID) != domain.OwnershipBackup {
			t.Errorf("service %d backup=%d 扩展中未标记 OwnershipBackup", s.ID, s.BackupOwnerID)
		}
	}
}

// TestServiceCatalog_P0DispatchRequiresSenior 用真实服务字典跑一次派单，
// 验证 P0 gate 与 ownership 组合下至少有一条 sensible 路径。
//
// 选服务 2001 核心下单接口：owner 101 是 senior（张伟），backup 107 是 senior（黄磊）。
// P0 工单 → 服务解析命中 2001 → 只有 101/107 是 P0 候选 → 101 ownership 更高等胜出。
func TestServiceCatalog_P0DispatchRequiresSenior(t *testing.T) {
	dir := assign.Directory{
		Employees:  Employees(),
		Extensions: ExtensionsByEmployeeID(),
		Services:   ServicesByID(),
	}
	resolver := assign.StaticServiceResolver{
		Default: []assign.ServiceMatch{{ServiceID: 2001, Score: 0.9, Reason: "fixture"}},
	}
	chooser := assign.NewScriptedChooser(map[int64]assign.ScriptedChoice{
		1: {AssigneeID: 101, Rationale: "fallback", Confidence: 0.8},
	})
	p := assign.NewPipeline(resolver, assign.NoopSimilarIndex{}, chooser)

	ticket := domain.Ticket{
		ID: 1, Title: "下单挂了", Description: "核心下单接口返回 500，影响所有用户",
		Category: domain.CategoryIncident, Priority: domain.PriorityP0,
		Status: domain.TicketStatusPending,
	}
	d, err := p.Run(context.Background(), ticket, dir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if d.AssigneeID != 101 {
		t.Errorf("P0 下单接口应派给 owner 张伟(101)，实际 %d；Reason=%s", d.AssigneeID, d.Reason)
	}
	// 105/106/103 是 mid 或非核心 owner；108 junior；109 已离职。P0 gate 应过滤掉。
	for _, c := range d.Candidates {
		if c.EmployeeID == 105 || c.EmployeeID == 106 || c.EmployeeID == 103 || c.EmployeeID == 108 {
			if !c.Filtered {
				t.Errorf("员工 %d 级别不满足 P0，应被过滤，实际未过滤", c.EmployeeID)
			}
		}
	}
}

// TestServiceCatalog_BM25ResolvesRealTicket 验证 BM25 服务解析对一条真实风格工单
// 能命中正确服务 —— 这是 Stage 1 "no_service_match" 判据的正面回归。
//
// 与 StaticServiceResolver 版本互补：那条测 pipeline 逻辑，这条测字典内容本身。
func TestServiceCatalog_BM25ResolvesRealTicket(t *testing.T) {
	cases := []struct {
		name     string
		title    string
		body     string
		expected int64
	}{
		{"下单500", "核心下单接口持续返回 500", "订单无法创建，从 10:20 起错误率 35%", 2001},
		{"报表超时", "月度报表查询超时", "运营反馈报表加载 60 秒失败", 2002},
		{"账号锁定", "员工账号无法登录后台", "多名员工密码正确但提示账号被锁", 2003},
		{"私有化部署", "希望了解私有化部署的硬件要求", "客户询问最低服务器配置与端口", 2006},
	}
	dir := assign.Directory{
		Employees:  Employees(),
		Extensions: ExtensionsByEmployeeID(),
		Services:   ServicesByID(),
	}
	resolver := assign.NewBM25ServiceResolver(3)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tk := domain.Ticket{
				ID: 1, Title: tc.title, Description: tc.body,
				Category: domain.CategoryIncident, Priority: domain.PriorityP2,
				Status: domain.TicketStatusPending,
			}
			matches, err := resolver.Resolve(context.Background(), tk, dir)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if len(matches) == 0 {
				t.Fatalf("BM25 未召回任何服务；字典或分词需要调整")
			}
			if matches[0].ServiceID != tc.expected {
				got := make([]int64, 0, len(matches))
				scores := make([]float64, 0, len(matches))
				for _, m := range matches {
					got = append(got, m.ServiceID)
					scores = append(scores, m.Score)
				}
				t.Errorf("期望 top1=%d，实际 %v (scores %v)", tc.expected, got, scores)
			}
		})
	}
}
