// Package domain 定义一期核心领域模型。
//
// 设计取舍：模型只承载数据与不变量，不含 HTTP、持久化或工作流逻辑。
package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Priority 工单优先级。
//
// 由确定性规则计算，不由模型预测：优先级直接影响 SLA 与派单顺序，
// 交给 LLM 会导致不可复现、且难以向业务方解释。
type Priority string

const (
	PriorityP0 Priority = "P0" // 系统瘫痪或核心业务中断
	PriorityP1 Priority = "P1" // 核心功能不可用
	PriorityP2 Priority = "P2" // 部分功能异常
	PriorityP3 Priority = "P3" // 一般问题
)

var AllPriorities = []Priority{PriorityP0, PriorityP1, PriorityP2, PriorityP3}

func (p Priority) Valid() bool {
	for _, item := range AllPriorities {
		if item == p {
			return true
		}
	}
	return false
}

// Weight 返回优先级的排序权重，数值越大越紧急。
func (p Priority) Weight() int {
	switch p {
	case PriorityP0:
		return 4
	case PriorityP1:
		return 3
	case PriorityP2:
		return 2
	case PriorityP3:
		return 1
	default:
		return 0
	}
}

// Category 工单分类。
//
// 决定建单交互的阻塞槽位表（见 ticket/blocking_slots.go）；SLA 与派单顺序由 Priority 承担。
type Category string

const (
	CategoryIncident     Category = "incident"     // 故障
	CategoryConsultation Category = "consultation" // 咨询
	CategoryRequest      Category = "request"      // 服务请求
	CategoryChange       Category = "change"       // 变更请求
)

var AllCategories = []Category{CategoryIncident, CategoryConsultation, CategoryRequest, CategoryChange}

func (c Category) Valid() bool {
	for _, item := range AllCategories {
		if item == c {
			return true
		}
	}
	return false
}

// Employee 处理人。
type Employee struct {
	ID            int64
	Name          string
	Active        bool
	CurrentLoad   int     // 当前进行中的工单数
	MaxConcurrent int     // 最大并发接待数，<=0 视为不限
	Recency       float64 // 最近响应表现，0..1，越大越好
}

// Validate 校验员工数据，并归一化越界的 Recency。
func (e *Employee) Validate() error {
	if e.ID <= 0 {
		return errors.New("employee id must be positive")
	}
	if strings.TrimSpace(e.Name) == "" {
		return fmt.Errorf("employee %d: name is required", e.ID)
	}
	if e.CurrentLoad < 0 {
		return fmt.Errorf("employee %d: current load cannot be negative", e.ID)
	}
	if e.MaxConcurrent < 0 {
		return fmt.Errorf("employee %d: max concurrent cannot be negative", e.ID)
	}
	if e.Recency < 0 {
		e.Recency = 0
	}
	if e.Recency > 1 {
		e.Recency = 1
	}
	return nil
}

// HasCapacity 判断该员工是否还有并发余量。
// MaxConcurrent <= 0 表示不限并发，只要在职即可承接。
func (e Employee) HasCapacity() bool {
	if e.MaxConcurrent <= 0 {
		return true
	}
	return e.CurrentLoad < e.MaxConcurrent
}

// Available 判断该员工是否可被派单：在职且有并发余量。
func (e Employee) Available() bool {
	return e.Active && e.HasCapacity()
}

// LoadRatio 返回负载率 0..1，越接近 1 越忙。
// MaxConcurrent <= 0 时视为不占负载，返回 0。
func (e Employee) LoadRatio() float64 {
	if e.MaxConcurrent <= 0 {
		return 0
	}
	ratio := float64(e.CurrentLoad) / float64(e.MaxConcurrent)
	if ratio < 0 {
		return 0
	}
	if ratio > 1 {
		return 1
	}
	return ratio
}

// TicketStatus 工单状态。
//
// 一期补齐流转规则：agent-desk 参考实现中 in_progress 从不由任何代码
// 路径自动设置，导致「处理中」状态实际不可达。
type TicketStatus string

const (
	TicketStatusPending    TicketStatus = "pending"     // 待处理
	TicketStatusInProgress TicketStatus = "in_progress" // 处理中
	TicketStatusDone       TicketStatus = "done"        // 已处理
)

var AllTicketStatuses = []TicketStatus{TicketStatusPending, TicketStatusInProgress, TicketStatusDone}

