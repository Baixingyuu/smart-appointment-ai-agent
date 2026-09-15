// Package classify 实现意图分类。
//
// 存在的理由：一期没有显式的意图路由——路由完全隐式地由模型选工具完成，
// 因此「路由准确性」这个指标无法单独度量。同时，寒暄类输入会走完整的
// 检索 + 工具链路，白白付出工具目录与证据的 token 成本。
//
// 一次轻量分类调用同时解决这两个问题：既可度量，又能短路不必要的主循环。
//
// 成本设计：分类调用刻意保持极小——短提示词 + 单 token 级输出，
// 而它省掉的是主循环里数千 token 的工具 schema 与证据上下文。
// 因此净效果是降本，但必须实测验证（见 eval 的按意图成本分解）。
package classify

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mac/helpdesk-agent/internal/domain"
	"github.com/mac/helpdesk-agent/internal/llm"
)

// Result 一次分类的结果。
type Result struct {
	Intent domain.Intent
	// Raw 为模型原始输出，保留用于诊断解析失败的情况。
	Raw string
	// Parsed 为 false 表示模型输出无法解析为已知意图。
	// 这类样本在评测中应单独统计，而不是简单地计为「分类错误」——
	// 两者的修复方向不同（改提示词 vs 改分类体系）。
	Parsed bool
	Usage  llm.Usage
}

// Classifier 意图分类器。
type Classifier struct {
	model  llm.ChatModel
	system string
}

// New 构造分类器。
func New(model llm.ChatModel) *Classifier {
	return &Classifier{model: model, system: systemPrompt}
}

// systemPrompt 是为分类任务定制的极小提示词。
//
// 刻意不复用主助手的系统提示词：主提示词包含完整的工作原则、
// 工具使用规则与建单流程，用于分类纯属浪费——分类只需要知道
// 五个类别的边界。
const systemPrompt = `你是意图分类器。把用户消息归入且仅归入以下一类，只输出类别名，不要任何其它文字：

chitchat - 寒暄、致谢、告别、与技术无关的闲聊
knowledge - 询问功能、用法、排查步骤，期望从知识库得到答案
incident - 报告故障、要求登记问题、明确要求建单
handoff - 明确要求转人工或找真人客服
out_of_scope - 与技术支持完全无关的请求

判定要点：
- 问「怎么做/什么原因」是 knowledge；说「坏了/不能用/帮我登记」是 incident
- 只要明确要求人工或真人，无论主题都归 handoff
- 要求做一件具体无关的事（写诗、推荐餐厅）是 out_of_scope；仅打招呼致谢是 chitchat`

// Classify 判定消息意图。
//
// 任何失败都回退到 IntentKnowledge：
// 它是覆盖面最广、行为最保守的选择——会走完整的检索与工具链路，
// 与一期「无路由时」的行为一致，因此分类失败不会引入新的风险，
// 只是丧失了短路带来的成本收益。
func (c *Classifier) Classify(ctx context.Context, message string) (Result, error) {
	message = strings.TrimSpace(message)
	if message == "" {
		return Result{}, errors.New("消息不能为空")
	}
	if c == nil || c.model == nil {
		return Result{}, errors.New("未配置模型")
	}

	response, err := c.model.Chat(ctx, c.system, message)
	if err != nil {
		return Result{}, fmt.Errorf("意图分类调用失败: %w", err)
	}

	result := Result{Raw: response.Content, Usage: response.Usage}
	intent, ok := domain.ParseIntent(response.Content)
	if ok {
		result.Intent = intent
		result.Parsed = true
		return result, nil
	}

	// 输出无法解析时重试一次。
	//
	// 必要性来自实测：思维链模型偶发返回空内容（completion 仅数个 token），
	// 在 180 条样本的完整评测中出现过一次。这类失败是瞬时的，
	// 重试的边际成本极低（一次短调用），但能避免把偶发故障计入准确率。
	retry, retryErr := c.model.Chat(ctx, c.system, message)
	result.Usage.PromptTokens += retry.Usage.PromptTokens
	result.Usage.CompletionTokens += retry.Usage.CompletionTokens
	if retryErr == nil {
		if retryIntent, retryOK := domain.ParseIntent(retry.Content); retryOK {
			result.Intent = retryIntent
			result.Raw = retry.Content
			result.Parsed = true
			return result, nil
		}
	}

	// 重试仍失败：回退到覆盖面最广的意图。
	// 该行为的后果（多走一次完整工具链路）与一期「无路由」时一致，
	// 因此不会引入新风险，只是丧失短路的成本收益。
	result.Intent = domain.IntentKnowledge
	result.Parsed = false
	return result, nil
}
