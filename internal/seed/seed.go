// Package seed 提供演示与测试用的种子数据。
//
// 人员名册在本文件；派单真正依赖的归属关系（负责哪个服务、级别、团队）
// 在 service_catalog.go 的服务字典与员工扩展里。
package seed

import (
	"github.com/mac/helpdesk-agent/internal/domain"
	"github.com/mac/helpdesk-agent/internal/rag"
	"github.com/mac/helpdesk-agent/internal/store"
)

// Employees 返回演示用处理人。
//
// 名册刻意做出层次差异：有人只负责一个系统，有人负责一片（全栈通才），
// 有人实习、有人已离职。派单依据是他们负责哪些服务与级别（见
// service_catalog.go 的 ExtensionsByEmployeeID），负载与近期表现是次级信号；
// 离职者与满载者用于检验"硬约束过滤后无人可选就退待认领池"。
func Employees() []domain.Employee {
	return []domain.Employee{
		{ID: 101, Name: "张伟（接口组）", Active: true, MaxConcurrent: 5, Recency: 0.85},
		{ID: 102, Name: "王强（数据库组）", Active: true, MaxConcurrent: 4, Recency: 0.80},
		{ID: 103, Name: "陈磊（运维组）", Active: true, MaxConcurrent: 6, Recency: 0.75},
		{ID: 104, Name: "袁泉（安全组）", Active: true, MaxConcurrent: 3, Recency: 0.70},
		{ID: 105, Name: "钱枫（前端组）", Active: true, MaxConcurrent: 4, Recency: 0.78},
		{ID: 106, Name: "周涛（权限计费组）", Active: true, MaxConcurrent: 4, Recency: 0.72},
		{ID: 107, Name: "黄磊（全栈通才）", Active: true, MaxConcurrent: 8, Recency: 0.60},
		{ID: 108, Name: "李娜（实习坐席）", Active: true, MaxConcurrent: 10, Recency: 0.95},
		{ID: 109, Name: "郑爽（已离职）", Active: false, MaxConcurrent: 5, Recency: 0.90},
	}
}

// NewBM25Retriever 用知识库语料构造 BM25 检索器。
//
// 为什么离线默认用 BM25 而不是「字符哈希向量化」：后者只是把 bigram
// 随机投影到固定维度，没有检索语义，实测导致模型不信任检索结果并反复
// 改写查询重试。BM25 是正经的检索算法，对中文客服 FAQ 这类词面重叠
// 场景效果好、打分可解释、且不需要任何外部服务。
//
// 生产环境若要用模型向量化，替换为 rag.NewOpenAIEmbedder 即可，
// 检索、门控与工具层都不受影响。
func NewBM25Retriever(options rag.Options) *rag.Retriever {
	chunks := KnowledgeChunks()
	documents := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		documents = append(documents, chunk.Title+" "+chunk.Content)
	}
	bm25 := rag.NewBM25Embedder(documents)
	return rag.NewWithScorer(chunks, bm25, bm25, options)
}

// Load 把种子数据写入存储。
func Load(st store.Store) error {
	for _, emp := range Employees() {
		if err := st.SaveEmployee(emp); err != nil {
			return err
		}
	}
	return nil
}

// DemoInputs 返回演示与集成测试用的建单输入。
//
// 每个用例对应一类真实场景，并刻意覆盖归属派单的不同分支：
// owner 直接命中、P0 触发级别门槛、无归属命中退待认领池、信息缺失仍建单。
func DemoInputs() []domain.TicketInput {
	return []domain.TicketInput{
		{
			Title:          "核心接口持续返回 500，订单无法创建",
			Description:    "自今日 10:20 起核心下单接口错误率升至 35%，影响全部线上用户。",
			Category:       domain.CategoryIncident,
			Priority:       domain.PriorityP0,
			ConversationID: 1001,
			SourceChannel:  "web",
		},
		{
			Title:          "报表查询超时",
			Description:    "运营反馈月度报表加载超过 60 秒后失败。",
			Category:       domain.CategoryIncident,
			Priority:       domain.PriorityP2,
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
			ConversationID: 1003,
			SourceChannel:  "web",
		},
		{
			Title:          "希望了解私有化部署的硬件要求",
			Description:    "客户计划私有化部署，询问最低服务器配置与网络要求。",
			Category:       domain.CategoryConsultation,
			Priority:       domain.PriorityP3,
			ConversationID: 1004,
			SourceChannel:  "web",
		},
	}
}

// KnowledgeChunks 返回演示与离线评测用的知识库语料。
//
// 内容与服务字典的主题对齐（接口/数据库/账号/部署），使「检索命中 → 按归属派单」
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