// TicketInput 建单输入。
//
// 定义在 domain 而非业务服务包，使 seed 等工具包无需反向依赖业务层，
// 保持依赖方向单向（domain ← 其余所有包）。
type TicketInput struct {
	Title          string
	Description    string
	Category       Category
	Priority       Priority
	ConversationID int64
	SourceChannel  string
	// MissingInfo 由调用方（对话流程）判定后传入，服务不再自行猜测。
	MissingInfo []string
	// Intake 携带"进展驱动追问"的跨轮状态，随确认中断一起落库。
	//
	// 只在启用建单交互时写入；omitempty 之外它对既有链路透明——旧草案没有该字段时
	// 反序列化为零值，等价于"还没追问过"。
	Intake IntakeProgress
}

// Ticket 工单。
//
// 与参考实现的差异：新增 Category 与 Priority。
// 前者让建单交互能按类型选出阻塞槽位（参考实现的工单没有任何分类维度），
// 后者用于派单排序与 SLA。
type Ticket struct {
	ID            int64
	Title         string
	Description   string
	Category      Category
	Priority      Priority
	Status        TicketStatus
	AssigneeID    int64 // 0 表示未指派
	SourceChannel string

	// ConversationID 关联的来源会话，0 表示非会话来源（人工建单）。
	ConversationID int64

	// MissingInfo 记录建单时缺失的关键信息字段名。
	//
	// 信息不全不阻塞建单：真实场景中用户常无法一次说清，卡住不建单会让问题丢失。
	// 缺什么显式记录，供处理人补充。
	MissingInfo []string

	// DedupedFrom 命中重复时的原工单 ID，0 表示非重复。
	// 去重命中走「追加进展」而非新建，避免同一问题产生多张工单。
	DedupedFrom int64
}

// Validate 校验工单数据。
func (t *Ticket) Validate() error {
	if t.ID <= 0 {
		return errors.New("ticket id must be positive")
	}
	if strings.TrimSpace(t.Title) == "" {
		return fmt.Errorf("ticket %d: title is required", t.ID)
	}
	if !t.Category.Valid() {
		return fmt.Errorf("ticket %d: invalid category %q", t.ID, t.Category)
	}
	if !t.Priority.Valid() {
		return fmt.Errorf("ticket %d: invalid priority %q", t.ID, t.Priority)
	}
	return nil
}

// AssignmentOutcome 派单结果类型。用于区分「正常匹配」与各类兜底，
// 使兜底率成为可观测指标。
type AssignmentOutcome string

const (
	OutcomeMatched      AssignmentOutcome = "matched"       // 命中可派单的归属人
	OutcomeFallbackPool AssignmentOutcome = "fallback_pool" // 判弱或无归属命中，退回待认领池
	OutcomeNoCandidate  AssignmentOutcome = "no_candidate"  // 无任何可用处理人
)

// AssignmentLog 派单审计记录。
//
// 必须落库：它是「可解释」（业务方可复核理由）与「可评测」
// （指派准确率需要金标对比）的共同前提。
type AssignmentLog struct {
	// ID 与 CreatedAt 由存储层在落库时填充，派单器本身不感知它们，
	// 以保持派单是纯函数。
	ID         int64
	TicketID   int64
	AssigneeID int64
	Outcome    AssignmentOutcome
	Reason     string
	Score      float64
	// Candidates 保存本次全部候选人的评分明细（含被过滤者）。
	// 不保存明细就只能给出总分，无法复核「是不是因为负载而非归属胜出」。
	Candidates []CandidateScore
	CreatedAt  time.Time
}

// CandidateScore 记录单个候选人的评分明细，对应 Stage 1 的五特征。
// 保留分项分数是为了让「为什么选了他」可被逐步复核，而不是只给一个总分。
type CandidateScore struct {
	EmployeeID     int64
	Name           string
	OwnScore       float64 // 归属命中：该候选人是否负责此服务
	SimScore       float64 // 相似历史工单：谁处理过同类问题
	AvailScore     float64 // 可用性：剩余并发容量
	SeniorityScore float64 // 级别：P0 硬约束之后的软加权
	RecentScore    float64 // 近期表现
	Total          float64
	Filtered       bool
	FilterReason   string
}

// Active 表示该候选人是否通过了候选过滤。
func (c CandidateScore) Active() bool { return !c.Filtered }
