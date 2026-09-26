// 确认判定的 LLM 主判组件（对话侧的 "Stage 2"）。
//
// 背景：确认阶梯的关键词匹配（domain.ParseConfirmationDecision）是事故清单式
// 的经验沉淀——每条词条都来自一次误判的补丁，覆盖不了没踩过的坑，且天然缺上下文
// （用户这句话是不是在对草案表态，词表判断不了）。2026-09-25 起生产路径改为
// LLM 主判：模型带草案与近期对话上下文做枚举判定，词表降级为失败回退。
//
// 不变式不变：只有 confirm / force_commit 能触达建单（domain.GrantsApproval），
// 无论判定来自哪条路径。非法输出 / 调用失败一律回退词表，绝不静默放行。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mac/helpdesk-agent/internal/domain"
	"github.com/mac/helpdesk-agent/internal/llm"
)

// ConfirmationVerdict 一次确认判定的结论。
type ConfirmationVerdict struct {
	Decision  domain.ConfirmationDecision
	Rationale string
	Usage     llm.Usage
}

// ConfirmationJudge 判定用户消息对一份待确认草案的表态。
//
// draft 是中断里展示给用户的确认文案（含工单草案内容）；recentUserMessages
// 是本会话此前的用户原话（不含当前消息），供模型判断"这句话是不是在回应草案"。
// 实现必须自行做枚举白名单校验，非法输出返回 error 而不是猜测。
type ConfirmationJudge interface {
	Judge(ctx context.Context, draft string, userMessage string, recentUserMessages []string) (ConfirmationVerdict, error)
}

// judgeConfirmationToolName 注册的函数名。保持短，省 prompt 空间。
const judgeConfirmationToolName = "judge_confirmation"

// LLMConfirmationJudge 用 function calling + enum 契约调模型（与 assign.OpenAIChooser 同一范式）。
type LLMConfirmationJudge struct {
	Model llm.ChatModel
}

// confirmationEnum 是判定输出的全集。unknown 必须在 enum 内：
// 模型拿不准时输出 unknown 由编排层带上下文澄清，而不是逼它在 confirm/cancel 里二选一。
var confirmationEnum = []string{"confirm", "cancel", "force_commit", "has_new_demand", "unknown"}

// Judge 执行一次判定。
//
// 语义：未发起工具调用、参数非法、decision 不在 enum 内，都返回 error；
// 调用方（Agent.judgeConfirmation）据此回退关键词阶梯。
func (j *LLMConfirmationJudge) Judge(ctx context.Context, draft, userMessage string, recentUserMessages []string) (ConfirmationVerdict, error) {
	if j.Model == nil {
		return ConfirmationVerdict{}, errors.New("LLMConfirmationJudge: Model 未注入")
	}
	tools := []llm.ToolSchema{{
		Name:        judgeConfirmationToolName,
		Description: "判定用户消息对一份待确认的工单草案是什么表态",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"decision": map[string]any{
					"type":        "string",
					"enum":        confirmationEnum,
					"description": "confirm=同意按草案建单；cancel=取消；force_commit=明确要求停止追问直接建；has_new_demand=提出新诉求而非答复草案；unknown=语义不明",
				},
				"rationale": map[string]any{
					"type":        "string",
					"description": "一句话依据，引用用户原话里的关键表述",
				},
			},
			"required": []string{"decision", "rationale"},
		},
	}}

	var b strings.Builder
	b.WriteString("【待确认的工单草案】\n")
	b.WriteString(draft)
	b.WriteString("\n\n【用户最新消息】\n")
	b.WriteString(userMessage)
	if len(recentUserMessages) > 0 {
		b.WriteString("\n\n【此前用户说过的话】（判断新消息是否在回应草案时参考）\n")
		for i, msg := range recentUserMessages {
			fmt.Fprintf(&b, "%d. %s\n", i+1, msg)
		}
	}
	b.WriteString("\n请调用 judge_confirmation 工具。")

	resp, err := j.Model.ChatWithTools(ctx, llm.ToolRequest{
		System:   judgeConfirmationSystemPrompt,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: b.String()}},
		Tools:    tools,
	})
	if err != nil {
		return ConfirmationVerdict{}, err
	}
	if len(resp.ToolCalls) == 0 {
		return ConfirmationVerdict{}, errors.New("LLMConfirmationJudge: 模型未发起工具调用")
	}
	call := resp.ToolCalls[0]
	if call.Name != judgeConfirmationToolName {
		return ConfirmationVerdict{}, fmt.Errorf("LLMConfirmationJudge: 非预期的工具名 %q", call.Name)
	}
	var payload struct {
		Decision  string `json:"decision"`
		Rationale string `json:"rationale"`
	}
	if err := json.Unmarshal([]byte(call.Arguments), &payload); err != nil {
		return ConfirmationVerdict{}, fmt.Errorf("LLMConfirmationJudge: tool arguments 非法 JSON: %w", err)
	}
	// 二次白名单校验：不同 OpenAI-compatible 实现对 enum 的强制程度不一（同 chooser 的双保险理由）。
	if !confirmationEnumContains(strings.ToLower(strings.TrimSpace(payload.Decision))) {
		return ConfirmationVerdict{}, fmt.Errorf("LLMConfirmationJudge: decision %q 不在枚举内", payload.Decision)
	}
	return ConfirmationVerdict{
		Decision:  domain.ConfirmationDecision(payload.Decision),
		Rationale: payload.Rationale,
		Usage:     resp.Usage,
	}, nil
}

