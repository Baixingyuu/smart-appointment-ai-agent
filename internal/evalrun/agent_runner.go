// Package evalrun 把 Agent 接到评测框架上。
//
// 独立成包是为了守住依赖方向：internal/eval 不认识 agent，
// internal/agent 也不认识 eval。转换只在这一个地方发生，
// 使评测框架可以脱离具体编排实现被独立测试。
package evalrun

import (
	"context"

	"github.com/mac/agentdesk/internal/agent"
	"github.com/mac/agentdesk/internal/eval"
)

// AgentRunner 用真实 Agent 执行评测用例。
type AgentRunner struct {
	agent *agent.Agent
	ctx   context.Context
}

// New 构造评测用 runner。ctx 为 nil 时使用 context.Background。
func New(ag *agent.Agent, ctx context.Context) *AgentRunner {
	if ctx == nil {
		ctx = context.Background()
	}
	return &AgentRunner{agent: ag, ctx: ctx}
}

// RunTurn 实现 eval.Runner。
func (r *AgentRunner) RunTurn(conversationID int64, message string) (eval.TurnObservation, error) {
	result, err := r.agent.Run(r.ctx, agent.TurnInput{
		ConversationID: conversationID,
		UserMessage:    message,
	})
	if err != nil {
		return eval.TurnObservation{}, err
	}

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
		DurationMS: result.DurationMS,
	}, nil
}
