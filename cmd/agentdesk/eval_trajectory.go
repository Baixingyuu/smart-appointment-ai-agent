package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mac/agentdesk/internal/agent"
	"github.com/mac/agentdesk/internal/assign"
	"github.com/mac/agentdesk/internal/eval"
	"github.com/mac/agentdesk/internal/llm"
	"github.com/mac/agentdesk/internal/rag"
	"github.com/mac/agentdesk/internal/seed"
	"github.com/mac/agentdesk/internal/store"
	"github.com/mac/agentdesk/internal/ticket"
)

// runEvalTrajectory 跑轨迹评测。
//
// 执行方式：先为每条用例独立跑一遍真实 Agent（模型为脚本化假模型），
// 采集逐轮观测；再由评测框架对这些观测做断言与汇总。
//
// 这样拆分的用意：观测来自真实编排链路（工具治理、确认中断、建单、派单
// 都真实发生），而断言逻辑与具体实现解耦，可独立测试。
//
// 重要局限：脚本化模型是「照着期望演」的，因此本命令的指标反映的是
// 「评测链路与断言是否正确」，不是「真实模型的工具选择能力」。
func runEvalTrajectory(args []string) error {
	fs := flag.NewFlagSet("eval-trajectory", flag.ContinueOnError)
	datasetPath := fs.String("dataset", defaultTrajectoryDatasetPath(), "轨迹评测集路径")
	jsonPath := fs.String("json", "", "机读报告输出路径")
	sabotage := fs.String("sabotage", "", "人为注入缺陷以验证评测区分力："+
		"skip_rag（跳过检索）/ always_write（总是建单）/ extra_rounds（多余步骤）/ unknown_tool（调用不存在的工具）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	dataset, err := eval.LoadTrajectoryDataset(*datasetPath)
	if err != nil {
		return err
	}

	runner, err := buildReplayRunner(dataset, *sabotage)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "注意：本命令由脚本化假模型驱动，指标反映的是「评测链路与断言是否正确」，\n")
	fmt.Fprintf(os.Stderr, "而不是真实模型的工具选择能力。接入真实模型后同一套评测即可产出对比数据。\n")
	if *sabotage != "" {
		fmt.Fprintf(os.Stderr, "已注入缺陷：%s\n", *sabotage)
	}
	fmt.Fprintln(os.Stderr)

	report := eval.Run(dataset, runner)
	fmt.Print(report.Text())

	if *jsonPath != "" {
		if err := writeJSON(*jsonPath, report); err != nil {
			return err
		}
		fmt.Printf("\n机读报告已写入 %s\n", *jsonPath)
	}
	return nil
}

// replayRunner 回放已采集的逐轮观测。
type replayRunner struct {
	observations map[int64][]eval.TurnObservation
	position     map[int64]int
}

// RunTurn 实现 eval.Runner。
func (r *replayRunner) RunTurn(conversationID int64, _ string) (eval.TurnObservation, error) {
	sequence := r.observations[conversationID]
	index := r.position[conversationID]
	if index >= len(sequence) {
		return eval.TurnObservation{}, fmt.Errorf(
			"会话 %d 的观测已回放完毕（共 %d 轮），评测集声明的消息数多于实际执行轮次",
			conversationID, len(sequence))
	}
	r.position[conversationID] = index + 1
	return sequence[index], nil
}

// buildReplayRunner 为每条用例真实跑一遍 Agent，采集观测。
func buildReplayRunner(dataset *eval.TrajectoryDataset, sabotage string) (eval.Runner, error) {
	runner := &replayRunner{
		observations: make(map[int64][]eval.TurnObservation, len(dataset.Cases)),
		position:     make(map[int64]int, len(dataset.Cases)),
	}
	for _, item := range dataset.Cases {
		observations, err := executeCase(item, sabotage)
		if err != nil {
			return nil, fmt.Errorf("用例 %s 执行失败: %w", item.ID, err)
		}
		runner.observations[item.ConversationID] = observations
	}
	return runner, nil
}

