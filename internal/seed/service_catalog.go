// 服务字典、团队目录与员工扩展（ownership / level / team / profile）。
//
// 技能层删除后，这里就是派单的唯一数据来源：Stage 1 用服务字典做归属解析，
// 用员工扩展决定谁负责、级别是否够接 P0。见 docs/DISPATCH_PIPELINE.md §5。
//
// 编号约定：ServiceID 从 2001 起、TeamID 从 1 起，且必须保持稳定——
// 员工扩展与评测金标都按 ID 引用它们。
package seed

import (
	"github.com/mac/helpdesk-agent/internal/domain"
)

// Teams 返回团队目录。ID 稳定：EmployeeExtension.TeamID 与 Service.TeamID 都指向这里。
func Teams() []domain.Team {
	return []domain.Team{
		{ID: 1, Name: "接口组"},
		{ID: 2, Name: "数据组"},
		{ID: 3, Name: "权限计费组"},
		{ID: 4, Name: "运维组"},
		{ID: 5, Name: "前端组"},
		{ID: 6, Name: "安全组"},
		{ID: 7, Name: "综合组"},
		{ID: 8, Name: "实习组"},
	}
}

// Services 返回服务字典。每条 = 一个可运维单元，OwnerID 必填。
//
// Aliases 与 Keywords 是 BM25 主要命中面：真实工单里"下单挂了""订单接口
// 报 500"这种口语不会精确写出服务名，别名覆盖不到就是 no_service_match。
func Services() []domain.Service {
	return []domain.Service{
		{
			ID: 2001, Name: "核心下单接口",
			Aliases:     []string{"下单接口", "订单接口", "创建订单", "下单服务"},
			Description: "对外提供订单创建、修改、取消的同步接口；日均调用 20 万次，P0 服务。",
			Keywords:    []string{"下单", "订单", "500", "超时", "接口"},
			OwnerID:     101, BackupOwnerID: 107, TeamID: 1,
		},
		{
			ID: 2002, Name: "报表与 BI 查询",
			Aliases:     []string{"报表", "BI", "看板", "月报", "报表查询"},
			Description: "运营与财务侧的报表生成服务，含月度汇总看板与临时查询导出。",
			Keywords:    []string{"报表", "看板", "月报", "慢查询", "导出"},
			OwnerID:     102, BackupOwnerID: 107, TeamID: 2,
		},
		{
			ID: 2003, Name: "用户账号中心",
			Aliases:     []string{"账号", "登录", "注册", "用户中心", "成员管理"},
			Description: "账号注册、登录、锁定解锁、密码重置。工单常见词包括\"登不上\"\"账号被锁\"。",
			Keywords:    []string{"账号", "登录", "锁定", "解锁", "密码"},
			OwnerID:     106, BackupOwnerID: 104, TeamID: 3,
		},
		{
			ID: 2004, Name: "计费与账单",
			Aliases:     []string{"计费", "账单", "扣费", "发票", "订阅"},
			Description: "订阅计费、账单生成、发票开具。异常常与账号权限、支付通道联动。",
			Keywords:    []string{"计费", "账单", "扣费", "发票", "订阅"},
			OwnerID:     106, BackupOwnerID: 107, TeamID: 3,
		},
		{
			ID: 2005, Name: "网络与机房链路",
			Aliases:     []string{"网络", "链路", "机房", "专线", "带宽"},
			Description: "内部机房与专线、DNS、跨机房链路。任何大面积\"打不开\"都可能落到这里。",
			Keywords:    []string{"网络", "DNS", "链路", "机房", "带宽"},
			OwnerID:     103, BackupOwnerID: 107, TeamID: 4,
		},
		{
			ID: 2006, Name: "私有化部署环境",
			Aliases:     []string{"私有化", "部署", "现场部署", "驻场"},
			Description: "客户私有化环境的部署、升级、参数配置。咨询类工单集中在这里。",
			Keywords:    []string{"私有化", "部署", "硬件要求", "端口", "出网"},
			OwnerID:     103, BackupOwnerID: 104, TeamID: 4,
		},
		{
			ID: 2007, Name: "Web 控制台",
			Aliases:     []string{"控制台", "后台", "管理台", "Web 页面"},
			Description: "面向客户与内部运营的管理控制台前端，含权限路由与页面渲染。",
			Keywords:    []string{"控制台", "页面", "白屏", "前端", "浏览器"},
			OwnerID:     105, BackupOwnerID: 108, TeamID: 5,
		},
		{
			ID: 2008, Name: "桌面客户端",
			Aliases:     []string{"桌面", "客户端", "Windows 端", "Mac 端"},
			Description: "Windows / macOS 桌面客户端，含自动升级与本地缓存。",
			Keywords:    []string{"客户端", "桌面", "闪退", "升级", "Mac", "Windows"},
			OwnerID:     105, BackupOwnerID: 108, TeamID: 5,
		},
		{
			ID: 2009, Name: "数据库集群",
			Aliases:     []string{"数据库", "MySQL", "主从", "分库分表", "连接池"},
			Description: "生产 MySQL 集群、连接池、主从同步。慢查询与连接数打满是常见症状。",
			Keywords:    []string{"数据库", "连接池", "慢查询", "主从", "同步"},
			OwnerID:     102, BackupOwnerID: 101, TeamID: 2,
		},
		{
			ID: 2010, Name: "安全加固与合规",
			Aliases:     []string{"安全", "加固", "合规", "漏洞", "渗透"},
			Description: "系统层安全加固、漏洞响应、合规审计。P0 通常直接触发。",
			Keywords:    []string{"安全", "漏洞", "加固", "合规", "渗透"},
			OwnerID:     104, BackupOwnerID: 103, TeamID: 6,
		},
		{
			ID: 2011, Name: "数据同步管道",
			Aliases:     []string{"同步", "管道", "任务", "延迟", "数据不同步"},
			Description: "上下游数据同步任务，含定时与流式；常见症状是\"数据延迟\"\"对不上账\"。",
			Keywords:    []string{"同步", "管道", "延迟", "对账", "任务"},
			OwnerID:     101, BackupOwnerID: 102, TeamID: 1,
		},
		{
			ID: 2012, Name: "硬件与设备",
			Aliases:     []string{"硬件", "设备", "服务器", "打印机", "POS"},
			Description: "服务器、终端、外设（POS / 打印机）等硬件故障。",
			Keywords:    []string{"硬件", "服务器", "打印机", "POS", "硬盘"},
			OwnerID:     103, BackupOwnerID: 0, TeamID: 4,
		},
		{
			ID: 2013, Name: "接口鉴权与令牌",
			Aliases:     []string{"鉴权", "令牌", "token", "签名", "401"},
			Description: "对外接口的鉴权、签名与令牌刷新；工单常见词\"401\"\"token 过期\"。",
			Keywords:    []string{"鉴权", "令牌", "token", "签名", "401"},
			OwnerID:     101, BackupOwnerID: 104, TeamID: 1,
		},
		{
			ID: 2014, Name: "员工工具台",
			Aliases:     []string{"内部工具", "员工工具", "工单后台", "客服台"},
			Description: "面向内部员工与客服的操作台。跨系统兜底，多数\"其他\"类问题最终归到这里。",
			Keywords:    []string{"内部", "工具台", "客服", "工单"},
			OwnerID:     107, BackupOwnerID: 0, TeamID: 7,
		},
	}
}

