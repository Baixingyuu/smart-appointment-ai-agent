package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// TicketProgress 工单进展记录。
//
// 参考实现（agent-desk）的状态变更不写进展，导致「谁在什么时候改了什么」
// 无法审计；本项目要求每一次状态流转与指派都留下记录。
type TicketProgress struct {
	ID        int64
	TicketID  int64
	Kind      ProgressKind
	Content   string
	AuthorID  int64
	CreatedAt time.Time
}

// ProgressKind 进展类型。
//
// 参考实现的进展是单一自由文本列，创建、指派、人工备注混在一起，
// 只能靠解析中文字符串区分。显式类型化后可按类型统计与断言。
type ProgressKind string

const (
	ProgressCreated   ProgressKind = "created"   // 工单创建
	ProgressAssigned  ProgressKind = "assigned"  // 指派
	ProgressAccepted  ProgressKind = "accepted"  // 处理人接单
	ProgressEscalated ProgressKind = "escalated" // 升级
	ProgressResolved  ProgressKind = "resolved"  // 处理完成
	ProgressComment   ProgressKind = "comment"   // 人工备注
	ProgressDedupe    ProgressKind = "dedupe"    // 命中重复工单，追加而非新建
)

// TicketAssignmentLog 已移除：指派审计直接复用 domain.AssignmentLog
// （见 domain.go），它已包含候选评分明细。避免维护两个结构相同、
// 语义重叠的类型导致赋值处需要不断转换。

// TicketWorkflow 描述工单的完整生命周期。
//
// 允许的流转：
//
//	pending --assign--> pending(已指派)
//	pending --accept--> in_progress
//	in_progress --resolve--> done
//	pending/in_progress --escalate--> 重新指派（清空处理人）
//
// 参考实现中 in_progress 从不由任何代码路径自动设置，
// 导致「处理中」状态实际不可达；本项目要求 accept 必须显式触发该流转。
type TicketWorkflow struct{}

var workflow = TicketWorkflow{}

// ErrInvalidTransition 表示非法的状态流转。
var ErrInvalidTransition = errors.New("invalid ticket status transition")

// CanAssign 判断当前状态是否允许指派。
func (TicketWorkflow) CanAssign(t Ticket) error {
	if t.Status == TicketStatusDone {
		return fmt.Errorf("%w: 已完成的工单不能重新指派", ErrInvalidTransition)
	}
	return nil
}

// CanAccept 判断当前状态是否允许接单。
//
// 未指派的工单不能被接单：没有处理人的工单进入处理中会让责任人不可追溯。
func (TicketWorkflow) CanAccept(t Ticket) error {
	if t.AssigneeID <= 0 {
		return fmt.Errorf("%w: 工单尚未指派，不能接单", ErrInvalidTransition)
	}
	if t.Status != TicketStatusPending {
		return fmt.Errorf("%w: 只有待处理工单可以接单，当前状态 %s", ErrInvalidTransition, t.Status)
	}
	return nil
}

// CanResolve 判断当前状态是否允许标记完成。
func (TicketWorkflow) CanResolve(t Ticket) error {
	if t.Status != TicketStatusInProgress {
		return fmt.Errorf("%w: 只有处理中的工单可以标记完成，当前状态 %s", ErrInvalidTransition, t.Status)
	}
	return nil
}

// CanEscalate 判断当前状态是否允许升级。
func (TicketWorkflow) CanEscalate(t Ticket) error {
	if t.Status == TicketStatusDone {
		return fmt.Errorf("%w: 已完成的工单不能升级", ErrInvalidTransition)
	}
	return nil
}

// CanAttachProgress 判断是否允许追加进展。
func (TicketWorkflow) CanAttachProgress(t Ticket) error {
	if t.Status == TicketStatusDone {
		return fmt.Errorf("%w: 已完成的工单不能再追加进展", ErrInvalidTransition)
	}
	return nil
}

// Transitions 返回状态机的允许边，供文档与测试共用。
func (TicketWorkflow) Transitions() map[TicketStatus][]TicketStatus {
	return map[TicketStatus][]TicketStatus{
		TicketStatusPending:    {TicketStatusInProgress},
		TicketStatusInProgress: {TicketStatusDone},
		TicketStatusDone:       {},
	}
}

// AddMissingInfo 记录一条未收集到的关键信息。
//
// 信息不全不阻塞建单：真实客服场景中用户往往无法一次说清，
// 卡住不建单会导致问题丢失。缺什么应显式记录，供处理人补充。
func (t *Ticket) AddMissingInfo(fields ...string) {
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		if t.HasMissingInfo(field) {
			continue
		}
		t.MissingInfo = append(t.MissingInfo, field)
	}
}

// HasMissingInfo 判断某字段是否已被标记为缺失。
func (t Ticket) HasMissingInfo(field string) bool {
	for _, item := range t.MissingInfo {
		if item == field {
			return true
		}
	}
	return false
}

// InfoComplete 表示建单所需的关键信息是否齐备。
func (t Ticket) InfoComplete() bool { return len(t.MissingInfo) == 0 }