// executeCase 为单条用例建立独立环境并顺序执行全部消息。
//
// 每条用例使用全新的存储与脚本：用例之间不得共享状态，
// 否则前一用例建出的工单会改变后一用例的查重结果。
func executeCase(item eval.TrajectoryCase, sabotage string) ([]eval.TurnObservation, error) {
	st := store.NewMemory()
	if err := seed.Load(st); err != nil {
		return nil, err
	}
	tickets := ticket.New(st, assign.New(assign.DefaultWeights()))
	retriever := seed.NewBM25Retriever(rag.DefaultOptions())

	// 不注入固定时钟：延迟是评测的四个轴之一，
	// 固定时钟会让 DurationMS 恒为 0，指标直接失真。
	ag, err := agent.New(&scriptedModel{queue: caseScript(item, sabotage)}, st, retriever, tickets, agent.DefaultConfig())
	if err != nil {
		return nil, err
	}

	observations := make([]eval.TurnObservation, 0, len(item.Messages))
	for _, message := range item.Messages {
		startedAt := time.Now()
		result, err := ag.Run(context.Background(), agent.TurnInput{
			ConversationID: item.ConversationID,
			UserMessage:    message,
		})
		if err != nil {
			return nil, err
		}
		observations = append(observations, toObservation(result, time.Since(startedAt)))
	}
	return observations, nil
}

// toObservation 把 Agent 结果转换成评测观测。
func toObservation(result *agent.TurnResult, elapsed time.Duration) eval.TurnObservation {
	tools := make([]eval.ToolInvocation, 0, len(result.ToolCalls))
	for _, call := range result.ToolCalls {
		tools = append(tools, eval.ToolInvocation{
			Code:      call.Code,
			Status:    call.Status,
			ErrorKind: string(call.ErrorKind),
		})
	}
	return eval.TurnObservation{
		Reply:       result.Reply,
		Interrupted: result.Interrupted,
		TicketID:    result.TicketID,
		Tools:       tools,
		Rounds:      result.Rounds,
		Usage: eval.TokenUsage{
			PromptTokens:     result.Usage.PromptTokens,
			CompletionTokens: result.Usage.CompletionTokens,
		},
		// 用外层实测耗时：Agent 返回的耗时基于注入时钟，
		// 而真实端到端耗时应由调用方度量，二者不应混用。
		DurationMS: int(elapsed.Milliseconds()),
		DurationUS: int(elapsed.Microseconds()),
	}
}

// scriptedModel 按队列回放响应。
//
// 队列耗尽后重复最后一个响应，使脚本长度不必与真实轮次严格相等
// （确认恢复那一轮不调用模型，轮次难以预先精确推算）。
type scriptedModel struct {
	queue []*llm.Response
	index int
}

func (m *scriptedModel) Chat(_ context.Context, _, _ string) (*llm.Response, error) {
	return &llm.Response{
		Content: "纯文本回复",
		Usage:   llm.Usage{PromptTokens: 50, CompletionTokens: 10},
	}, nil
}

func (m *scriptedModel) ChatWithTools(_ context.Context, _ llm.ToolRequest) (*llm.Response, error) {
	if len(m.queue) == 0 {
		return nil, fmt.Errorf("脚本未配置任何响应")
	}
	index := m.index
	m.index++
	if index >= len(m.queue) {
		index = len(m.queue) - 1
	}
	return m.queue[index], nil
}

func callResponse(id, name, args string) *llm.Response {
	return &llm.Response{
		ToolCalls: []llm.ToolCall{{ID: id, Name: name, Arguments: args}},
		Usage:     llm.Usage{PromptTokens: 900, CompletionTokens: 80},
	}
}

func replyResponse(text string) *llm.Response {
	return &llm.Response{
		Content: text,
		Usage:   llm.Usage{PromptTokens: 300, CompletionTokens: 120},
	}
}

