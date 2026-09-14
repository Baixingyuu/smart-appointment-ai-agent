// Package seed 提供演示与测试用的种子数据。
//
// 技能树采用层级结构（ParentID），与业务侧的技能分类保持一致；
// 匹配只看技能集合，层级仅用于人工维护与展示。
package seed

import (
	"github.com/mac/agentdesk/internal/domain"
	"github.com/mac/agentdesk/internal/rag"
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

// KnowledgeChunks 返回演示与离线评测用的知识库语料。
//
// 内容与技能树对齐（接口/数据库/账号/部署），使「检索命中 → 按技能派单」
// 这条链路在演示中可完整走通。规模刻意保持很小：一期的重点不是检索质量，
// 而是链路正确性与失败归因。
func KnowledgeChunks() []rag.Chunk {
	return []rag.Chunk{
		{
			ID: "kb-interface-auth", DocID: "doc-interface", Title: "接口鉴权失败排查",
			Content: "接口返回 401 表示鉴权令牌过期或签名不正确。请先检查请求头 Authorization " +
				"是否携带有效令牌，再确认服务器系统时间是否准确——时间偏差过大会导致签名校验失败。" +
				"若确认令牌有效仍报错，请核对签名算法与密钥是否与平台一致。",
			Keywords: []string{"接口", "401", "鉴权", "令牌", "Authorization", "签名"},
		},
		{
			ID: "kb-interface-timeout", DocID: "doc-interface", Title: "接口超时排查",
			Content: "接口超时的常见原因是下游依赖响应缓慢或连接池耗尽。建议先查看调用链耗时分布，" +
				"定位是网络、数据库还是第三方服务造成的延迟，再确认连接池配置与慢查询情况。",
			Keywords: []string{"接口", "超时", "连接池", "慢查询", "调用链"},
		},
		{
			ID: "kb-account-lock", DocID: "doc-account", Title: "账号被锁定处理",
			Content: "连续多次输入错误密码会触发账号锁定，通常锁定十五分钟。管理员可在成员管理中" +
				"重置密码并解锁账号。若账号绑定的邮箱不可用，需要由组织管理员代为重置。",
			Keywords: []string{"账号", "锁定", "密码", "解锁", "成员管理"},
		},
		{
			ID: "kb-account-reset", DocID: "doc-account", Title: "重置密码方法",
			Content: "在登录页点击忘记密码，输入绑定邮箱获取验证码即可重置。重置成功后" +
				"所有设备都需要重新登录。若未收到验证邮件，请检查垃圾邮件并确认邮箱地址无误。",
			Keywords: []string{"重置密码", "忘记密码", "验证码", "登录"},
		},
		{
			ID: "kb-deploy-hardware", DocID: "doc-deploy", Title: "私有化部署硬件要求",
			Content: "私有化部署最低需要四核八 GB 内存，建议十六 GB 以上；需要可访问外部模型服务的" +
				"网络出口，或在内网部署独立的模型服务。磁盘建议预留一百 GB 以上用于知识与日志。",
			Keywords: []string{"私有化", "部署", "硬件", "内存", "磁盘"},
		},
		{
			ID: "kb-deploy-network", DocID: "doc-deploy", Title: "私有化部署网络配置",
			Content: "私有化部署需要开放应用端口与数据库端口；若使用外部模型服务还需开放出网访问。" +
				"内网部署模型服务时，请确认域名解析与证书配置正确。",
			Keywords: []string{"私有化", "部署", "网络", "端口", "出网"},
		},
		{
			ID: "kb-db-pool", DocID: "doc-database", Title: "数据库连接池耗尽",
			Content: "连接池耗尽的典型表现是请求大量超时且数据库连接数打满。常见原因是慢查询" +
				"长期占用连接，或连接未正确释放。建议先排查慢查询，再评估连接池上限是否需要调整。",
			Keywords: []string{"数据库", "连接池", "耗尽", "慢查询"},
		},
	}
}
