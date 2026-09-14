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

	// now 允许测试注入固定时钟，使时间相关断言可复现。
	now func() time.Time
}

// NewMemory 构造空的内存存储。
func NewMemory() *Memory {
	return &Memory{
		employees: make(map[int64]domain.Employee),
		skills:    make(map[int64]domain.Skill),
		tickets:   make(map[int64]domain.Ticket),
		now:       time.Now,
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
