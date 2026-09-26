// 归属模型：Service / Team / EmployeeExtension。
//
// 派单的第一因是"谁负责这个业务"，所以这一层不是附加维度，而是派单的数据来源：
// 工单文本 → 服务节点 → 归属人，见 docs/DISPATCH_PIPELINE.md §1.1。
//
// EmployeeExtension 独立于 Employee 而非并入其字段：Employee 描述的是
// 「一个人的负载与容量」这类运行时状态，扩展描述的是「级别与归属关系」这类
// 组织结构，两者的变更频率与数据来源都不同。pipeline 用索引把两者拼起来用。
package domain

import (
	"errors"
	"fmt"
	"strings"
)

// Level 员工级别，用于 P0/P1 硬约束（"P0 不许派给 junior"）。
//
// 只有三级：一期种子数据撑不起更细的分层，且更多层级会引入
// "mid 与 senior 边界怎么定"这种没有客观答案的争论。
type Level string

const (
	LevelJunior  Level = "junior"
	LevelMid     Level = "mid"
	LevelSenior  Level = "senior"
	LevelUnknown Level = "" // 未标注时的零值，校验时视为无效
)

var AllLevels = []Level{LevelJunior, LevelMid, LevelSenior}

func (l Level) Valid() bool {
	for _, item := range AllLevels {
		if item == l {
			return true
		}
	}
	return false
}

// Rank 数值越大级别越高。用于 AtLeast 比较。
// LevelUnknown 返回 -1，保证任何 valid level 都大于它。
func (l Level) Rank() int {
	switch l {
	case LevelJunior:
		return 1
	case LevelMid:
		return 2
	case LevelSenior:
		return 3
	default:
		return -1
	}
}

// AtLeast 判断 l 是否达到 min 级别。未知级别视为不达标。
func (l Level) AtLeast(min Level) bool {
	return l.Rank() >= min.Rank()
}

// Team 处理团队。一期只用作 grouping 与"同 team 弱加分"，不建模权限域。
//
// 权限域是二期议题（多租户 / 跨部门）；一期引入会诱导种子数据出现
// "这个人能看 A 但不能看 B"这种规则，评测无法验证。
type Team struct {
	ID   int64
	Name string
}

func (t Team) Validate() error {
	if t.ID <= 0 {
		return errors.New("team id must be positive")
	}
	if strings.TrimSpace(t.Name) == "" {
		return fmt.Errorf("team %d: name is required", t.ID)
	}
	return nil
}

// Service 服务节点，工单归属的最小单元。
//
// 大致对应 CMDB 的一条"可运维单元"：一个后端服务、一条数据流、一类客户端组件。
// OwnerID 唯一、BackupOwnerID 可空、TeamID 归属团队。这三个字段是 Stage 1 打分的主要输入。
//
// Keywords 用于 BM25 之外的字面兜底匹配（服务别名常出现在工单原文里）。
// 一期不做 embedding：服务字典规模 < 30 条时 BM25 够用，
// 触发条件见 docs/DISPATCH_PIPELINE.md §6。
type Service struct {
	ID            int64
	ParentID      int64 // 0 = 根节点；层级用于展示，匹配只看叶子
	Name          string
	Aliases       []string
	Description   string
	Keywords      []string
	OwnerID       int64
	BackupOwnerID int64 // 0 表示无备份
	TeamID        int64
}

func (s Service) Validate() error {
	if s.ID <= 0 {
		return errors.New("service id must be positive")
	}
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("service %d: name is required", s.ID)
	}
	if s.ParentID == s.ID {
		return fmt.Errorf("service %d: cannot be its own parent", s.ID)
	}
	if s.OwnerID <= 0 {
		return fmt.Errorf("service %d: owner is required (unowned services can't be dispatched by ownership)", s.ID)
	}
	return nil
}

// SearchableText 返回用于 BM25 索引的拼接文本。
// 刻意把 Name / Aliases / Keywords / Description 拼在一起：
// 工单里出现的可能是简称（"下单挂了" vs 服务名"核心下单接口"），
// 只索引 Description 会让这类字面召回失败。
func (s Service) SearchableText() string {
	parts := make([]string, 0, 4+len(s.Aliases)+len(s.Keywords))
	parts = append(parts, s.Name)
	parts = append(parts, s.Aliases...)
	parts = append(parts, s.Keywords...)
	if strings.TrimSpace(s.Description) != "" {
		parts = append(parts, s.Description)
	}
	return strings.Join(parts, " ")
}

