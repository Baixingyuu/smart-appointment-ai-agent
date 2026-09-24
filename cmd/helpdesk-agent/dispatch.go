package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/mac/helpdesk-agent/internal/assign"
	"github.com/mac/helpdesk-agent/internal/domain"
	"github.com/mac/helpdesk-agent/internal/llm"
	"github.com/mac/helpdesk-agent/internal/seed"
	"github.com/mac/helpdesk-agent/internal/store"
	"github.com/mac/helpdesk-agent/internal/ticket"
)

// newTicketService 按环境开关装配派单后端，并在启动时打印当前模式。
//
// 为什么不直接切默认后端：三段流水线一期只做过有限样本验证，
// 默认仍走已跑通全量评测的 legacy Assigner；pipeline 通过 DISPATCH_PIPELINE=on 显式启用。
// 每次启动都把模式打到 stderr —— 派单结果差异背后是这个开关，
// 不打印就会出现"以为在跑 pipeline 其实没跑"这种无法回溯的误判。
//
// chatModel 可为 nil（离线 / demo）：nil 时 Stage 2 不注入 chooser，
// 判弱样本直落人工池，而不是静默退回 Stage 1 强派。
func newTicketService(st store.Store, chatModel llm.ChatModel) *ticket.Service {
	if !pipelineEnabled() {
		fmt.Fprintln(os.Stderr, "派单后端：legacy Assigner（技能 Jaccard 打分）。设 DISPATCH_PIPELINE=on 切换到三段流水线。")
		return ticket.New(st, assign.New(assign.DefaultWeights()))
	}

	var chooser assign.LLMChooser
	if chatModel != nil {
		chooser = assign.NewOpenAIChooser(chatModel, 5)
	}
	pipeline := assign.NewPipeline(
		assign.NewBM25ServiceResolver(3),
		assign.NoopSimilarIndex{},
		chooser,
	)
	stage2 := "关闭（判弱直落人工）"
	if chooser != nil {
		stage2 = "启用（LLM enum 约束）"
	}
	fmt.Fprintf(os.Stderr, "派单后端：三段流水线。Stage 2 %s。\n", stage2)

	return ticket.NewWith(st, ticket.NewPipelineDispatcher(pipeline),
		ticket.WithDirectoryProvider(seedDirectoryProvider()))
}

// pipelineEnabled 读取 DISPATCH_PIPELINE。仅 "on"/"1"/"true" 视为启用（大小写不敏感）。
func pipelineEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DISPATCH_PIPELINE"))) {
	case "on", "1", "true", "yes":
		return true
	default:
		return false
	}
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
