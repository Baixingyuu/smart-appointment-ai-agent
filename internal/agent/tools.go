package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mac/agentdesk/internal/domain"
	"github.com/mac/agentdesk/internal/tooling"
)

// buildDefinitions 构造四个模型侧工具。
//
// 只给模型这四个工具，是刻意的边界：
//   - 信息收集靠对话追问，不做成工具（模型自己就会问）
//   - 派单、优先级计算是确定性业务规则，留在后端，绝不交给模型
//     （否则「指派准确率」这个指标将无法归因）
func (a *Agent) buildDefinitions() []tooling.Definition {
	return []tooling.Definition{
		a.ragSearchTool(),
		a.findOpenTicketTool(),
		a.createDraftTool(),
		a.createConfirmTool(),
	}
}

func (a *Agent) ragSearchTool() tooling.Definition {
	return tooling.Definition{
		Code: ToolRAGSearch,
		Description: "检索知识库，返回与提问相关的证据片段。回答任何技术问题前都应先调用。" +
			"返回 sufficient=false 时表示知识库证据不足，不可据此作答，应引导创建工单。",
		Risk:     tooling.RiskRead,
		Required: []string{"query"},
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "检索用的提问，尽量使用用户问题中的关键术语",
				},
			},
			"required":             []string{"query"},
			"additionalProperties": false,
		},
		Handler: func(ctx tooling.HandlerContext, args map[string]any) (string, error) {
			query := tooling.StringArg(args, "query")
			if a.retriever == nil {
				// 未配置知识库不是错误，而是一种正常的业务状态：
				// 明确告知模型，让它直接走建单路径。
				return marshalJSON(map[string]any{
					"sufficient": false,
					"reason":     string("no_knowledge"),
					"message":    "当前未配置知识库，无法检索，请引导用户创建工单",
					"hitCount":   0,
				})
			}

			result := a.retriever.Retrieve(ctx.Ctx, query)
			payload := map[string]any{
				"sufficient": result.Gate.Sufficient,
				"reason":     string(result.Gate.Reason),
				"message":    result.Gate.Message,
				"coverage":   result.Gate.Coverage,
				"hitCount":   len(result.Hits),
			}
			if len(result.Hits) > 0 {
				payload["topScore"] = result.Hits[0].Score
			}
			// 证据充分时才附带上下文：不充分时把片段给模型看，
			// 反而会诱导它基于噪声作答。
			if result.Gate.Sufficient {
				payload["evidence"] = result.Context
			}
			return marshalJSON(payload)
		},
	}
}

func (a *Agent) findOpenTicketTool() tooling.Definition {
	return tooling.Definition{
		Code: ToolFindOpenTicket,
		Description: "检查当前会话或客户是否已有未关闭的同类工单。创建工单前应先调用，避免重复建单。" +
			"若已存在，应告知用户已有工单在处理中，而不是再建一张。",
		Risk:     tooling.RiskRead,
		Required: []string{"topic"},
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"topic": map[string]any{
					"type":        "string",
					"description": "要检查的问题主题，用于与已有工单比对",
				},
			},
			"required":             []string{"topic"},
			"additionalProperties": false,
		},
		Handler: func(ctx tooling.HandlerContext, args map[string]any) (string, error) {
			topic := tooling.StringArg(args, "topic")
			existing := a.store.FindOpenTicketsByConversation(ctx.ConversationID)

			type ticketBrief struct {
				ID       int64  `json:"id"`
				Title    string `json:"title"`
				Status   string `json:"status"`
				Assignee string `json:"assignee,omitempty"`
			}
			briefs := make([]ticketBrief, 0, len(existing))
			for _, t := range existing {
				brief := ticketBrief{ID: t.ID, Title: t.Title, Status: string(t.Status)}
				if t.AssigneeID > 0 {
					if emp, err := a.store.GetEmployee(t.AssigneeID); err == nil {
						brief.Assignee = emp.Name
					}
				}
				briefs = append(briefs, brief)
			}

			return marshalJSON(map[string]any{
				"topic":         topic,
				"found":         len(briefs) > 0,
				"openTicketNum": len(briefs),
				"tickets":       briefs,
				"message": pickMessage(len(briefs) > 0,
					"该会话已有未关闭工单，请勿重复创建",
					"未发现未关闭的同类工单，可以创建"),
			})
		},
	}
}

