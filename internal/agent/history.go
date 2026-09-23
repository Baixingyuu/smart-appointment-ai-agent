package agent

import (
	"strings"

	"github.com/mac/helpdesk-agent/internal/domain"
	"github.com/mac/helpdesk-agent/internal/llm"
)

// 跨轮上下文。
//
// 修复的具体缺陷：此前每次模型调用只带当前这一条消息，于是「还是没弄好，
// 帮我建个单跟进」里的「没弄好」指的是什么，模型完全无从知道
// （实测样本 eval/samples/manual-20260920-跨轮上下文缺失.jsonl）。
// 用户看到的是"我说过的事它全忘了"。

// turnMessages 组装一次回合送给模型的消息序列：历史窗口 + 当前这条。
func (a *Agent) turnMessages(input TurnInput) []llm.Message {
	history := a.historyWindow(input.ConversationID, input.UserMessage)
	return append(history, llm.Message{Role: llm.RoleUser, Content: input.UserMessage})
}

// historyWindow 取会话内最近的历史消息并投影成模型消息。
//
// 只用于主工具循环：短路路径（寒暄/无关）与意图分类器输入保持单条消息。
// 前者是省成本的确定性路径，后者的判定只针对当前这句话，
// 给它们加历史只会增加上下文压力而不会改变判定结果。
//
// 关键前提：客户消息在 AI 回合执行**之前**就已落库
// （conversation.Service.Send 先 AppendMessage 再 executor.Execute），
// 因此这里的回读必然包含当前这条输入。不去掉就等于把同一句话
// 送给模型两遍，模型会认为用户重复说过。
func (a *Agent) historyWindow(conversationID int64, current string) []llm.Message {
	limit := a.config.HistoryMaxMessages
	if limit <= 0 || conversationID <= 0 {
		return nil
	}

	items := dropCurrentTail(a.store.MessagesByConversation(conversationID), current)
	if len(items) > limit {
		items = items[len(items)-limit:]
	}

	// 从最近一条往前累积，超出总预算就停：
	// 宁可丢最早的历史，也不能丢用户刚说的话。
	projected := make([]llm.Message, 0, len(items))
	remaining := a.config.HistoryMaxRunes
	for i := len(items) - 1; i >= 0; i-- {
		msg, ok := projectHistoryMessage(items[i], a.config.HistoryMaxItemRunes)
		if !ok {
			continue
		}
		cost := len([]rune(msg.Content))
		if cost > remaining {
			break
		}
		remaining -= cost
		projected = append(projected, msg)
	}

	for i, j := 0, len(projected)-1; i < j; i, j = i+1, j-1 {
		projected[i], projected[j] = projected[j], projected[i]
	}
	return projected
}

// projectHistoryMessage 把一条会话消息投影为模型消息。
//
// 人工客服与系统通知一律投成 user 而非 assistant：模型必须把它们当作
// 别人的输入。投成 assistant 会让模型认为那些话是自己说过的，
// 进而据此行动（例如声称自己已经处理过某件事）。
func projectHistoryMessage(item domain.Message, maxItemRunes int) (llm.Message, bool) {
	content := capRunes(strings.TrimSpace(item.Content), maxItemRunes)
	if content == "" {
		return llm.Message{}, false
	}
	switch item.Sender {
	case domain.SenderCustomer:
		return llm.Message{Role: llm.RoleUser, Content: content}, true
	case domain.SenderAI:
		return llm.Message{Role: llm.RoleAssistant, Content: content}, true
	case domain.SenderHuman:
		return llm.Message{Role: llm.RoleUser, Content: "【人工客服】" + content}, true
	case domain.SenderSystem:
		return llm.Message{Role: llm.RoleUser, Content: "【系统】" + content}, true
	default:
		return llm.Message{}, false
	}
}

// dropCurrentTail 去掉尾部那条与当前输入相同的客户消息。
//
// 只去掉一条：多去会把用户真的重复说过一次的信息也抹掉。
func dropCurrentTail(items []domain.Message, current string) []domain.Message {
	if len(items) == 0 {
		return nil
	}
	last := items[len(items)-1]
	if last.Sender == domain.SenderCustomer && strings.TrimSpace(last.Content) == strings.TrimSpace(current) {
		return items[:len(items)-1]
	}
	return items
}

// capRunes 按 rune 截断，避免把一个汉字切成半个字节序列。
func capRunes(value string, maxRunes int) string {
	if maxRunes <= 0 {
		return value
	}
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return strings.TrimSpace(string(runes[:maxRunes])) + "…"
}
