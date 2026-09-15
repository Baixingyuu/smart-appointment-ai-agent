// Package conversation 实现会话与消息编排。
//
// 职责：把「客户发来一条消息」变成「一条客户消息 + 一次 Agent 回合 + 一条 AI 回复」，
// 并保证三件事：
//
//	① 幂等 —— 同一 RequestID 重复投递不会产生两条消息，也不会重复触发 AI
//	② 顺序 —— 消息按追加顺序落库，回复写在对应位置上
//	③ 状态 —— 已关闭的会话不再接受新消息，也不触发 AI
package conversation

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mac/helpdesk-agent/internal/domain"
	"github.com/mac/helpdesk-agent/internal/store"
)

// TurnExecutor 执行一次 Agent 回合。
//
// 抽象为接口而非直接依赖 agent 包：会话编排不应绑定具体编排实现，
// 测试也可注入桩。
type TurnExecutor interface {
	Execute(conversationID int64, message string) (TurnOutcome, error)
}

// TurnOutcome 一次回合的结果。
type TurnOutcome struct {
	Reply       string
	Interrupted bool
	TicketID    int64
}

// TurnExecutorFunc 便于用函数构造 TurnExecutor。
type TurnExecutorFunc func(conversationID int64, message string) (TurnOutcome, error)

// Execute 实现 TurnExecutor。
func (f TurnExecutorFunc) Execute(conversationID int64, message string) (TurnOutcome, error) {
	return f(conversationID, message)
}

// Service 会话服务。
type Service struct {
	store    store.Store
	executor TurnExecutor
	now      func() time.Time
}

// Option 调整服务行为。
type Option func(*Service)

// WithClock 注入时钟，用于测试。
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// New 构造会话服务。executor 可为 nil，此时只做消息落库不触发 AI。
func New(st store.Store, executor TurnExecutor, opts ...Option) *Service {
	svc := &Service{store: st, executor: executor, now: time.Now}
	for _, opt := range opts {
		opt(svc)
	}
	return svc
}

// StartInput 新建会话的输入。
type StartInput struct {
	Title         string
	SourceChannel string
	CustomerRef   string
}

// Start 新建会话。
func (s *Service) Start(input StartInput) (domain.Conversation, error) {
	channel := strings.TrimSpace(input.SourceChannel)
	if channel == "" {
		channel = "web"
	}
	title := strings.TrimSpace(input.Title)
	if title == "" {
		title = "新的咨询"
	}

	item := domain.Conversation{
		Title:         title,
		Status:        domain.ConversationActive,
		SourceChannel: channel,
		CustomerRef:   strings.TrimSpace(input.CustomerRef),
	}
	id, err := s.store.SaveConversation(item)
	if err != nil {
		return domain.Conversation{}, err
	}
	return s.store.GetConversation(id)
}

// SendResult 一次客户消息处理的结果。
type SendResult struct {
	CustomerMessage domain.Message
	Reply           *domain.Message
	// Duplicate 为 true 时表示该 RequestID 已处理过，本次未产生任何新数据。
	Duplicate bool
	// Turn 为 AI 回合结果，未触发 AI 时为 nil。
	Turn *TurnOutcome
}

// Send 处理一条客户消息：落库客户消息、执行 AI 回合、落库回复。
//
// 幂等语义：RequestID 重复时直接返回 Duplicate 而不报错。
// 返回错误会让客户端重试，重试又会再次命中重复——用 Duplicate 标记
// 比用错误更利于调用方区分「重复」与「真失败」。
func (s *Service) Send(conversationID int64, content, requestID string) (SendResult, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return SendResult{}, errors.New("消息内容不能为空")
	}

	conv, err := s.store.GetConversation(conversationID)
	if err != nil {
		return SendResult{}, fmt.Errorf("会话 %d 不存在: %w", conversationID, err)
	}
	// 已关闭的会话不再接受新消息：否则会出现「会话已结束但 AI 仍在回复」，
	// 且新问题会被挂在旧会话上，导致去重逻辑误判。
	if conv.Closed() {
		return SendResult{}, fmt.Errorf("%w: 会话 %d", domain.ErrConversationClosed, conversationID)
	}

	customer := domain.Message{
		ConversationID: conversationID,
		Sender:         domain.SenderCustomer,
		Content:        content,
		RequestID:      strings.TrimSpace(requestID),
	}
	if err := s.store.AppendMessage(&customer); err != nil {
		if errors.Is(err, domain.ErrDuplicateRequest) {
			return SendResult{Duplicate: true}, nil
		}
		return SendResult{}, err
	}
	result := SendResult{CustomerMessage: customer}

	// 未配置执行器时只落库消息，便于离线导入历史数据。
	if s.executor == nil {
		return result, nil
	}

	outcome, err := s.executor.Execute(conversationID, content)
	if err != nil {
		// AI 失败不应丢失客户消息：消息已经落库，错误向上返回让调用方
		// 决定是否转人工，而不是把整条消息回滚掉。
		return result, fmt.Errorf("AI 回合执行失败: %w", err)
	}
	result.Turn = &outcome

	if strings.TrimSpace(outcome.Reply) != "" {
		reply := domain.Message{
			ConversationID: conversationID,
			Sender:         domain.SenderAI,
			Content:        strings.TrimSpace(outcome.Reply),
		}
		if err := s.store.AppendMessage(&reply); err != nil {
			return result, fmt.Errorf("写入 AI 回复失败: %w", err)
		}
		result.Reply = &reply
	}

	// 会话标题沿用首条客户消息，避免列表里全是「新的咨询」。
	if conv.Title == "新的咨询" {
		conv.Title = summarizeTitle(content)
		if _, err := s.store.SaveConversation(conv); err != nil {
			return result, err
		}
	}
	return result, nil
}

// Close 关闭会话。
func (s *Service) Close(conversationID int64) (domain.Conversation, error) {
	conv, err := s.store.GetConversation(conversationID)
	if err != nil {
		return domain.Conversation{}, err
	}
	conv.Status = domain.ConversationClosed
	if _, err := s.store.SaveConversation(conv); err != nil {
		return domain.Conversation{}, err
	}
	return s.store.GetConversation(conversationID)
}

// Detail 会话详情。
type Detail struct {
	Conversation domain.Conversation
	Messages     []domain.Message
}

// Get 读取会话详情。
func (s *Service) Get(conversationID int64) (Detail, error) {
	conv, err := s.store.GetConversation(conversationID)
	if err != nil {
		return Detail{}, err
	}
	return Detail{
		Conversation: conv,
		Messages:     s.store.MessagesByConversation(conversationID),
	}, nil
}

// List 列出全部会话。
func (s *Service) List() []domain.Conversation { return s.store.ListConversations() }

// summarizeTitle 由消息内容生成简短标题。
func summarizeTitle(content string) string {
	text := strings.Join(strings.Fields(content), " ")
	runes := []rune(text)
	if len(runes) > 24 {
		return string(runes[:24]) + "…"
	}
	return text
}
