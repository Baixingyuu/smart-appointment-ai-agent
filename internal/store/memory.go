// Package store 提供领域对象的持久化抽象。
//
// 一期使用内存实现：派单与状态机的正确性不依赖数据库，
// 先用内存实现把业务规则跑通并测透，避免过早引入迁移与连接管理。
// 接口刻意保持窄，使后续替换为 SQLite / MySQL 时业务层无需改动。
package store

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/mac/agentdesk/internal/domain"
)

// ErrNotFound 表示目标对象不存在。
var ErrNotFound = errors.New("not found")

// Store 定义持久化能力。
//
// 只暴露业务真正需要的方法：没有通用的「查任意条件」入口，
// 避免业务逻辑通过查询条件泄漏到存储层之外。
type Store interface {
	// 员工
	SaveEmployee(e domain.Employee) error
	GetEmployee(id int64) (domain.Employee, error)
	ListEmployees() []domain.Employee

	// 技能
	SaveSkill(s domain.Skill) error
	ListSkills() []domain.Skill

	// 工单
	SaveTicket(t domain.Ticket) (int64, error)
	GetTicket(id int64) (domain.Ticket, error)
	ListTickets() []domain.Ticket
	// FindOpenTicketsBySkills 返回指定处理人名下未完成的工单，用于负载计算。
	FindOpenTicketsBySkills(assigneeID int64) []domain.Ticket
	// FindOpenTicketsByConversation 返回指定会话下未完成的工单，
	// 用于创建前的去重检查：同一会话已有未关闭工单时不应重复建单。
	FindOpenTicketsByConversation(conversationID int64) []domain.Ticket

	// 指派日志
	AppendAssignmentLog(log domain.AssignmentLog) error
	AssignmentLogsByTicket(ticketID int64) []domain.AssignmentLog

	// 进展
	AppendProgress(p domain.TicketProgress) error
	ProgressByTicket(ticketID int64) []domain.TicketProgress

	// 会话与消息
	SaveConversation(c domain.Conversation) (int64, error)
	GetConversation(id int64) (domain.Conversation, error)
	ListConversations() []domain.Conversation
	// AppendMessage 追加消息，并回填 ID 与创建时间。
	//
	// 接收指针而非值：按值接收时存储层填的 ID/时间不会传回调用方，
	// 表现为「刚写入的消息时间戳为零值」（实测踩过）。
	// RequestID 重复时返回 ErrDuplicateRequest，使重试不产生两条消息。
	AppendMessage(m *domain.Message) error
	MessagesByConversation(conversationID int64) []domain.Message

	// 确认中断
	SaveInterrupt(i domain.Interrupt) (int64, error)
	GetInterrupt(id int64) (domain.Interrupt, error)
	// FindPendingInterrupt 返回会话下最新的待确认中断。
	//
	// 返回最新一条而非全部：用户的下一条消息只应恢复最近一次提问，
	// 同时恢复多条会让一次回复触发多个写操作。
	FindPendingInterrupt(conversationID int64) (domain.Interrupt, bool)
}

// Memory 是 Store 的并发安全内存实现。
type Memory struct {
	mu sync.RWMutex

	employees  map[int64]domain.Employee
	skills     map[int64]domain.Skill
	tickets    map[int64]domain.Ticket
	nextTicket int64

	assignLogs []domain.AssignmentLog
	progress   []domain.TicketProgress
	nextLogID  int64
	nextProgID int64

	interrupts      map[int64]domain.Interrupt
	nextInterruptID int64

	conversations      map[int64]domain.Conversation
	nextConversationID int64
	messages           []domain.Message
	nextMessageID      int64
	// requestIDs 记录已处理的请求幂等键，用于丢弃重复投递。
	requestIDs map[string]struct{}

	// now 允许测试注入固定时钟，使时间相关断言可复现。
	now func() time.Time
}

// NewMemory 构造空的内存存储。
func NewMemory() *Memory {
	return &Memory{
		employees:     make(map[int64]domain.Employee),
		skills:        make(map[int64]domain.Skill),
		tickets:       make(map[int64]domain.Ticket),
		interrupts:    make(map[int64]domain.Interrupt),
		conversations: make(map[int64]domain.Conversation),
		requestIDs:    make(map[string]struct{}),
		now:           time.Now,
	}
}