// EmployeeExtensions 返回与 Employees() 一一对应的扩展记录。
//
// 关键设计：
//   - senior 只给真正的资深员工（张伟、王强、袁泉、黄磊）。这样 P0 硬约束才有区分度：
//     若全员 senior，P0 gate 就是空规则；若 senior 太多，Stage 1 命中率会失真。
//   - OwnedServices 与 Services() 双向对齐：GrantOwnership 出现的每条服务
//     都能在 Services() 找到；反向亦然。这条不变量由 TestExtensionsMatchServices 断言。
//   - Profile 是给 Stage 2 LLM 看的自然语言摘要，刻意避免直接列出所有服务名 ——
//     否则"LLM 是不是理解服务归属"退化为"LLM 是不是在做字符串匹配"。
func EmployeeExtensions() []domain.EmployeeExtension {
	zhang := domain.NewEmployeeExtension(101, domain.LevelSenior, 1)
	zhang = *zhang.GrantOwnership(2001, domain.OwnershipOwner).
		GrantOwnership(2011, domain.OwnershipOwner).
		GrantOwnership(2013, domain.OwnershipOwner).
		GrantOwnership(2009, domain.OwnershipBackup)
	zhang.Profile = "接口组骨干，负责对外核心接口，尤其熟悉下单链路和令牌鉴权；" +
		"数据库层能兜底但非首选。历史处理过高并发写导致的下单雪崩类故障。"

	wang := domain.NewEmployeeExtension(102, domain.LevelSenior, 2)
	wang = *wang.GrantOwnership(2002, domain.OwnershipOwner).
		GrantOwnership(2009, domain.OwnershipOwner).
		GrantOwnership(2011, domain.OwnershipBackup)
	wang.Profile = "数据组负责人，数据库调优与慢查询排查经验最丰富；报表与 BI 侧的问题第一时间找他。"

	chen := domain.NewEmployeeExtension(103, domain.LevelMid, 4)
	chen = *chen.GrantOwnership(2005, domain.OwnershipOwner).
		GrantOwnership(2006, domain.OwnershipOwner).
		GrantOwnership(2012, domain.OwnershipOwner).
		GrantOwnership(2010, domain.OwnershipBackup)
	chen.Profile = "运维组主力，负责网络、机房链路与私有化现场部署；硬件类工单基本都走他。"

	yuan := domain.NewEmployeeExtension(104, domain.LevelSenior, 6)
	yuan = *yuan.GrantOwnership(2010, domain.OwnershipOwner).
		GrantOwnership(2003, domain.OwnershipBackup).
		GrantOwnership(2006, domain.OwnershipBackup).
		GrantOwnership(2013, domain.OwnershipBackup)
	yuan.Profile = "安全组唯一员工，负责漏洞响应与合规；账号鉴权异常时会被拉进来一起看。"

	qian := domain.NewEmployeeExtension(105, domain.LevelMid, 5)
	qian = *qian.GrantOwnership(2007, domain.OwnershipOwner).
		GrantOwnership(2008, domain.OwnershipOwner)
	qian.Profile = "前端组骨干，控制台与桌面客户端的疑难杂症归口；权限路由与渲染性能问题最擅长。"

	zhou := domain.NewEmployeeExtension(106, domain.LevelMid, 3)
	zhou = *zhou.GrantOwnership(2003, domain.OwnershipOwner).
		GrantOwnership(2004, domain.OwnershipOwner)
	zhou.Profile = "权限计费组唯一员工，账号锁定与账单异常是他的日常；跨系统的账号打通问题会拉上安全组。"

	huang := domain.NewEmployeeExtension(107, domain.LevelSenior, 7)
	huang = *huang.GrantOwnership(2014, domain.OwnershipOwner).
		GrantOwnership(2001, domain.OwnershipBackup).
		GrantOwnership(2002, domain.OwnershipBackup).
		GrantOwnership(2004, domain.OwnershipBackup).
		GrantOwnership(2005, domain.OwnershipBackup)
	huang.Profile = "全栈通才，跨栈救火队员；主战场是内部工具台，也是多个核心服务的备份。"

	li := domain.NewEmployeeExtension(108, domain.LevelJunior, 8)
	li = *li.GrantOwnership(2007, domain.OwnershipBackup).
		GrantOwnership(2008, domain.OwnershipBackup)
	li.Profile = "实习坐席，前端类工单 backup。响应快、任务多，但独立处理复杂故障的能力有限。"

	zheng := domain.NewEmployeeExtension(109, domain.LevelSenior, 1)
	zheng.Profile = "已离职。保留记录用于测试 Active=false 的过滤路径。"

	return []domain.EmployeeExtension{zhang, wang, chen, yuan, qian, zhou, huang, li, zheng}
}

// ExtensionsByEmployeeID 返回以 EmployeeID 为键的索引，方便构造 assign.Directory。
func ExtensionsByEmployeeID() map[int64]domain.EmployeeExtension {
	ret := make(map[int64]domain.EmployeeExtension)
	for _, ext := range EmployeeExtensions() {
		ret[ext.EmployeeID] = ext
	}
	return ret
}

// ServicesByID 返回以 ServiceID 为键的索引。
func ServicesByID() map[int64]domain.Service {
	ret := make(map[int64]domain.Service)
	for _, s := range Services() {
		ret[s.ID] = s
	}
	return ret
}