func (a *Agent) createDraftTool() tooling.Definition {
	return tooling.Definition{
		Code: ToolCreateDraft,
		Description: "整理工单字段草稿并检查信息是否齐备。此工具只做校验与整理，不会创建工单。" +
			"缺少次要信息时不要拒绝创建，把缺失项填进 missingInfo 即可。",
		Risk:     tooling.RiskRead,
		Required: []string{"title"},
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title":       map[string]any{"type": "string", "description": "工单标题，简明概括问题，不超过 40 字"},
				"description": map[string]any{"type": "string", "description": "问题现象、影响范围、已尝试的操作"},
				"category": map[string]any{
					"type":        "string",
					"enum":        []string{"incident", "consultation", "request", "change"},
					"description": "工单类型：incident 故障 / consultation 咨询 / request 服务请求 / change 变更",
				},
				"priority": map[string]any{
					"type":        "string",
					"enum":        []string{"P0", "P1", "P2", "P3"},
					"description": "优先级：P0 系统瘫痪或核心业务中断 / P1 核心功能不可用 / P2 部分异常 / P3 一般问题",
				},
				"missingInfo": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "尚缺的关键信息字段名，例如 复现步骤、影响范围",
				},
			},
			"required":             []string{"title"},
			"additionalProperties": false,
		},
		Handler: func(_ tooling.HandlerContext, args map[string]any) (string, error) {
			title := tooling.StringArg(args, "title")
			description := tooling.StringArg(args, "description")
			category := tooling.StringArg(args, "category")
			priority := tooling.StringArg(args, "priority")
			missing := tooling.StringSliceArg(args, "missingInfo")

			// 服务端自行判断必要字段，不采信模型自称"信息已完整"：
			// 描述为空是实质缺失，必须显式记录，否则处理人要重新问一遍。
			if description == "" && !containsString(missing, "description") {
				missing = append(missing, "description")
			}

			issues := make([]string, 0, 2)
			if !domain.Category(category).Valid() {
				issues = append(issues, fmt.Sprintf("类型 %q 不合法，将按 incident 处理", category))
			}
			if !domain.Priority(priority).Valid() {
				issues = append(issues, fmt.Sprintf("优先级 %q 不合法，将按 P3 处理", priority))
			}

			return marshalJSON(map[string]any{
				"ready":       len(missing) == 0,
				"title":       title,
				"description": description,
				"category":    category,
				"priority":    priority,
				"missingInfo": missing,
				"issues":      issues,
				"message": pickMessage(len(missing) == 0,
					"字段齐备，可发起创建确认",
					"字段不齐备，可先向用户追问，或直接创建并在 missingInfo 中标注"),
			})
		},
	}
}

func (a *Agent) createConfirmTool() tooling.Definition {
	return tooling.Definition{
		Code: ToolCreateConfirm,
		Description: "向用户发起工单创建确认。调用后不会立即创建工单，而是先向用户展示工单内容并等待确认。" +
			"用户确认后工单才会真正创建并自动指派处理人。",
		// 写操作且必须确认：这是整个治理层的核心约束，
		// 没有它模型就能绕过用户直接改数据。
		Risk:                tooling.RiskWrite,
		RequireConfirmation: true,
		Required:            []string{"title"},
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title":       map[string]any{"type": "string", "description": "工单标题，简明概括问题，不超过 40 字"},
				"description": map[string]any{"type": "string", "description": "问题现象、影响范围、已尝试的操作"},
				"category": map[string]any{
					"type":        "string",
					"enum":        []string{"incident", "consultation", "request", "change"},
					"description": "工单类型",
				},
				"priority": map[string]any{
					"type":        "string",
					"enum":        []string{"P0", "P1", "P2", "P3"},
					"description": "优先级",
				},
				"skillIds": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "integer"},
					"description": "该问题涉及的技能编号，用于匹配有相关经验的处理人。留空则由系统按类型推导",
				},
				"missingInfo": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "尚缺的关键信息",
				},
			},
			"required":             []string{"title"},
			"additionalProperties": false,
		},
		// Handler 不会被执行：写操作在治理层被拦下并转为确认中断。
		// 仍然提供一个明确的空实现，避免误配置时静默通过。
		Handler: func(_ tooling.HandlerContext, _ map[string]any) (string, error) {
			return "", errors.New("写操作必须经由确认中断执行，不应直接调用")
		},
	}
}

// marshalJSON 序列化工具观察结果。
//
// 工具返回值必须是稳定 JSON：模型据此判断，评测也据此还原详情。
// 若序列化失败则返回可读错误而非空字符串——空字符串会让模型以为
// 工具执行成功但没有内容。
func marshalJSON(payload map[string]any) (string, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("工具结果序列化失败: %w", err)
	}
	return string(data), nil
}

func pickMessage(condition bool, whenTrue, whenFalse string) string {
	if condition {
		return whenTrue
	}
	return whenFalse
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if strings.EqualFold(strings.TrimSpace(item), target) {
			return true
		}
	}
	return false
}