// caseScript 依据用例声明的期望，构造一个「行为基本正确」的脚本。
//
// 这是脚本化模型的固有局限：它在照着期望演，因此只能用于验证
// 评测链路是否正确，不能用于评估模型能力。若要评估模型能力，
// 把 scriptedModel 换成 llm.OpenAIModel 即可，其余代码无需改动。
func caseScript(item eval.TrajectoryCase, sabotage string) []*llm.Response {
	title := safeTitle(item)
	issue := "测试用例构造的问题描述，用于验证工具链路"
	draftArgs := fmt.Sprintf(
		`{"title":%q,"description":%q,"category":"incident","priority":"P2"}`, title, issue)
	confirmArgs := fmt.Sprintf(
		`{"title":%q,"description":%q,"category":"incident","priority":"P2","skillIds":[2,3]}`, title, issue)

	switch sabotage {
	case "unknown_tool":
		return []*llm.Response{
			callResponse("s1", "make_up_a_tool", `{}`),
			replyResponse("抱歉，我暂时无法处理。"),
		}
	case "always_write":
		// 无视是否该建单一律建单：应被安全越界率抓到。
		return []*llm.Response{
			callResponse("s1", toolConfirm, confirmArgs),
			replyResponse("已建单。"),
		}
	case "skip_rag":
		return []*llm.Response{replyResponse("我直接回答，不检索。")}
	case "extra_rounds":
		return []*llm.Response{
			callResponse("s1", toolRAGSearch, fmt.Sprintf(`{"query":%q}`, title)),
			callResponse("s2", toolRAGSearch, `{"query":"额外检索一"}`),
			callResponse("s3", toolRAGSearch, `{"query":"额外检索二"}`),
			callResponse("s4", toolRAGSearch, `{"query":"额外检索三"}`),
			callResponse("s5", toolRAGSearch, `{"query":"额外检索四"}`),
			replyResponse("检索了很多次。"),
		}
	}

	// 正常脚本：按期望的工具顺序构造调用。
	var queue []*llm.Response
	step := 0
	appendCall := func(name, args string) {
		step++
		queue = append(queue, callResponse(fmt.Sprintf("s%d", step), name, args))
	}

	// 期望里声明的顺序即脚本顺序；未声明时按「检索→查重→起草→建单」的合理顺序。
	ordered := item.Expect.OrderedTools
	if len(ordered) == 0 {
		ordered = item.Expect.Tools
	}
	if len(ordered) == 0 && item.Expect.ExpectTicketCreated != nil && *item.Expect.ExpectTicketCreated {
		ordered = []string{toolRAGSearch, toolFindOpen, toolDraft, toolConfirm}
	}

	for _, code := range ordered {
		switch code {
		case toolRAGSearch:
			appendCall(code, fmt.Sprintf(`{"query":%q}`, title))
		case toolFindOpen:
			appendCall(code, fmt.Sprintf(`{"topic":%q}`, title))
		case toolDraft:
			appendCall(code, draftArgs)
		case toolConfirm:
			appendCall(code, confirmArgs)
		}
	}

	// 收尾回复：确认恢复那一轮不调用模型，队列耗尽后会重复最后一条，不会越界。
	queue = append(queue, replyResponse("已按你的要求处理完成。"))
	return queue
}

// safeTitle 由用例首条消息生成简短标题。
func safeTitle(item eval.TrajectoryCase) string {
	if len(item.Messages) == 0 {
		return "测试问题"
	}
	text := strings.TrimSpace(item.Messages[0])
	// 去掉可能破坏 JSON 字面量的字符。
	text = strings.NewReplacer(`"`, "", `\`, "", "\n", " ").Replace(text)
	if runes := []rune(text); len(runes) > 20 {
		text = string(runes[:20])
	}
	if text == "" {
		return "测试问题"
	}
	return text
}

// 工具编码常量。
const (
	toolRAGSearch = "rag_search"
	toolFindOpen  = "ticket_find_open_by_topic"
	toolDraft     = "ticket_create_draft"
	toolConfirm   = "ticket_create_confirm"
)

// defaultTrajectoryDatasetPath 定位轨迹评测集。
func defaultTrajectoryDatasetPath() string {
	candidates := []string{
		"eval/datasets/trajectory.json",
		"../eval/datasets/trajectory.json",
		"../../eval/datasets/trajectory.json",
	}
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return filepath.Join("eval", "datasets", "trajectory.json")
}
