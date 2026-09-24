// Package ticket 实现工单业务编排。
//
// 职责边界：本包持有业务规则与状态流转，存储与派单算法通过依赖注入进入。
// 工单的每一次变更（创建、指派、接单、升级、完成）都必须落一条进展记录，
// 否则「谁在什么时候改了什么」不可审计。
package ticket

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mac/helpdesk-agent/internal/assign"
	"github.com/mac/helpdesk-agent/internal/domain"
	"github.com/mac/helpdesk-agent/internal/store"
)

// Service 工单业务服务。
//
// dispatcher 与 directory 是"派单"这条子流程的两个可替换点：
//   - dispatcher：一期默认 legacyAssigner；接进三段流水线时显式换成 pipelineDispatcher
//   - directory：默认只提供 Employees；pipeline 需要服务字典与员工扩展时用 WithDirectoryProvider
type Service struct {
	store      store.Store
	dispatcher Dispatcher
	directory  DirectoryProvider
	now        func() time.Time
}

// workflow 是包级的无状态工作流校验器。
//
// 用包级变量而非每次构造字面量：后者写在 if 语句里需要额外括号
// （`if err := (domain.TicketWorkflow{}).CanAccept(t); ...`），可读性差。
var workflow = domain.TicketWorkflow{}

// Option 调整服务行为。
type Option func(*Service)

// WithClock 注入时钟，用于测试中固定时间。
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// WithDirectoryProvider 覆盖默认的 DirectoryProvider。
// pipeline 路径必须注入 provider 才能拿到 Services / Extensions；
// 不注入时 pipeline 会因为 Services 空判 WeaknessNoServiceMatch，
// 若无 Stage 2 chooser 就落 Stage 3。这不是 bug，是"没接数据"的正确反应。
func WithDirectoryProvider(p DirectoryProvider) Option {
	return func(s *Service) {
		if p != nil {
			s.directory = p
		}
	}
}

// New 构造工单服务，使用旧的 *assign.Assigner 派单。
//
// 保留这个入口是为了让既有 API / 测试 / eval 零改动。接进 pipeline 请显式用 NewWith。
func New(st store.Store, assigner *assign.Assigner, opts ...Option) *Service {
	return NewWith(st, NewLegacyDispatcher(assigner), opts...)
}

// NewWith 用任意 Dispatcher 构造服务。pipeline 走这个入口。
//
// dispatcher 为 nil 时回退到 legacy 默认 Assigner，与 New(st, nil) 行为一致。
// 这样调用方拿不到 dispatcher 时的兜底是"回到一期上线时的行为"，不是 crash。
func NewWith(st store.Store, dispatcher Dispatcher, opts ...Option) *Service {
	if dispatcher == nil {
		dispatcher = NewLegacyDispatcher(nil)
	}
	svc := &Service{
		store:      st,
		dispatcher: dispatcher,
		directory:  emptyDirectoryProvider(),
		now:        time.Now,
	}
	for _, opt := range opts {
		opt(svc)
	}
	return svc
}

// CreateResult 建单结果。
//
// Deduped 为 true 时表示命中未关闭的同类工单，此时不新建，
// 而是向原工单追加一条进展，返回的 Ticket 是原工单。
type CreateResult struct {
	Ticket     domain.Ticket
	Deduped    bool
	Assignment domain.AssignmentLog
}

// Create 创建工单并按技能派单。
//
// 去重规则：同一会话已存在未关闭工单时，不重复建单，改为追加进展。
// 真实场景中用户常就同一问题反复追问，若每次都建单会产生大量重复工单，
// 也会让处理人看到多张内容相近的待办。
func (s *Service) Create(input domain.TicketInput) (CreateResult, error) {
	input.Title = strings.TrimSpace(input.Title)
	input.Description = strings.TrimSpace(input.Description)
	if input.Title == "" {
		return CreateResult{}, errors.New("工单标题不能为空")
	}
	if !input.Category.Valid() {
		return CreateResult{}, fmt.Errorf("无效的工单分类: %q", input.Category)
	}
	if !input.Priority.Valid() {
		return CreateResult{}, fmt.Errorf("无效的优先级: %q", input.Priority)
	}

	if existing, ok := s.findOpenTicketInConversation(input.ConversationID); ok {
		if err := s.appendProgress(existing.ID, domain.ProgressDedupe,
			fmt.Sprintf("命中未关闭工单 T%d，追加进展而非新建：%s", existing.ID, input.Title), 0); err != nil {
			return CreateResult{}, err
		}
		return CreateResult{Ticket: existing, Deduped: true}, nil
	}

	t := domain.Ticket{
		Title:          input.Title,
		Description:    input.Description,
		Category:       input.Category,
		Priority:       input.Priority,
		Status:         domain.TicketStatusPending,
		RequiredSkill:  input.RequiredSkill,
		ConversationID: input.ConversationID,
		SourceChannel:  input.SourceChannel,
	}
	t.AddMissingInfo(input.MissingInfo...)

	id, err := s.store.SaveTicket(t)
	if err != nil {
		return CreateResult{}, err
	}
	t.ID = id

	detail := fmt.Sprintf("工单已创建：%s（%s/%s）", t.Title, t.Category, t.Priority)
	if !t.InfoComplete() {
		// 信息不全不阻塞建单，但必须显式记录缺什么。
		detail += fmt.Sprintf("；缺失信息：%s", strings.Join(t.MissingInfo, ", "))
	}
	if err := s.appendProgress(id, domain.ProgressCreated, detail, 0); err != nil {
		return CreateResult{}, err
	}

	log, err := s.AssignToBest(id, 0)
	if err != nil {
		return CreateResult{}, err
	}
	saved, err := s.store.GetTicket(id)
	if err != nil {
		return CreateResult{}, err
	}
	return CreateResult{Ticket: saved, Assignment: log}, nil
}

