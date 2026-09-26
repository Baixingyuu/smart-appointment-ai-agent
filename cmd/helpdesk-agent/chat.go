package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mac/helpdesk-agent/internal/agent"
	"github.com/mac/helpdesk-agent/internal/classify"
	"github.com/mac/helpdesk-agent/internal/conversation"
	"github.com/mac/helpdesk-agent/internal/evalrun"
	"github.com/mac/helpdesk-agent/internal/rag"
	"github.com/mac/helpdesk-agent/internal/seed"
	"github.com/mac/helpdesk-agent/internal/store"
)

// turnSample 一条人工测试样本。
//
// 只存回复文本无法定位缺陷，因此每回合连同归因字段一起落盘：
// 意图、是否短路、逐轮 token、每个工具调用的状态与拒绝原因、是否发起确认、
// 会话是否仍待确认、本轮的确认判定、是否建单。
// 事后分析靠这些字段做归因，不靠回忆。
type turnSample struct {
	At             string `json:"at"`
	ConversationID int64  `json:"conversationId"`
	Turn           int    `json:"turn"`
	UserMessage    string `json:"userMessage"`
	Reply          string `json:"reply"`
	Intent         string `json:"intent,omitempty"`
	ShortCircuit   bool   `json:"shortCircuit,omitempty"`
	Interrupted    bool   `json:"interrupted,omitempty"`
	// AwaitingConfirmation 与 Interrupted 必须分开落盘：前者是「会话仍有草案待确认」，
	// 后者是「本轮新起了一个确认」。复核「待确认期间消息被吞」这条缺陷靠的是前者。
	AwaitingConfirmation bool                `json:"awaitingConfirmation,omitempty"`
	Decision             string              `json:"confirmationDecision,omitempty"`
	TicketID             int64               `json:"ticketId,omitempty"`
	Rounds               int                 `json:"rounds"`
	PromptTokens         int                 `json:"promptTokens"`
	TokenTotal           int                 `json:"tokenTotal"`
	DurationMS           int64               `json:"durationMs"`
	Tools                []string            `json:"tools,omitempty"`
	Rejections           []string            `json:"rejections,omitempty"`
	RagSufficient        bool                `json:"ragSufficient,omitempty"`
	RagReason            string              `json:"ragReason,omitempty"`
	RagRounds            int                 `json:"ragRounds,omitempty"`
	RoundUsage           []agent.RoundRecord `json:"roundUsage,omitempty"`
	Error                string              `json:"error,omitempty"`
}

type sessionMeta struct {
	Kind      string `json:"kind"`
	ModelMode string `json:"modelMode"`
	Model     string `json:"model,omitempty"`
	Started   string `json:"started"`
}

