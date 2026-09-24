// Stage 2：LLM chooser（慢通道，function-calling + enum 约束）。
//
// 契约核心：模型只能从 Stage 1 送来的 top-N 员工 ID 加 ESCALATE_HUMAN 中选。
// 幻觉 ID 不重试，直接走 Stage 3 —— 一次幻觉说明当前 prompt 对目标模型超出能力，
// 重试只会烧钱并掩盖问题；把幻觉率作为独立指标（PathStage2Invalid / Stage2 总数）暴露。
//
// 见 docs/DISPATCH_PIPELINE.md §2。
package assign

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mac/helpdesk-agent/internal/domain"
	"github.com/mac/helpdesk-agent/internal/llm"
)

// EscalateHumanID 是模型显式放弃决策时的 assignee_id 哨兵值。
const EscalateHumanID int64 = -1

// LLMChoice Stage 2 的输出。AssigneeID 为 EscalateHumanID 表示模型主动放弃。
type LLMChoice struct {
	AssigneeID int64
	Rationale  string
	Confidence float64
	Usage      llm.Usage
}

// Stage2Request 送给 Stage 2 的输入。
//
// DirectoryRef 只用于把 EmployeeExtension.Profile 拼进 prompt；
// 不作为决策依据 —— 避免 prompt 里出现"目录里所有员工"这种越界信息。
type Stage2Request struct {
	Ticket       domain.Ticket
	Matches      []ServiceMatch
	Candidates   []Candidate
	Weakness     WeaknessReason
	DirectoryRef Directory
}

// LLMChooser Stage 2 抽象。
//
// 独立接口而不是绑死 llm.ChatModel 的原因：
// ScriptedChooser 与 OpenAIChooser 需要的输入完全相同但输出协议不同
// （fixture 直接给 int64，真模型给 JSON tool arguments），
// 用同一 interface 让 pipeline 单测能精确控制"返回幻觉 ID"这类失败模式。
type LLMChooser interface {
	Choose(ctx context.Context, req Stage2Request) (LLMChoice, error)
}

// chooseAssigneeToolName 是注册的函数名。命名保持短，避免占 prompt 空间。
const chooseAssigneeToolName = "choose_assignee"

// OpenAIChooser 通过 function calling 契约调 llm.ChatModel。
//
// 温度依赖上层 llm.Config 设置（一期约定 temp=0 保证可复现），
// 本层不再重复控制 —— 与 llm 包 applySampling 的注释同一原则。
type OpenAIChooser struct {
	Model llm.ChatModel
	// MaxCandidates 送进 enum 的候选上限；超出后截断而不是报错。
	// 截断是因为模型上下文窗口对 prompt 长度更敏感，宁缺毋滥。
	MaxCandidates int
}

// NewOpenAIChooser 构造 chooser。maxCandidates <= 0 时回退为 5。
func NewOpenAIChooser(model llm.ChatModel, maxCandidates int) *OpenAIChooser {
	if maxCandidates <= 0 {
		maxCandidates = 5
	}
	return &OpenAIChooser{Model: model, MaxCandidates: maxCandidates}
}