// SetClock 注入时钟，仅用于测试。
func (m *Memory) SetClock(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
}

func (m *Memory) SaveEmployee(e domain.Employee) error {
	if err := e.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.employees[e.ID] = e
	return nil
}

func (m *Memory) GetEmployee(id int64) (domain.Employee, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.employees[id]
	if !ok {
		return domain.Employee{}, ErrNotFound
	}
	return e, nil
}

func (m *Memory) ListEmployees() []domain.Employee {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ret := make([]domain.Employee, 0, len(m.employees))
	for _, e := range m.employees {
		ret = append(ret, e)
	}
	// 按 ID 升序返回：派单器虽已保证与顺序无关，但稳定输出便于日志比对。
	sort.Slice(ret, func(i, j int) bool { return ret[i].ID < ret[j].ID })
	return ret
}

func (m *Memory) SaveSkill(s domain.Skill) error {
	if err := s.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.skills[s.ID] = s
	return nil
}

func (m *Memory) ListSkills() []domain.Skill {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ret := make([]domain.Skill, 0, len(m.skills))
	for _, s := range m.skills {
		ret = append(ret, s)
	}
	sort.Slice(ret, func(i, j int) bool { return ret[i].ID < ret[j].ID })
	return ret
}

// SaveTicket 保存工单。ID 为 0 时自动分配并返回新 ID。
func (m *Memory) SaveTicket(t domain.Ticket) (int64, error) {
	if t.ID <= 0 {
		m.mu.Lock()
		m.nextTicket++
		t.ID = m.nextTicket
		m.mu.Unlock()
	}
	if err := t.Validate(); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if t.ID > m.nextTicket {
		m.nextTicket = t.ID
	}
	m.tickets[t.ID] = t
	return t.ID, nil
}

func (m *Memory) GetTicket(id int64) (domain.Ticket, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tickets[id]
	if !ok {
		return domain.Ticket{}, ErrNotFound
	}
	return t, nil
}

func (m *Memory) ListTickets() []domain.Ticket {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ret := make([]domain.Ticket, 0, len(m.tickets))
	for _, t := range m.tickets {
		ret = append(ret, t)
	}
	sort.Slice(ret, func(i, j int) bool { return ret[i].ID < ret[j].ID })
	return ret
}

func (m *Memory) FindOpenTicketsBySkills(assigneeID int64) []domain.Ticket {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var ret []domain.Ticket
	for _, t := range m.tickets {
		if t.AssigneeID != assigneeID {
			continue
		}
		if t.Status == domain.TicketStatusDone {
			continue
		}
		ret = append(ret, t)
	}
	sort.Slice(ret, func(i, j int) bool { return ret[i].ID < ret[j].ID })
	return ret
}