// runChat 提供人工测试用的命令行对话窗口，并把每回合轨迹落成可复核样本。
//
// 与 serve 走同一条装配路径（同一套提示词、路由分类器、检索器），
// 目的是让人工发现的缺陷能直接落到评测口径上，而不是「演示环境里才会出现的问题」。
//
// 边界：-offline 下脚本模型不理解输入，归因字段（意图/轮次/token）没有解释力，
// 该模式只用于验证窗口与样本落盘链路。
func runChat(args []string) error {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	offline := fs.Bool("offline", false, "使用离线脚本模型（无需 API Key）")
	baseURL := fs.String("base-url", envOr("LLM_BASE_URL", "https://api.deepseek.com/v1"), "模型服务地址（OpenAI-compatible）")
	apiKey := fs.String("api-key", envOr("LLM_API_KEY", ""), "模型 API Key（或用 LLM_API_KEY）")
	modelName := fs.String("model", envOr("LLM_MODEL", "deepseek-flash"), "模型名称")
	logPath := fs.String("log", "", "样本 JSONL 路径（默认 eval/samples/chat-<时间>.jsonl）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	chatModel, embedder, mode, err := buildModel(*offline, llmConfig{
		baseURL: *baseURL, apiKey: *apiKey, model: *modelName,
	})
	if err != nil {
		return err
	}

	st := store.NewMemory()
	if err := seed.Load(st); err != nil {
		return err
	}
	tickets := newTicketService(st, chatModel)

	retriever := seed.NewBM25Retriever(rag.DefaultOptions())
	if embedder != nil {
		retriever = rag.New(seed.KnowledgeChunks(), embedder, rag.DefaultOptions())
	}
	// 与 serve 同一条装配路径：WithIntake（两级可追溯判定 + 追问门控）与
	// LLM 确认判定（词表降为回退）都必须在，否则人工发现的缺陷落不到生产口径上。
	// WithEventObserver 把「第几轮 / 调了哪个工具 / 结果如何」实时打到终端，
	// 减少对 agent 内部流程的未知性（模型调用是回合内最耗时的黑洞，round 事件先亮起）。
	ag, err := agent.New(chatModel, st, retriever, tickets, agent.DefaultConfig(),
		agent.WithClassifier(evalrun.NewClassifierRunner(classify.New(chatModel))),
		agent.WithIntake(agent.IntakeConfig{
			Checker: agent.NewTwoLevelTraceabilityChecker(chatModel, agent.DefaultTraceabilityConfig()),
			Gating:  true,
		}),
		agent.WithConfirmationJudge(&agent.LLMConfirmationJudge{Model: chatModel}),
		agent.WithEventObserver(printTurnEvent),
	)
	if err != nil {
		return err
	}

	var lastTurn *agent.TurnResult
	// streamed 缓存本轮已流式输出的回复文本。printReply 用它判断是否已被逐字打过，
	// 避免「流式打一遍、结束再打一遍」的双份回复。每回合开始前由主循环清空。
	var streamed strings.Builder
	executor := conversation.TurnExecutorFunc(func(ctx context.Context, conversationID int64, message string) (conversation.TurnOutcome, error) {
		result, err := ag.Run(ctx, agent.TurnInput{
			ConversationID: conversationID,
			UserMessage:    message,
			Stream: func(delta string) {
				// 首段先亮「助手 >」前缀，再逐字外发正文。
				if streamed.Len() == 0 {
					fmt.Print("助手 > ")
				}
				fmt.Print(delta)
				streamed.WriteString(delta)
			},
		})
		if err != nil {
			return conversation.TurnOutcome{}, err
		}
		lastTurn = result
		return conversation.TurnOutcome{
			Reply:       result.Reply,
			Interrupted: result.Interrupted,
			TicketID:    result.TicketID,
		}, nil
	})
	conversations := conversation.New(st, executor)

	samples, err := openSampleLog(*logPath, mode, *modelName, *offline)
	if err != nil {
		return err
	}
	defer samples.Close()
	encoder := json.NewEncoder(samples)

	fmt.Printf("helpdesk-agent 人工对话窗口\n")
	fmt.Printf("  模型模式  %s\n", mode)
	if !*offline {
		fmt.Printf("  服务地址  %s\n", *baseURL)
		fmt.Printf("  模型      %s\n", *modelName)
	}
	fmt.Printf("  样本      %s\n", samples.Name())
	fmt.Printf("  指令      /new 新会话  /trace 上回合归因  /tickets 工单  /help 说明  /quit 退出\n")
	if !*offline {
		fmt.Printf("  提示      本地模型用 make chat-local（qwen3:8b；勿用 8k/16k 变体，会撑爆显存）\n")
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	conversationID, err := startConversation(conversations)
	if err != nil {
		return err
	}
	turn := 0

	for {
		fmt.Printf("\n你 > ")
		if !scanner.Scan() {
			fmt.Println()
			return scanner.Err()
		}
		line := strings.TrimSpace(scanner.Text())
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "/"):
			quit, cmdErr := handleCommand(line, conversations, st, &conversationID, &turn, lastTurn)
			if cmdErr != nil {
				fmt.Printf("  ! %v\n", cmdErr)
			}
			if quit {
				return nil
			}
			continue
		}

		turn++
		startedAt := time.Now()
		streamed.Reset()
		result, err := conversations.Send(context.Background(), conversationID, line, fmt.Sprintf("chat-%d-%d", conversationID, turn))
		sample := turnSample{
			At:             startedAt.Format(time.RFC3339),
			ConversationID: conversationID,
			Turn:           turn,
			UserMessage:    line,
			DurationMS:     time.Since(startedAt).Milliseconds(),
		}
		if err != nil {
			sample.Error = err.Error()
			fmt.Printf("助手 > （本轮失败）%v\n", err)
		} else if result.Turn != nil {
			sample.Reply = result.Turn.Reply
			if lastTurn != nil {
				fillFromTurn(&sample, lastTurn)
			}
			sample.Interrupted = result.Turn.Interrupted
			sample.TicketID = result.Turn.TicketID
			printReply(&sample, streamed.String())
		}
		if err := encoder.Encode(sample); err != nil {
			fmt.Printf("  ! 样本写入失败: %v\n", err)
		}
	}
}

