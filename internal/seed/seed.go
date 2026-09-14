// Package seed 提供演示与测试用的种子数据。
//
// 技能树采用层级结构（ParentID），与业务侧的技能分类保持一致；
// 匹配只看技能集合，层级仅用于人工维护与展示。
package seed

import (
	"github.com/mac/agentdesk/internal/domain"
	"github.com/mac/agentdesk/internal/store"
)

// 根技能 ID。
//
// 必须避开 1..12：那一段被叶子技能占用，且与评测集（eval/datasets）的
// 技能编号一一对应，便于人工核对。根节点只是层级容器，不参与匹配。
const (
	rootInfra    int64 = 1001
	rootBusiness int64 = 1002
)

// Skills 返回技能树。
//
// 编号与评测集（eval/datasets）中的技能编号保持一致，便于人工核对：
// 1=网络 2=接口 3=数据库 4=性能 5=安全 6=前端 7=客户端 8=部署
// 9=账号权限 10=计费 11=数据同步 12=硬件
func Skills() []domain.Skill {
	return []domain.Skill{
		{ID: rootInfra, Name: "基础设施"},
		{ID: rootBusiness, Name: "业务系统"},

		{ID: 1, ParentID: rootInfra, Name: "网络"},
		{ID: 8, ParentID: rootInfra, Name: "部署"},
		{ID: 12, ParentID: rootInfra, Name: "硬件"},

		{ID: 2, ParentID: rootBusiness, Name: "接口"},
		{ID: 3, ParentID: rootBusiness, Name: "数据库"},
		{ID: 4, ParentID: rootBusiness, Name: "性能"},
		{ID: 5, ParentID: rootBusiness, Name: "安全"},
		{ID: 6, ParentID: rootBusiness, Name: "前端"},
		{ID: 7, ParentID: rootBusiness, Name: "客户端"},
		{ID: 9, ParentID: rootBusiness, Name: "账号权限"},
		{ID: 10, ParentID: rootBusiness, Name: "计费"},
		{ID: 11, ParentID: rootBusiness, Name: "数据同步"},
	}
}

// Employees 返回演示用处理人。
//
// 技能分布刻意设计为「专才 + 通才 + 实习坐席」：
//   - 专才技能窄但深，是同类问题的最佳人选；
//   - 通才覆盖面广但 Jaccard 相似度天然偏低（分母被撑大）；
//   - 实习坐席技能少且偏门，用于检验「无技能命中时退回待认领池」。
//
// 这样设计能让派单器「技能优先于负载」的性质在演示中直接可见。
func Employees() []domain.Employee {
	return []domain.Employee{
		{
			ID: 101, Name: "张伟（接口组）", Active: true,
			Skills: domain.NewSkillSet(2, 3, 11), MaxConcurrent: 5, Recency: 0.85,
		},
		{
			ID: 102, Name: "王强（数据库组）", Active: true,
			Skills: domain.NewSkillSet(3, 4), MaxConcurrent: 4, Recency: 0.80,
		},
		{
			ID: 103, Name: "陈磊（运维组）", Active: true,
			Skills: domain.NewSkillSet(8, 1, 12), MaxConcurrent: 6, Recency: 0.75,
		},
		{
			ID: 104, Name: "袁泉（安全组）", Active: true,
			Skills: domain.NewSkillSet(5, 8), MaxConcurrent: 3, Recency: 0.70,
		},
		{
			ID: 105, Name: "钱枫（前端组）", Active: true,
			Skills: domain.NewSkillSet(6, 7), MaxConcurrent: 4, Recency: 0.78,
		},
		{
			ID: 106, Name: "周涛（权限计费组）", Active: true,
			Skills: domain.NewSkillSet(9, 10), MaxConcurrent: 4, Recency: 0.72,
		},
		{
			ID: 107, Name: "黄磊（全栈通才）", Active: true,
			Skills:        domain.NewSkillSet(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12),
			MaxConcurrent: 8, Recency: 0.60,
		},
		{
			ID: 108, Name: "李娜（实习坐席）", Active: true,
			Skills: domain.NewSkillSet(6), MaxConcurrent: 10, Recency: 0.95,
		},
		{
			ID: 109, Name: "郑爽（已离职）", Active: false,
			Skills: domain.NewSkillSet(2, 3, 4), MaxConcurrent: 5, Recency: 0.90,
		},
	}
}

// Load 把种子数据写入存储。
func Load(st store.Store) error {
	for _, skill := range Skills() {
		if err := st.SaveSkill(skill); err != nil {
			return err
		}
	}
	for _, emp := range Employees() {
		if err := st.SaveEmployee(emp); err != nil {
			return err
		}
	}
	return nil
}

// DemoInputs 返回演示与集成测试用的建单输入。
//
// 每个用例对应一类真实场景，并刻意覆盖派单器的不同分支：
// 技能主导、专才对通才、无技能命中、信息缺失。
func DemoInputs() []domain.TicketInput {
	return []domain.TicketInput{
		{
			Title:          "核心接口持续返回 500，订单无法创建",
			Description:    "自今日 10:20 起核心下单接口错误率升至 35%，影响全部线上用户。",
			Category:       domain.CategoryIncident,
			Priority:       domain.PriorityP0,
			RequiredSkill:  domain.NewSkillSet(2, 3),
			ConversationID: 1001,
			SourceChannel:  "web",
		},
		{
			Title:          "报表查询超时",
			Description:    "运营反馈月度报表加载超过 60 秒后失败。",
			Category:       domain.CategoryIncident,
			Priority:       domain.PriorityP2,
			RequiredSkill:  domain.NewSkillSet(3, 4),
			ConversationID: 1002,
			SourceChannel:  "web",
			// 缺少复现步骤与影响范围：先建单再补充，不阻塞。
			MissingInfo: []string{"复现步骤", "影响范围"},
		},
		{
			Title:          "员工账号无法登录后台",
			Description:    "多名员工反馈密码正确但提示账号被锁定。",
			Category:       domain.CategoryIncident,
			Priority:       domain.PriorityP1,
			RequiredSkill:  domain.NewSkillSet(9),
			ConversationID: 1003,
			SourceChannel:  "web",
		},
		{
			Title:          "希望了解私有化部署的硬件要求",
			Description:    "客户计划私有化部署，询问最低服务器配置与网络要求。",
			Category:       domain.CategoryConsultation,
			Priority:       domain.PriorityP3,
			RequiredSkill:  domain.NewSkillSet(8, 1),
			ConversationID: 1004,
			SourceChannel:  "web",
		},
	}
}
