package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ConversationStatus 会话状态。
type ConversationStatus string

const (
	ConversationActive ConversationStatus = "active" // AI 接待中
	ConversationClosed ConversationStatus = "closed" // 已结束
)

// Conversation 一次客户咨询会话。
//
// 与工单的关系：一个会话可以产生多张工单（问题解决后再提问），
// 但同一时刻只应有一张未关闭工单——去重规则依赖这一点。
type Conversation struct {
	ID     int64
	Title  string
	Status ConversationStatus
	// SourceChannel 接入渠道，如 web。
	SourceChannel string
	// CustomerRef 客户标识。一期不做客户主数据，仅保留外部引用。
	CustomerRef string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Validate 校验会话数据。
func (c *Conversation) Validate() error {
	if c.ID < 0 {
		return errors.New("会话 ID 不能为负")
	}
	if c.Status != ConversationActive && c.Status != ConversationClosed {
		return fmt.Errorf("无效的会话状态: %q", c.Status)
	}
	return nil
}

// Closed 判断会话是否已关闭。
func (c Conversation) Closed() bool { return c.Status == ConversationClosed }

// MessageSender 消息发送方。
type MessageSender string

const (
	SenderCustomer MessageSender = "customer" // 客户
	SenderAI       MessageSender = "ai"       // AI 助手
	SenderHuman    MessageSender = "human"    // 人工客服
	SenderSystem   MessageSender = "system"   // 系统提示（如指派通知）
)

// Message 一条会话消息。
type Message struct {
	ID             int64
	ConversationID int64
	Sender         MessageSender
	Content        string
	// RequestID 幂等键：同一次客户提交重复到达时应被识别并丢弃，
	// 否则会重复触发 AI 回复甚至重复建单。
	RequestID string
	CreatedAt time.Time
}

// Validate 校验消息数据。
func (m *Message) Validate() error {
	if m.ConversationID <= 0 {
		return errors.New("消息必须关联会话")
	}
	if strings.TrimSpace(m.Content) == "" {
		return errors.New("消息内容不能为空")
	}
	switch m.Sender {
	case SenderCustomer, SenderAI, SenderHuman, SenderSystem:
	default:
		return fmt.Errorf("无效的消息发送方: %q", m.Sender)
	}
	return nil
}

// ErrConversationClosed 表示会话已关闭，不能再追加消息。
var ErrConversationClosed = errors.New("会话已关闭")

// ErrDuplicateRequest 表示同一 RequestID 的消息已处理过。
var ErrDuplicateRequest = errors.New("重复的请求")