// Choose 执行一次 Stage 2 决策。
//
// 语义：模型未发起工具调用、参数不是合法 JSON、或 assignee_id 不在 enum 内，
// 都返回 error。pipeline 会把 error 视为 Stage 3 兜底，不计入幻觉率
// —— 幻觉率的分母是"模型成功返回了结构化输出"这一子集。
func (c *OpenAIChooser) Choose(ctx context.Context, req Stage2Request) (LLMChoice, error) {
	if c.Model == nil {
		return LLMChoice{}, errors.New("OpenAIChooser: Model 未注入")
	}
	if len(req.Candidates) == 0 {
		return LLMChoice{}, errors.New("OpenAIChooser: 候选集为空")
	}
	candidates := req.Candidates
	if len(candidates) > c.MaxCandidates {
		candidates = candidates[:c.MaxCandidates]
	}
	enumValues := make([]string, 0, len(candidates)+1)
	for _, cand := range candidates {
		enumValues = append(enumValues, strconv.FormatInt(cand.EmployeeID, 10))
	}
	enumValues = append(enumValues, "ESCALATE_HUMAN")

	tools := []llm.ToolSchema{{
		Name:        chooseAssigneeToolName,
		Description: "从给定候选人中挑一位处理本工单；证据不足时选 ESCALATE_HUMAN。禁止编造 ID。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"assignee_id": map[string]any{
					"type":        "string",
					"enum":        enumValues,
					"description": "候选员工 ID 字符串（来自 enum）或 ESCALATE_HUMAN",
				},
				"rationale": map[string]any{
					"type":        "string",
					"description": "引用 profile / ownership / past_ticket 字段的具体证据；一句话",
				},
				"confidence": map[string]any{
					"type":        "number",
					"minimum":     0,
					"maximum":     1,
					"description": "自报置信度，仅用于观测与校准，不参与本次控制流",
				},
			},
			"required": []string{"assignee_id", "rationale", "confidence"},
		},
	}}

	messages := []llm.Message{{Role: llm.RoleUser, Content: buildStage2Prompt(req, candidates)}}
	resp, err := c.Model.ChatWithTools(ctx, llm.ToolRequest{
		System:   stage2SystemPrompt,
		Messages: messages,
		Tools:    tools,
	})
	if err != nil {
		return LLMChoice{}, err
	}
	if len(resp.ToolCalls) == 0 {
		return LLMChoice{}, errors.New("OpenAIChooser: 模型未发起工具调用")
	}
	call := resp.ToolCalls[0]
	var payload struct {
		AssigneeID string  `json:"assignee_id"`
		Rationale  string  `json:"rationale"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(call.Arguments), &payload); err != nil {
		return LLMChoice{}, fmt.Errorf("OpenAIChooser: tool arguments 非法 JSON: %w", err)
	}
	choice := LLMChoice{
		Rationale:  payload.Rationale,
		Confidence: clamp01(payload.Confidence),
	}
	if strings.EqualFold(payload.AssigneeID, "ESCALATE_HUMAN") {
		choice.AssigneeID = EscalateHumanID
		choice.Usage = resp.Usage
		return choice, nil
	}
	// 二次校验：即使服务端未强制 enum，本地也拒绝候选集外 ID。
	// 双保险不是冗余：不同 OpenAI-compatible 实现对 enum 的强制程度不一。
	allowed := false
	for _, cand := range candidates {
		if strconv.FormatInt(cand.EmployeeID, 10) == payload.AssigneeID {
			allowed = true
			break
		}
	}
	if !allowed {
		// 保留 rationale 与 usage 供 pipeline 归因；AssigneeID 通过 strconv 回填，
		// pipeline 会看到它不在候选集里、把 Path 判为 PathStage2Invalid。
		// 无法解析的字符串退回 0（0 一定是无效 ID，会走同一条 invalid 路径）。
		id, perr := strconv.ParseInt(payload.AssigneeID, 10, 64)
		if perr != nil {
			id = 0
		}
		choice.AssigneeID = id
		choice.Usage = resp.Usage
		return choice, nil
	}
	id, _ := strconv.ParseInt(payload.AssigneeID, 10, 64)
	choice.AssigneeID = id
	choice.Usage = resp.Usage
	return choice, nil
}

const stage2SystemPrompt = `你是派单助手，负责在若干候选处理人中选一位接手工单。
硬约束：
1. assignee_id 必须是给定 enum 中的一个字符串（员工 ID 数字串），或者 "ESCALATE_HUMAN"。禁止编造任何 ID。
2. rationale 必须引用候选人 profile / ownership / 历史处理记录里的具体证据；一句话。
3. 若候选集合里没有明显合适的人或你自己的判断低于 0.5，选 "ESCALATE_HUMAN"，不要猜。
输出仅一次工具调用，不发散解释。`

// buildStage2Prompt 组装送给模型的 user 消息。
// 只喂 §设计文档 §2.1 里那五样东西，其他一概不给。
func buildStage2Prompt(req Stage2Request, candidates []Candidate) string {
	var b strings.Builder
	fmt.Fprintf(&b, "【升级原因】%s\n\n", req.Weakness)

	b.WriteString("【工单】\n")
	fmt.Fprintf(&b, "ID=%d 标题=%s\n", req.Ticket.ID, req.Ticket.Title)
	fmt.Fprintf(&b, "分类=%s 优先级=%s\n", req.Ticket.Category, req.Ticket.Priority)
	if strings.TrimSpace(req.Ticket.Description) != "" {
		fmt.Fprintf(&b, "描述：%s\n", req.Ticket.Description)
	}
	b.WriteString("\n")

	if len(req.Matches) > 0 {
		b.WriteString("【服务解析 top-K】\n")
		for i, m := range req.Matches {
			fmt.Fprintf(&b, "%d. service=%d score=%.2f\n", i+1, m.ServiceID, m.Score)
		}
		b.WriteString("\n")
	}

	b.WriteString("【候选人】（已按 Stage 1 分数排序）\n")
	for i, cand := range candidates {
		fmt.Fprintf(&b, "%d. id=%d name=%s stage1_total=%.3f ownership=%s",
			i+1, cand.EmployeeID, cand.Name, cand.Total, ownershipLabel(cand.OwnershipHit))
		if ext, ok := req.DirectoryRef.ExtensionOf(cand.EmployeeID); ok && strings.TrimSpace(ext.Profile) != "" {
			fmt.Fprintf(&b, " profile=%s", truncate(ext.Profile, 200))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n请调用 choose_assignee 工具。")
	return b.String()
}

func ownershipLabel(kind domain.OwnershipKind) string {
	switch kind {
	case domain.OwnershipOwner:
		return "owner"
	case domain.OwnershipBackup:
		return "backup"
	case domain.OwnershipTeam:
		return "team"
	default:
		return "none"
	}
}
