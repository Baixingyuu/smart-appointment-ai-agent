// Package domain 定义一期核心领域模型。
//
// 设计取舍：模型只承载数据与不变量，不含 HTTP、持久化或工作流逻辑。
// 技能（Skill）即标签，采用层级结构，工单与员工各自持有技能集合，
// 二者的交集是「相关处理经验」的唯一事实来源。
package domain

import (
	"errors"
	"fmt"
	"strings"
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

// Category 工单分类，是技能匹配的主键来源。
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

// Skill 技能标签，层级化。
//
// ParentID 为 0 表示根节点。层级用于业务侧组织技能树
// （如 网络 → 接口 / 性能），匹配时只看叶子集合，不看层级。
type Skill struct {
	ID       int64
	ParentID int64
	Name     string
}

// Validate 校验技能自身的合法性。
func (s Skill) Validate() error {
	if s.ID <= 0 {
		return errors.New("skill id must be positive")
	}
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("skill %d: name is required", s.ID)
	}
	if s.ParentID == s.ID {
		return fmt.Errorf("skill %d: cannot be its own parent", s.ID)
	}
	return nil
}

// Employee 处理人。
//
// SkillIDs 是方案 A（标签即技能）的核心：人工维护的技能集合。
// 三期将由「自动技能画像」从历史工单统计生成并替换人工维护。
type Employee struct {
	ID            int64
	Name          string
	Active        bool
	Skills        SkillSet
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

// Ticket 工单。
//
// 与参考实现的差异：新增 CategoryID 与 Priority。
// 前者是技能匹配的主键（参考实现的工单没有任何分类维度，导致无法匹配），
// 后者用于派单排序与 SLA。
type Ticket struct {
	ID            int64
	Title         string
	Description   string
	Category      Category
	Priority      Priority
	Status        TicketStatus
	RequiredSkill SkillSet // 由 Category + 标签推导出的技能需求向量
	AssigneeID    int64    // 0 表示未指派
	SourceChannel string
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
	OutcomeMatched      AssignmentOutcome = "matched"       // 按技能匹配成功
	OutcomeFallbackPool AssignmentOutcome = "fallback_pool" // 无技能命中，退回待认领池
	OutcomeNoCandidate  AssignmentOutcome = "no_candidate"  // 无任何可用处理人
)

// AssignmentLog 派单审计记录。
//
// 必须落库：它是「可解释」（业务方可复核理由）与「可评测」
// （指派准确率需要金标对比）的共同前提。
type AssignmentLog struct {
	TicketID   int64
	AssigneeID int64
	Outcome    AssignmentOutcome
	Reason     string
	Score      float64
	Candidates []CandidateScore
}

// CandidateScore 记录单个候选人的评分明细。
// 保留分项分数是为了让「为什么选了他」可被逐步复核，而不是只给一个总分。
type CandidateScore struct {
	EmployeeID   int64
	Name         string
	SkillScore   float64
	LoadScore    float64
	RecencyScore float64
	Total        float64
	Filtered     bool
	FilterReason string
}

// Active 表示该候选人是否通过了候选过滤。
func (c CandidateScore) Active() bool { return !c.Filtered }
