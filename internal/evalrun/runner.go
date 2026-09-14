// Package evalrun 把业务实现接到评测框架上。
//
// 独立成包是为了守住依赖方向：internal/eval 不认识 agent 与 classify，
// 业务包也不认识 eval 的报告结构。转换只在这一个地方发生，
// 使评测框架可以脱离具体实现被独立测试。
package evalrun

import (
	"context"

	"github.com/mac/agentdesk/internal/agent"
	"github.com/mac/agentdesk/internal/classify"
	"github.com/mac/agentdesk/internal/eval"
	"github.com/mac/agentdesk/internal/rag"
)

// AgentRunner 用真实 Agent 执行轨迹评测用例。
type AgentRunner struct {
	agent *agent.Agent
	ctx   context.Context
}

// New 构造轨迹评测用 runner。ctx 为 nil 时使用 context.Background。
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
		DurationUS: int(result.DurationUS),
	}, nil
}

// IntentRunner 用真实分类器执行意图评测。
type IntentRunner struct {
	classifier *classify.Classifier
	ctx        context.Context
}

// NewIntentRunner 构造意图评测用 runner。
func NewIntentRunner(classifier *classify.Classifier, ctx context.Context) *IntentRunner {
	if ctx == nil {
		ctx = context.Background()
	}
	return &IntentRunner{classifier: classifier, ctx: ctx}
}

// ClassifyIntent 实现 eval.IntentClassifier。
func (r *IntentRunner) ClassifyIntent(text string) (eval.IntentPrediction, error) {
	result, err := r.classifier.Classify(r.ctx, text)
	if err != nil {
		return eval.IntentPrediction{}, err
	}
	return eval.IntentPrediction{
		Intent:           result.Intent,
		Raw:              result.Raw,
		Parsed:           result.Parsed,
		PromptTokens:     result.Usage.PromptTokens,
		CompletionTokens: result.Usage.CompletionTokens,
	}, nil
}

// ClassifierRunner 把分类器适配到 agent.IntentClassifier。
//
// 与 IntentRunner 的区别：IntentRunner 面向评测（返回 eval 类型），
// 本适配器面向编排（返回 agent 类型）。两者都保留，
// 避免让任一方向反过来依赖对方的结构。
type ClassifierRunner struct {
	classifier *classify.Classifier
	ctx        context.Context
}

// NewClassifierRunner 构造编排用分类器适配器。
func NewClassifierRunner(classifier *classify.Classifier) *ClassifierRunner {
	return &ClassifierRunner{classifier: classifier, ctx: context.Background()}
}

// ClassifyIntent 实现 agent.IntentClassifier。
func (r *ClassifierRunner) ClassifyIntent(text string) (agent.IntentOutcome, error) {
	result, err := r.classifier.Classify(r.ctx, text)
	if err != nil {
		return agent.IntentOutcome{}, err
	}
	return agent.IntentOutcome{
		Intent: result.Intent,
		Parsed: result.Parsed,
		Usage:  result.Usage,
	}, nil
}

// RetrievalRunner 用真实检索器执行检索评测。
type RetrievalRunner struct {
	retriever *rag.Retriever
	ctx       context.Context
}

// NewRetrievalRunner 构造检索评测用 runner。
func NewRetrievalRunner(retriever *rag.Retriever, ctx context.Context) *RetrievalRunner {
	if ctx == nil {
		ctx = context.Background()
	}
	return &RetrievalRunner{retriever: retriever, ctx: ctx}
}

// RetrieveForEval 实现 eval.Retriever。
func (r *RetrievalRunner) RetrieveForEval(query string, topK int) (eval.RetrievalResult, error) {
	options := r.retriever.Options()
	// 用评测指定的 K 覆盖检索器默认值，使 Recall@K 的含义确定。
	options.TopK = topK
	engine := rag.NewWithScorer(r.retriever.Chunks(), r.retriever.Embedder(), r.retriever.Scorer(), options)
	result := engine.Retrieve(r.ctx, query)

	hits := make([]eval.RetrievalHit, 0, len(result.Hits))
	for _, hit := range result.Hits {
		hits = append(hits, eval.RetrievalHit{
			DocID: hit.Chunk.DocID,
			Chunk: hit.Chunk.ID,
			Score: hit.Score,
		})
	}
	return eval.RetrievalResult{
		Hits:       hits,
		DecidedHit: result.Gate.Sufficient,
		Reason:     string(result.Gate.Reason),
	}, nil
}
