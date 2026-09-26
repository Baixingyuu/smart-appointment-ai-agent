package main

import (
	"fmt"
	"os"

	"github.com/mac/helpdesk-agent/internal/assign"
	"github.com/mac/helpdesk-agent/internal/domain"
	"github.com/mac/helpdesk-agent/internal/llm"
	"github.com/mac/helpdesk-agent/internal/seed"
	"github.com/mac/helpdesk-agent/internal/store"
	"github.com/mac/helpdesk-agent/internal/ticket"
)

// newTicketService 装配派单服务，并把 Stage 2 是否启用打到 stderr。
//
// 打印模式不是日志装饰：Stage 2 有无 chooser 决定判弱样本是"让 LLM 在候选里选"
// 还是"直落人工池"，两种模式的派单结果差异很大。不打印就会出现
// "以为在跑 Stage 2 其实没跑"这种事后无法回溯的误判。
//
// chatModel 可为 nil（离线 / demo）：nil 时 Stage 2 不注入 chooser，
// 判弱样本直落人工池，而不是静默退回 Stage 1 强派。
func newTicketService(st store.Store, chatModel llm.ChatModel) *ticket.Service {
	var chooser assign.LLMChooser
	if chatModel != nil {
		chooser = assign.NewOpenAIChooser(chatModel, 5)
	}
	stage2 := "关闭（判弱直落人工）"
	if chooser != nil {
		stage2 = "启用（LLM enum 约束）"
	}
	fmt.Fprintf(os.Stderr, "派单：三段流水线。Stage 2 %s。\n", stage2)
	return newDispatchService(st, chooser)
}

// newDispatchService 装配三段流水线，不打印模式。
//
// 评测命令用它逐用例建服务：那类场景每个用例一份全新存储，
// 逐条打 banner 会把输出淹掉。Stage 1/3 是纯确定性的，
// 离线评测刻意不注入 Stage 2 chooser —— 派单里多一次 LLM 调用会污染
// 轮次、延迟与 token 三个轴；派单质量本身由 eval-assign-v2 那一轴单独测。
func newDispatchService(st store.Store, chooser assign.LLMChooser) *ticket.Service {
	pipeline := assign.NewPipeline(
		assign.NewBM25ServiceResolver(3),
		assign.NoopSimilarIndex{},
		chooser,
	)
	return ticket.New(st, pipeline, ticket.WithDirectoryProvider(seedDirectoryProvider()))
}

// seedDirectoryProvider 从 seed 包拼装一次派单所需的人员与服务目录。
//
// 一期服务字典与员工扩展硬编码在 seed（无导入通路），所以这里直接取内存快照。
// 接 store/CMDB 后只需换掉这个闭包的实现，Service 侧无感。
func seedDirectoryProvider() ticket.DirectoryProvider {
	return ticket.DirectoryProviderFunc(func(emps []domain.Employee) assign.Directory {
		return assign.Directory{
			Employees:  emps,
			Extensions: seed.ExtensionsByEmployeeID(),
			Services:   seed.ServicesByID(),
		}
	})
}