// AssignToBest 为工单选择最合适的处理人并落库。
//
// excludeEmployeeID 用于升级场景：排除当前处理人，避免把工单又派回给他。
//
// ctx 这里固定用 context.Background()：Store 层不感知 ctx，Service 方法签名也没接；
// 若未来把 request ctx 打通到 Service，Stage 2 的 LLM 调用才能真正被上游取消。
// 记在 docs/DISPATCH_PIPELINE.md §5 的"未接线事项"里。
func (s *Service) AssignToBest(ticketID, excludeEmployeeID int64) (domain.AssignmentLog, error) {
	t, err := s.store.GetTicket(ticketID)
	if err != nil {
		return domain.AssignmentLog{}, err
	}
	if err := workflow.CanAssign(t); err != nil {
		return domain.AssignmentLog{}, err
	}

	candidates := s.candidates(excludeEmployeeID)
	dir := s.directory.Provide(candidates)
	log, err := s.dispatcher.Dispatch(context.Background(), t, dir)
	if err != nil {
		return domain.AssignmentLog{}, err
	}
	log.TicketID = ticketID

	if err := s.store.AppendAssignmentLog(log); err != nil {
		return domain.AssignmentLog{}, err
	}

	if log.AssigneeID > 0 {
		t.AssigneeID = log.AssigneeID
		if _, err := s.store.SaveTicket(t); err != nil {
			return domain.AssignmentLog{}, err
		}
	}

	content := log.Reason
	if log.Outcome == domain.OutcomeNoCandidate {
		content = "指派失败：" + log.Reason
	} else if log.Outcome == domain.OutcomeFallbackPool {
		content = "退回待认领池：" + log.Reason
	}
	if err := s.appendProgress(ticketID, domain.ProgressAssigned, content, 0); err != nil {
		return domain.AssignmentLog{}, err
	}
	return log, nil
}

// Accept 处理人接单，工单进入处理中。
//
// 参考实现中该状态从不由代码路径自动设置，导致「处理中」实际不可达；
// 此处要求接单必须显式触发流转。
func (s *Service) Accept(ticketID, employeeID int64) (domain.Ticket, error) {
	t, err := s.store.GetTicket(ticketID)
	if err != nil {
		return domain.Ticket{}, err
	}
	if err := workflow.CanAccept(t); err != nil {
		return domain.Ticket{}, err
	}
	if t.AssigneeID != employeeID {
		return domain.Ticket{}, fmt.Errorf("工单 T%d 指派给员工 %d，员工 %d 无权接单",
			ticketID, t.AssigneeID, employeeID)
	}

	t.Status = domain.TicketStatusInProgress
	if _, err := s.store.SaveTicket(t); err != nil {
		return domain.Ticket{}, err
	}
	if err := s.appendProgress(ticketID, domain.ProgressAccepted, "处理人已接单，进入处理中", employeeID); err != nil {
		return domain.Ticket{}, err
	}
	return t, nil
}

// Resolve 标记工单完成。
func (s *Service) Resolve(ticketID, employeeID int64) (domain.Ticket, error) {
	t, err := s.store.GetTicket(ticketID)
	if err != nil {
		return domain.Ticket{}, err
	}
	if err := workflow.CanResolve(t); err != nil {
		return domain.Ticket{}, err
	}
	t.Status = domain.TicketStatusDone
	if _, err := s.store.SaveTicket(t); err != nil {
		return domain.Ticket{}, err
	}
	if err := s.appendProgress(ticketID, domain.ProgressResolved, "工单已处理完成", employeeID); err != nil {
		return domain.Ticket{}, err
	}
	return t, nil
}

