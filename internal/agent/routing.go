package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mac/agentdesk/internal/domain"
	"github.com/mac/agentdesk/internal/llm"
)

// routeAndShortCircuit 做意图路由，并在意图无需工具循环时直接给出回复。
//
// 返回值 handled 为 false 表示应继续走完整的工具决策循环。
//
// 成本依据：完整链路的固定开销是系统提示词 + 四个工具 schema + 证据上下文
// （实测约 1,400 prompt token 起步）。寒暄与无关请求既不需要检索，
// 也不需要任何工具，走完整链路纯属浪费。短路路径只付一次分类调用
// 加一次短回复，净效果是降本。
func (a *Agent) routeAndShortCircuit(ctx context.Context, input TurnInput, startedAt time.Time, result *TurnResult) (bool, error) {
	if a.classifier == nil {
		return false, nil
	}

	outcome, err := a.classifier.ClassifyIntent(input.UserMessage)
	if err != nil {
		// 分类失败不阻断主流程：回退到完整链路，行为与无路由版本一致，
		// 只是丧失了短路的成本收益。让整个回合失败才是更糟的选择。
		return false, nil
	}
	// 把意图与分类成本记进结果，无论是否短路：
	// 分类 token 必须计入总成本，否则「加了分类层是升还是降」无法核算。
	result.Intent = outcome.Intent
	result.Usage.PromptTokens += outcome.Usage.PromptTokens
	result.Usage.CompletionTokens += outcome.Usage.CompletionTokens

	if outcome.Intent.NeedsToolLoop() {
		return false, nil
	}

	shortCircuit, err := a.shortCircuitReply(ctx, input, outcome)
	if err != nil {
		return false, err
	}
	*result = *shortCircuit
	a.finish(result, startedAt)
	return true, nil
}

// shortCircuitReply 生成短路路径的回复。
//
// 走一次模型调用而非返回固定话术：固定话术在真实客服场景里体验很差，
// 而且无法应答「你是机器人吗」这类需要针对性回应的问题。
// 这次调用的输入只有一句极短的指令，成本远低于完整链路。
func (a *Agent) shortCircuitReply(ctx context.Context, input TurnInput, outcome IntentOutcome) (*TurnResult, error) {
	system, err := a.shortCircuitPrompt(outcome.Intent)
	if err != nil {
		return nil, err
	}

	response, err := a.model.Chat(ctx, system, input.UserMessage)
	if err != nil {
		// 回复生成失败：降级为一句可读的兜底话术，而不是把错误抛给用户。
		response = &llm.Response{
			Content: fallbackReply(outcome.Intent),
			Usage:   llm.Usage{},
		}
	}

	result := &TurnResult{
		Intent:         outcome.Intent,
		ShortCircuited: true,
		Reply:          strings.TrimSpace(response.Content),
		Rounds:         0,
	}
	if result.Reply == "" {
		result.Reply = fallbackReply(outcome.Intent)
	}
	result.Usage.PromptTokens = outcome.Usage.PromptTokens + response.Usage.PromptTokens
	result.Usage.CompletionTokens = outcome.Usage.CompletionTokens + response.Usage.CompletionTokens
	result.RoundRecords = append(result.RoundRecords, RoundRecord{
		Round:            1,
		PromptTokens:     outcome.Usage.PromptTokens + response.Usage.PromptTokens,
		CompletionTokens: outcome.Usage.CompletionTokens + response.Usage.CompletionTokens,
		Tools:            nil, // 短路路径不调用任何工具
	})
	return result, nil
}

// shortCircuitPrompt 返回短路路径的系统提示词。
func (a *Agent) shortCircuitPrompt(intent domain.Intent) (string, error) {
	switch intent {
	case domain.IntentChitchat:
		return "你是企业技术支持客服助手。用户在做简短寒暄或致谢，请用一到两句话友好回应，" +
			"并自然引导对方描述具体的技术问题。不要编造任何业务信息，不要提及工单。", nil
	case domain.IntentHandoff:
		return "你是企业技术支持客服助手。用户明确要求转人工或找真人客服，" +
			"请用一到两句话确认已收到转接请求，并说明人工客服将尽快接入。" +
			"不要声称已经完成转接（转接由系统执行），不要询问技术细节。", nil
	case domain.IntentOutOfScope:
		return "你是企业技术支持客服助手。用户的问题与技术支持和工单无关，" +
			"请用一到两句话礼貌说明你只能协助技术支持相关的问题，并邀请对方描述遇到的技术问题。" +
			"不要尝试回答无关问题。", nil
	default:
		return "", fmt.Errorf("意图 %s 不应走短路路径", intent)
	}
}

// fallbackReply 在模型调用失败时给出可读兜底。
func fallbackReply(intent domain.Intent) string {
	switch intent {
	case domain.IntentChitchat:
		return "你好，请描述你遇到的技术问题，我来帮你处理。"
	case domain.IntentHandoff:
		return "已收到你的转接请求，人工客服会尽快接入。"
	case domain.IntentOutOfScope:
		return "抱歉，我只能协助技术支持相关的问题。你可以描述一下遇到的技术问题。"
	default:
		return "请描述你遇到的技术问题。"
	}
}