// confirmationEnumContains 判定值是否在确认枚举内（containsString 已在 tools.go 定义，
// 这里收一个专用小函数保持本文件自洽）。
func confirmationEnumContains(value string) bool {
	for _, item := range confirmationEnum {
		if item == value {
			return true
		}
	}
	return false
}

const judgeConfirmationSystemPrompt = `你是客服确认判定器。系统刚向用户展示了一份待确认的工单草案，用户回复了一条消息。
判定它属于哪类表态：
- confirm：明确同意按这份草案建单（如"确认""是的，就这样"）；
- cancel：明确取消（如"算了""不用了""不要建了"）；
- force_commit：明确表达"别再问了，直接建单"（如"直接建""别问了"）；
- has_new_demand：这条消息在提出新的诉求或补充另一件事，而不是对草案的答复（如"还是没弄好，帮我再提一个"）；
- unknown：语义不明、犹豫、或与草案无关但也不构成新诉求（如"我再想想"）。
硬约束：
1. decision 必须是 enum 中的一个字符串，禁止编造。
2. 判定拿不准时输出 unknown，不要猜。
输出仅一次工具调用，不发散解释。`

// WithConfirmationJudge 注入 LLM 确认判定器，启用 LLM 主判。
//
// 未注入时行为与既有版本完全一致（关键词阶梯判定），既有评测与测试零改动。
func WithConfirmationJudge(judge ConfirmationJudge) Option {
	return func(a *Agent) { a.confirmationJudge = judge }
}

// judgeConfirmation 判定用户对 pending 草案的表态。
//
// LLM 主判；未注入 judge 或调用失败时回退关键词阶梯（domain.ParseConfirmationDecision）。
// 回退不是降级风险：词表是确定性兜底，且其单测（含"yesterday 不算确认"等陷阱）
// 继续锁定回退路径的行为。判定失败最坏结果是"重问一次"，不是误建单。
func (a *Agent) judgeConfirmation(ctx context.Context, pending domain.Interrupt, input TurnInput) (domain.ConfirmationDecision, llm.Usage) {
	if a.confirmationJudge == nil {
		return domain.ParseConfirmationDecision(input.UserMessage), llm.Usage{}
	}
	verdict, err := a.confirmationJudge.Judge(ctx, pending.Prompt, input.UserMessage,
		a.recentCustomerMessages(input.ConversationID, 4))
	if err != nil || verdict.Decision == "" {
		return domain.ParseConfirmationDecision(input.UserMessage), llm.Usage{}
	}
	return verdict.Decision, verdict.Usage
}

// recentCustomerMessages 取本会话最近 n 条用户原话（不含当前正在处理的消息，
// 因为它由调用方单独传入），按时间升序。判定"是否在回应草案"的参考语料。
func (a *Agent) recentCustomerMessages(conversationID int64, n int) []string {
	if a.store == nil || conversationID <= 0 || n <= 0 {
		return nil
	}
	messages := a.store.MessagesByConversation(conversationID)
	corpus := make([]string, 0, len(messages))
	for _, msg := range messages {
		if msg.Sender != domain.SenderCustomer {
			continue
		}
		if text := strings.TrimSpace(msg.Content); text != "" {
			corpus = append(corpus, text)
		}
	}
	if len(corpus) > n {
		corpus = corpus[len(corpus)-n:]
	}
	return corpus
}