// Escalate 升级工单：排除当前处理人后重新派单。
//
// 真实场景里升级是必要的——首轮派单可能选错人，或问题超出该处理人的能力范围。
// 若不排除原处理人，派单器很可能因为技能匹配度最高而再次选中他。
func (s *Service) Escalate(ticketID int64, reason string) (domain.Ticket, domain.AssignmentLog, error) {
	t, err := s.store.GetTicket(ticketID)
	if err != nil {
		return domain.Ticket{}, domain.AssignmentLog{}, err
	}
	if err := workflow.CanEscalate(t); err != nil {
		return domain.Ticket{}, domain.AssignmentLog{}, err
	}

	previous := t.AssigneeID
	if strings.TrimSpace(reason) == "" {
		reason = "未提供升级原因"
	}
	detail := fmt.Sprintf("工单升级，原处理人 %d 已排除；原因：%s", previous, reason)
	if err := s.appendProgress(ticketID, domain.ProgressEscalated, detail, 0); err != nil {
		return domain.Ticket{}, domain.AssignmentLog{}, err
	}

	// 清理当前指派，回到待处理，再重新派单。
	t.AssigneeID = 0
	t.Status = domain.TicketStatusPending
	if _, err := s.store.SaveTicket(t); err != nil {
		return domain.Ticket{}, domain.AssignmentLog{}, err
	}

	log, err := s.AssignToBest(ticketID, previous)
	if err != nil {
		return domain.Ticket{}, domain.AssignmentLog{}, err
	}
	updated, err := s.store.GetTicket(ticketID)
	if err != nil {
		return domain.Ticket{}, domain.AssignmentLog{}, err
	}
	return updated, log, nil
}

// AddComment 追加人工备注。
func (s *Service) AddComment(ticketID, authorID int64, content string) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return errors.New("备注内容不能为空")
	}
	t, err := s.store.GetTicket(ticketID)
	if err != nil {
		return err
	}
	if err := workflow.CanAttachProgress(t); err != nil {
		return err
	}
	return s.appendProgress(ticketID, domain.ProgressComment, content, authorID)
}

// TicketDetail 工单详情，含指派历史与进展。
type TicketDetail struct {
	Ticket      domain.Ticket
	Assignee    *domain.Employee
	Assignments []domain.AssignmentLog
	Progress    []domain.TicketProgress
}

// Timeline 返回工单时间线：指派历史与进展记录合并后按时间排序。
type TimelineEntry struct {
	At      time.Time
	Kind    string
	Content string
}

// Detail 读取工单详情。
func (s *Service) Detail(ticketID int64) (TicketDetail, error) {
	t, err := s.store.GetTicket(ticketID)
	if err != nil {
		return TicketDetail{}, err
	}
	detail := TicketDetail{
		Ticket:      t,
		Assignments: s.store.AssignmentLogsByTicket(ticketID),
		Progress:    s.store.ProgressByTicket(ticketID),
	}
	if t.AssigneeID > 0 {
		if emp, err := s.store.GetEmployee(t.AssigneeID); err == nil {
			detail.Assignee = &emp
		}
	}
	return detail, nil
}

// candidates 加载候选员工，并用未完成工单数重算实时负载。
//
// 负载不直接采信 Employee.CurrentLoad：那是静态快照，会随时间失真。
// 以工单表的实际未完成数量为准，保证派单依据是当下的真实负载。
func (s *Service) candidates(excludeEmployeeID int64) []domain.Employee {
	employees := s.store.ListEmployees()
	ret := make([]domain.Employee, 0, len(employees))
	for _, emp := range employees {
		if emp.ID == excludeEmployeeID {
			// 仍保留在候选列表中并标记为过滤，使「因升级被排除」有据可查。
			emp.Active = false
		}
		emp.CurrentLoad = len(s.store.FindOpenTicketsBySkills(emp.ID))
		ret = append(ret, emp)
	}
	return ret
}

func (s *Service) findOpenTicketInConversation(conversationID int64) (domain.Ticket, bool) {
	if conversationID <= 0 {
		return domain.Ticket{}, false
	}
	items := s.store.FindOpenTicketsByConversation(conversationID)
	if len(items) == 0 {
		return domain.Ticket{}, false
	}
	return items[0], true
}

func (s *Service) appendProgress(ticketID int64, kind domain.ProgressKind, content string, authorID int64) error {
	return s.store.AppendProgress(domain.TicketProgress{
		TicketID:  ticketID,
		Kind:      kind,
		Content:   content,
		AuthorID:  authorID,
		CreatedAt: s.now(),
	})
}