func (m *Memory) FindOpenTicketsByConversation(conversationID int64) []domain.Ticket {
	if conversationID <= 0 {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var ret []domain.Ticket
	for _, t := range m.tickets {
		if t.ConversationID != conversationID {
			continue
		}
		if t.Status == domain.TicketStatusDone {
			continue
		}
		ret = append(ret, t)
	}
	sort.Slice(ret, func(i, j int) bool { return ret[i].ID < ret[j].ID })
	return ret
}

func (m *Memory) AppendAssignmentLog(log domain.AssignmentLog) error {
	if log.TicketID <= 0 {
		return errors.New("assignment log requires a ticket id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextLogID++
	log.ID = m.nextLogID
	if log.CreatedAt.IsZero() {
		log.CreatedAt = m.now()
	}
	m.assignLogs = append(m.assignLogs, log)
	return nil
}

func (m *Memory) AssignmentLogsByTicket(ticketID int64) []domain.AssignmentLog {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var ret []domain.AssignmentLog
	for _, item := range m.assignLogs {
		if item.TicketID == ticketID {
			ret = append(ret, item)
		}
	}
	return ret
}

func (m *Memory) AppendProgress(p domain.TicketProgress) error {
	if p.TicketID <= 0 {
		return errors.New("progress requires a ticket id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextProgID++
	p.ID = m.nextProgID
	if p.CreatedAt.IsZero() {
		p.CreatedAt = m.now()
	}
	m.progress = append(m.progress, p)
	return nil
}

func (m *Memory) ProgressByTicket(ticketID int64) []domain.TicketProgress {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var ret []domain.TicketProgress
	for _, item := range m.progress {
		if item.TicketID == ticketID {
			ret = append(ret, item)
		}
	}
	return ret
}

// SaveInterrupt 保存中断。ID 为 0 时自动分配并返回新 ID。
func (m *Memory) SaveInterrupt(i domain.Interrupt) (int64, error) {
	if err := i.Validate(); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	if i.ID <= 0 {
		m.nextInterruptID++
		i.ID = m.nextInterruptID
		i.CreatedAt = now
	} else if existing, ok := m.interrupts[i.ID]; ok {
		// 保留原始创建时间，只推进更新时间，使"何时发起确认"不丢失。
		i.CreatedAt = existing.CreatedAt
	} else if i.CreatedAt.IsZero() {
		i.CreatedAt = now
	}
	i.UpdatedAt = now
	if i.ID > m.nextInterruptID {
		m.nextInterruptID = i.ID
	}
	m.interrupts[i.ID] = i
	return i.ID, nil
}

func (m *Memory) GetInterrupt(id int64) (domain.Interrupt, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	i, ok := m.interrupts[id]
	if !ok {
		return domain.Interrupt{}, ErrNotFound
	}
	return i, nil
}

func (m *Memory) FindPendingInterrupt(conversationID int64) (domain.Interrupt, bool) {
	if conversationID <= 0 {
		return domain.Interrupt{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	var found domain.Interrupt
	var ok bool
	for _, item := range m.interrupts {
		if item.ConversationID != conversationID || item.Status != domain.InterruptPending {
			continue
		}
		// 取最新的一条：同一会话可能先后发起过多次确认。
		if !ok || item.ID > found.ID {
			found, ok = item, true
		}
	}
	return found, ok
}

// ---- 会话与消息 ----

// SaveConversation 保存会话。ID 为 0 时自动分配。
func (m *Memory) SaveConversation(c domain.Conversation) (int64, error) {
	if err := c.Validate(); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	if c.ID <= 0 {
		m.nextConversationID++
		c.ID = m.nextConversationID
		c.CreatedAt = now
	} else if existing, ok := m.conversations[c.ID]; ok {
		// 保留原始创建时间，只推进更新时间。
		c.CreatedAt = existing.CreatedAt
	} else if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	if c.ID > m.nextConversationID {
		m.nextConversationID = c.ID
	}
	m.conversations[c.ID] = c
	return c.ID, nil
}

func (m *Memory) GetConversation(id int64) (domain.Conversation, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.conversations[id]
	if !ok {
		return domain.Conversation{}, ErrNotFound
	}
	return c, nil
}

func (m *Memory) ListConversations() []domain.Conversation {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ret := make([]domain.Conversation, 0, len(m.conversations))
	for _, c := range m.conversations {
		ret = append(ret, c)
	}
	sort.Slice(ret, func(i, j int) bool { return ret[i].ID < ret[j].ID })
	return ret
}

// AppendMessage 追加一条消息。
//
// 幂等：RequestID 非空且已出现过时直接返回 ErrDuplicateRequest。
// 这是防止重复建单的第一道闸——Webhook 重投与客户端重试都会触发重复请求。
func (m *Memory) AppendMessage(msg *domain.Message) error {
	if msg == nil {
		return errors.New("消息不能为空")
	}
	if err := msg.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if msg.RequestID != "" {
		if _, exists := m.requestIDs[msg.RequestID]; exists {
			return domain.ErrDuplicateRequest
		}
		m.requestIDs[msg.RequestID] = struct{}{}
	}

	m.nextMessageID++
	msg.ID = m.nextMessageID
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = m.now()
	}
	m.messages = append(m.messages, *msg)
	return nil
}

func (m *Memory) MessagesByConversation(conversationID int64) []domain.Message {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var ret []domain.Message
	for _, msg := range m.messages {
		if msg.ConversationID == conversationID {
			ret = append(ret, msg)
		}
	}
	// 消息按 ID 升序：ID 单调递增，等价于按时间排序，
	// 但不依赖时间戳（注入时钟下多个消息可能共享同一时刻）。
	sort.Slice(ret, func(i, j int) bool { return ret[i].ID < ret[j].ID })
	return ret
}