// printTurnEvent 是回合事件的实时外显：直接写终端，不落样本。
//
// 与样本 JSONL 分工：这里给人看过程，样本留作事后归因；两者不重复记账。
// 状态文案沿用 tooling 的 Status 语义（completed / awaiting_user_info /
// awaiting_confirmation / failed...），把"为什么没建单"讲成人话。
func printTurnEvent(e agent.TurnEvent) {
	switch e.Kind {
	case "round":
		fmt.Printf("  ⟳ 第 %d 轮：%s…\n", e.Round, e.Detail)
	case "tool":
		if e.Detail != "" {
			fmt.Printf("     · %s → %s：%s\n", e.Tool, toolStatusText(e.Status), truncateRunes(e.Detail, 80))
		} else {
			fmt.Printf("     · %s → %s\n", e.Tool, toolStatusText(e.Status))
		}
	case "confirm":
		fmt.Printf("     · 确认判定：%s\n", e.Detail)
	}
}

// truncateRunes 按 rune 截断文本，超出补省略号；用于工具事件原因等短展示场景。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// toolStatusText 把工具状态码翻译成可读短语，未知状态原样返回避免吞信息。
func toolStatusText(status string) string {
	switch status {
	case "completed":
		return "完成"
	case "awaiting_user_info":
		return "信息不足，先追问"
	case "awaiting_confirmation":
		return "发起确认，等待回复"
	case "failed":
		return "执行失败"
	default:
		return status
	}
}

func fillFromTurn(sample *turnSample, result *agent.TurnResult) {
	sample.Intent = string(result.Intent)
	sample.ShortCircuit = result.ShortCircuited
	sample.AwaitingConfirmation = result.AwaitingConfirmation
	sample.Decision = string(result.Decision)
	sample.Rounds = result.Rounds
	sample.PromptTokens = result.Usage.PromptTokens
	sample.TokenTotal = result.Usage.Total()
	sample.RoundUsage = result.RoundRecords
	if result.RAGResult != nil {
		sample.RagSufficient = result.RAGResult.Gate.Sufficient
		sample.RagReason = string(result.RAGResult.Gate.Reason)
		sample.RagRounds = result.RAGResult.Rounds
	}
	for _, call := range result.ToolCalls {
		sample.Tools = append(sample.Tools, call.Code)
		if call.Status != "completed" {
			// 以 Status 为准而非 ErrorKind：awaiting_user_info（intake 追问）
			// 这类"未执行但非错误"的状态没有 ErrorKind，旧写法会打出空尾巴。
			detail := call.Status
			if call.ErrorKind != "" {
				detail += "/" + string(call.ErrorKind)
			}
			sample.Rejections = append(sample.Rejections, fmt.Sprintf("%s:%s", call.Code, detail))
		}
	}
}

func printReply(sample *turnSample, streamedText string) {
	// 已流式打过的回复不再重打，只补一个换行收尾；否则按原逻辑整段打印。
	// 判据用"流式文本 == 最终回复"而非一个布尔：模型在调用工具前可能先吐一句前言，
	// 那部分虽已流式外发，但正式回复（如确认话术/中断提示）仍需整段打印。
	fullyStreamed := sample.Reply != "" && strings.TrimSpace(streamedText) == strings.TrimSpace(sample.Reply)
	if fullyStreamed {
		fmt.Println()
	} else {
		if strings.TrimSpace(streamedText) != "" {
			fmt.Println() // 有前言流式输出时，先换行再打印正式回复，避免挤在同一行
		}
		prefix := "助手 > "
		switch {
		case sample.AwaitingConfirmation && sample.Interrupted:
			prefix = "助手 > [本轮发起确认，等待回复] "
		case sample.AwaitingConfirmation:
			prefix = "助手 > [仍在等待你的确认] "
		}
		fmt.Printf("%s%s\n", prefix, sample.Reply)
	}
	if sample.TicketID > 0 {
		fmt.Printf("       工单 #%d 已创建\n", sample.TicketID)
	}
	attribution := fmt.Sprintf("       归因: 意图=%s 轮次=%d 工具=%s token=%d 耗时=%dms",
		orNone(sample.Intent), sample.Rounds, orNone(strings.Join(sample.Tools, ",")),
		sample.TokenTotal, sample.DurationMS)
	if sample.Decision != "" {
		attribution += fmt.Sprintf(" 确认判定=%s", sample.Decision)
	}
	fmt.Println(attribution)
	if len(sample.Rejections) > 0 {
		fmt.Printf("       被拒调用: %s\n", strings.Join(sample.Rejections, " "))
	}
}