// OwnershipKind 员工与服务的归属关系。打分时对应不同权重档：
// owner > backup > team member。未命中任何一档 = 0 分。
type OwnershipKind int

const (
	OwnershipNone   OwnershipKind = iota
	OwnershipTeam                 // 同 team 但非 owner/backup
	OwnershipBackup               // 备份负责人
	OwnershipOwner                // 主负责人
)

// EmployeeExtension 员工扩展属性：Level / OnCall / OwnedServices / TeamID / Profile。
//
// 与 Employee 分列的原因见本文件顶部：组织结构与运行时负载分开，
// pipeline 通过 index 把两者拼起来用。
//
// Profile 是给 Stage 2 LLM 看的自然语言摘要，一期手工写；
// 真实系统里可以从 HR 系统 + 历史工单统计生成。写这段时**必须避免**
// 直接列出服务名 —— 那会让"LLM 是否理解服务归属"变成"LLM 是否做了字符串匹配"，
// 前者是能力，后者是抄答案。
type EmployeeExtension struct {
	EmployeeID    int64
	Level         Level
	OnCall        bool
	TeamID        int64
	OwnedServices map[int64]OwnershipKind // serviceID -> 关系档位
	Profile       string
}

// NewEmployeeExtension 构造扩展记录；OwnedServices 为空 map 而非 nil，
// 让 OwnershipOf 无需 nil 检查。
func NewEmployeeExtension(employeeID int64, level Level, teamID int64) EmployeeExtension {
	return EmployeeExtension{
		EmployeeID:    employeeID,
		Level:         level,
		TeamID:        teamID,
		OwnedServices: map[int64]OwnershipKind{},
	}
}

// DirectOwnership 返回该员工对某服务的直接归属档位（owner / backup / none）。
// 不含 team 匹配 —— team 归属由 InTeam 单独判定，
// 因为「这个员工是不是这个服务的 team member」需要拿到 Service 本身。
func (e EmployeeExtension) DirectOwnership(serviceID int64) OwnershipKind {
	if e.OwnedServices == nil {
		return OwnershipNone
	}
	if kind, ok := e.OwnedServices[serviceID]; ok {
		return kind
	}
	return OwnershipNone
}

// InTeam 判断该员工是否属于给定服务的团队。TeamID 任一侧为 0 时返回 false，
// 因为 0 表示"未归属团队"，不该被当作一个匹配条件。
func (e EmployeeExtension) InTeam(service Service) bool {
	return e.TeamID != 0 && service.TeamID != 0 && e.TeamID == service.TeamID
}

// GrantOwnership 设置某服务的归属档位。返回自身便于链式调用。
// 用于 seed 与测试 fixture 里显式声明归属，避免每次构造 map 字面量。
func (e *EmployeeExtension) GrantOwnership(serviceID int64, kind OwnershipKind) *EmployeeExtension {
	if e.OwnedServices == nil {
		e.OwnedServices = map[int64]OwnershipKind{}
	}
	if kind == OwnershipNone {
		delete(e.OwnedServices, serviceID)
	} else {
		e.OwnedServices[serviceID] = kind
	}
	return e
}

// Validate 校验扩展记录的不变量。
//
// 关键不变量：OwnedServices 里出现 OwnershipTeam 是错的 —— team 归属由
// TeamID + Service.TeamID 匹配得出，不该显式登记在服务映射里。
func (e EmployeeExtension) Validate() error {
	if e.EmployeeID <= 0 {
		return errors.New("employee extension: employee id must be positive")
	}
	if !e.Level.Valid() {
		return fmt.Errorf("employee extension %d: invalid level %q", e.EmployeeID, e.Level)
	}
	for serviceID, kind := range e.OwnedServices {
		if serviceID <= 0 {
			return fmt.Errorf("employee extension %d: service id %d must be positive", e.EmployeeID, serviceID)
		}
		if kind == OwnershipTeam {
			return fmt.Errorf("employee extension %d: OwnershipTeam on service %d is derived from TeamID, don't set it explicitly", e.EmployeeID, serviceID)
		}
	}
	return nil
}