func handleCommand(line string, conversations *conversation.Service, st store.Store,
	conversationID *int64, turn *int, lastTurn *agent.TurnResult) (bool, error) {
	switch strings.Fields(line)[0] {
	case "/quit", "/exit", "/q":
		return true, nil
	case "/help":
		fmt.Print(chatHelpText)
	case "/new":
		id, err := startConversation(conversations)
		if err != nil {
			return false, err
		}
		*conversationID, *turn = id, 0
		fmt.Printf("  已开新会话 #%d\n", id)
	case "/id":
		fmt.Printf("  当前会话 #%d\n", *conversationID)
	case "/tickets":
		listTickets(st)
	case "/trace":
		if lastTurn == nil {
			fmt.Println("  还没有已完成的回合")
			return false, nil
		}
		printTrace(lastTurn)
	default:
		return false, fmt.Errorf("未知指令 %s，/help 查看可用指令", line)
	}
	return false, nil
}

func startConversation(conversations *conversation.Service) (int64, error) {
	created, err := conversations.Start(conversation.StartInput{SourceChannel: "manual-test"})
	if err != nil {
		return 0, err
	}
	fmt.Printf("  会话 #%d 已开始\n", created.ID)
	return created.ID, nil
}

func listTickets(st store.Store) {
	list := st.ListTickets()
	if len(list) == 0 {
		fmt.Println("  暂无工单")
		return
	}
	for _, t := range list {
		fmt.Printf("  #%d [%s] %s\n", t.ID, t.Status, t.Title)
	}
}

func printTrace(result *agent.TurnResult) {
	fmt.Printf("  意图=%s 短路=%v 轮次=%d token=%d/%d 耗时=%dms 确认判定=%s 待确认=%v\n",
		orNone(string(result.Intent)), result.ShortCircuited, result.Rounds,
		result.Usage.PromptTokens, result.Usage.CompletionTokens, result.DurationMS,
		orNone(string(result.Decision)), result.AwaitingConfirmation)
	for _, record := range result.RoundRecords {
		fmt.Printf("    第 %d 轮  prompt=%d completion=%d 工具=%s\n",
			record.Round, record.PromptTokens, record.CompletionTokens, orNone(strings.Join(record.Tools, ",")))
	}
	for _, call := range result.ToolCalls {
		fmt.Printf("    调用 %-28s %-9s %dms", call.Code, call.Status, call.DurationMS)
		if call.ErrorKind != "" {
			fmt.Printf("  %s", call.ErrorKind)
		}
		fmt.Println()
	}
	if result.RAGResult != nil {
		fmt.Printf("    检索 sufficient=%v reason=%s 命中=%d 轮次=%d\n",
			result.RAGResult.Gate.Sufficient, result.RAGResult.Gate.Reason,
			len(result.RAGResult.Hits), result.RAGResult.Rounds)
	}
}

func openSampleLog(path, mode, modelName string, offline bool) (*os.File, error) {
	if path == "" {
		if err := os.MkdirAll("eval/samples", 0o755); err != nil {
			return nil, err
		}
		path = fmt.Sprintf("eval/samples/chat-%s.jsonl", time.Now().Format("20060102-150405"))
	} else if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	file, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("创建样本文件: %w", err)
	}
	meta := sessionMeta{Kind: "session", ModelMode: mode, Started: time.Now().Format(time.RFC3339)}
	if !offline {
		meta.Model = modelName
	}
	if err := json.NewEncoder(file).Encode(meta); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func orNone(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

const chatHelpText = `  直接输入文本即为发送给客服助手的消息，可连续多轮。
  /new      开一个新会话（样本仍在同一个 JSONL 里，按 conversationId 区分）
  /trace    打印上一回合的逐轮 token、每个工具调用及其状态、检索判定
  /tickets  列出当前进程里已创建的工单
  /id       显示当前会话 ID
  /quit     退出
`
