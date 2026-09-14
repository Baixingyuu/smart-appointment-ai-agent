// Package eval 实现一期评测框架。
//
// 设计约束（沿用设计文档中的红线）：
//  1. 评测是观察者：业务代码不得 import 本包，本包也不写业务表。
//  2. 指标必须可复现：不使用 LLM 作为评判者。
//  3. 报告同时输出 JSON（机读、供跨版本回归对比）与文本（人读）。
package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/mac/agentdesk/internal/assign"
	"github.com/mac/agentdesk/internal/domain"
)

// AssignmentCase 一条指派评测样本。
type AssignmentCase struct {
	ID        string             `json:"id"`
	Scenario  string             `json:"scenario"`
	Ticket    AssignmentTicket   `json:"ticket"`
	Employees []AssignmentWorker `json:"employees"`
	Expect    AssignmentExpect   `json:"expect"`
}

// AssignmentTicket 样本中的工单。
type AssignmentTicket struct {
	Title            string  `json:"title"`
	Category         string  `json:"category"`
	Priority         string  `json:"priority"`
	RequiredSkillIDs []int64 `json:"requiredSkillIds"`
}

// AssignmentWorker 样本中的候选处理人。
type AssignmentWorker struct {
	ID            int64   `json:"id"`
	Name          string  `json:"name"`
	Active        bool    `json:"active"`
	CurrentLoad   int     `json:"currentLoad"`
	MaxConcurrent int     `json:"maxConcurrent"`
	Recency       float64 `json:"recency"`
	SkillIDs      []int64 `json:"skillIds"`
}

// AssignmentExpect 金标期望。
type AssignmentExpect struct {
	GoldAssigneeID int64  `json:"goldAssigneeId"`
	NoMatch        bool   `json:"noMatch"`
	Rationale      string `json:"rationale"`
}

// AssignmentDataset 指派评测集。
type AssignmentDataset struct {
	Version     int              `json:"version"`
	Description string           `json:"description"`
	Cases       []AssignmentCase `json:"cases"`
}

// LoadAssignmentDataset 从 JSON 文件读取指派评测集。
func LoadAssignmentDataset(path string) (*AssignmentDataset, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read dataset %s: %w", path, err)
	}
	var dataset AssignmentDataset
	if err := json.Unmarshal(data, &dataset); err != nil {
		return nil, fmt.Errorf("parse dataset %s: %w", path, err)
	}
	if len(dataset.Cases) == 0 {
		return nil, fmt.Errorf("dataset %s contains no cases", path)
	}
	return &dataset, nil
}

// AssignmentCaseResult 单条样本的执行结果。
type AssignmentCaseResult struct {
	CaseID     string `json:"caseId"`
	Scenario   string `json:"scenario"`
	Passed     bool   `json:"passed"`
	Outcome    string `json:"outcome"`
	GotID      int64  `json:"gotAssigneeId"`
	GoldID     int64  `json:"goldAssigneeId"`
	Reason     string `json:"reason"`
	FailureMsg string `json:"failure,omitempty"`
}

// AssignmentReport 指派评测报告。
type AssignmentReport struct {
	Total        int                     `json:"total"`
	Passed       int                     `json:"passed"`
	Top1Accuracy float64                 `json:"top1Accuracy"`
	MRR          float64                 `json:"mrr"`
	NoMatchRate  float64                 `json:"noMatchRate"`
	FallbackRate float64                 `json:"fallbackRate"`
	ByScenario   map[string]ScenarioStat `json:"byScenario"`
	Failures     []AssignmentCaseResult  `json:"failures,omitempty"`
	Results      []AssignmentCaseResult  `json:"results,omitempty"`
}

// ScenarioStat 按场景聚合的通过率，用于定位是「基础匹配」还是
// 「过滤/平局」等特定环节出了问题。
type ScenarioStat struct {
	Total  int     `json:"total"`
	Passed int     `json:"passed"`
	Rate   float64 `json:"rate"`
}

// RunAssignmentEval 用给定派单器跑指派评测集。
//
// 指标定义：
//   - Top1Accuracy：首位命中金标的比例（noMatch 样本命中 noMatch 亦计入命中）
//   - MRR：命中样本中金标排序的倒数均值；一期派单器只产出单一结果，
//     故命中即 1、未命中即 0，保留该指标是为了二期引入候选排序后可比。
//   - NoMatchRate：系统判定「无可用处理人」的比例
//   - FallbackRate：系统判定「无技能命中，退回待认领池」的比例
func RunAssignmentEval(dataset *AssignmentDataset, assigner *assign.Assigner) AssignmentReport {
	report := AssignmentReport{
		Total:      len(dataset.Cases),
		ByScenario: make(map[string]ScenarioStat),
		Results:    make([]AssignmentCaseResult, 0, len(dataset.Cases)),
	}

	var mrrSum float64
	var noMatch, fallback int

	for _, item := range dataset.Cases {
		log := assigner.Assign(toTicket(item), toEmployees(item.Employees))

		result := AssignmentCaseResult{
			CaseID:   item.ID,
			Scenario: item.Scenario,
			Outcome:  string(log.Outcome),
			GotID:    log.AssigneeID,
			GoldID:   item.Expect.GoldAssigneeID,
			Reason:   log.Reason,
		}

		switch log.Outcome {
		case domain.OutcomeNoCandidate:
			noMatch++
		case domain.OutcomeFallbackPool:
			fallback++
		}

		result.Passed, result.FailureMsg = judge(item, log)
		if result.Passed {
			report.Passed++
			mrrSum++
		} else {
			report.Failures = append(report.Failures, result)
		}

		stat := report.ByScenario[item.Scenario]
		stat.Total++
		if result.Passed {
			stat.Passed++
		}
		report.ByScenario[item.Scenario] = stat

		report.Results = append(report.Results, result)
	}

	if report.Total > 0 {
		report.Top1Accuracy = float64(report.Passed) / float64(report.Total)
		report.MRR = mrrSum / float64(report.Total)
		report.NoMatchRate = float64(noMatch) / float64(report.Total)
		report.FallbackRate = float64(fallback) / float64(report.Total)
	}
	for name, stat := range report.ByScenario {
		if stat.Total > 0 {
			stat.Rate = float64(stat.Passed) / float64(stat.Total)
			report.ByScenario[name] = stat
		}
	}
	return report
}

// judge 判定单条样本是否通过。
func judge(item AssignmentCase, log domain.AssignmentLog) (bool, string) {
	if item.Expect.NoMatch {
		// 金标为「无人可用」：系统必须没有指派任何人。
		if log.AssigneeID != 0 {
			return false, fmt.Sprintf("金标为无匹配，但系统指派了员工 %d（%s）", log.AssigneeID, log.Outcome)
		}
		return true, ""
	}
	if log.Outcome == domain.OutcomeNoCandidate {
		return false, fmt.Sprintf("系统判定无可用处理人，但金标为员工 %d", item.Expect.GoldAssigneeID)
	}
	if log.AssigneeID != item.Expect.GoldAssigneeID {
		return false, fmt.Sprintf("期望员工 %d，实际 %d（%s）", item.Expect.GoldAssigneeID, log.AssigneeID, log.Outcome)
	}
	return true, ""
}

func toTicket(item AssignmentCase) domain.Ticket {
	return domain.Ticket{
		ID:            1,
		Title:         item.Ticket.Title,
		Category:      domain.Category(strings.TrimSpace(item.Ticket.Category)),
		Priority:      domain.Priority(strings.TrimSpace(item.Ticket.Priority)),
		Status:        domain.TicketStatusPending,
		RequiredSkill: domain.NewSkillSet(item.Ticket.RequiredSkillIDs...),
	}
}

func toEmployees(items []AssignmentWorker) []domain.Employee {
	ret := make([]domain.Employee, 0, len(items))
	for _, item := range items {
		ret = append(ret, domain.Employee{
			ID:            item.ID,
			Name:          item.Name,
			Active:        item.Active,
			Skills:        domain.NewSkillSet(item.SkillIDs...),
			CurrentLoad:   item.CurrentLoad,
			MaxConcurrent: item.MaxConcurrent,
			Recency:       item.Recency,
		})
	}
	return ret
}

// DatasetReport 单个数据集的结果。
type DatasetReport struct {
	Name   string           `json:"name"`
	Path   string           `json:"path"`
	Report AssignmentReport `json:"report"`
}

// SuiteReport 一次评测运行覆盖的全部数据集。
//
// 保留数据集粒度的明细，同时给出汇总，便于跨版本做整体回归对比。
type SuiteReport struct {
	Total        int             `json:"total"`
	Passed       int             `json:"passed"`
	Top1Accuracy float64         `json:"top1Accuracy"`
	Datasets     []DatasetReport `json:"datasets"`
}

// Summarize 依据各数据集结果计算汇总指标。
func (s *SuiteReport) Summarize() {
	s.Total, s.Passed = 0, 0
	for _, item := range s.Datasets {
		s.Total += item.Report.Total
		s.Passed += item.Report.Passed
	}
	if s.Total > 0 {
		s.Top1Accuracy = float64(s.Passed) / float64(s.Total)
	}
}

// Text 渲染整套评测的人读报告。
func (s SuiteReport) Text() string {
	var b strings.Builder
	for _, item := range s.Datasets {
		fmt.Fprintf(&b, "【%s】\n", item.Name)
		fmt.Fprint(&b, item.Report.Text())
		fmt.Fprintln(&b)
	}
	fmt.Fprintf(&b, "汇总：%d/%d 通过（%.2f%%）\n", s.Passed, s.Total, s.Top1Accuracy*100)
	fmt.Fprintf(&b, "\n注意：两份数据集的金标均由派单规则推导，因此该数字是\n")
	fmt.Fprintf(&b, "「实现与规格一致性」而非真实场景准确率。真实准确率需要\n")
	fmt.Fprintf(&b, "「工单 → 人工实际指派」的历史数据（见 PHASE_ROADMAP 三期）。\n")
	return b.String()
}

// Text 渲染单个数据集的人读报告。
func (r AssignmentReport) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "指派评测报告\n")
	fmt.Fprintf(&b, "%s\n", strings.Repeat("=", 52))
	fmt.Fprintf(&b, "样本总数      %d\n", r.Total)
	fmt.Fprintf(&b, "通过          %d\n", r.Passed)
	fmt.Fprintf(&b, "Top-1 准确率  %.2f%%\n", r.Top1Accuracy*100)
	fmt.Fprintf(&b, "MRR           %.4f\n", r.MRR)
	fmt.Fprintf(&b, "无可用处理人率 %.2f%%\n", r.NoMatchRate*100)
	fmt.Fprintf(&b, "退回待认领池率 %.2f%%\n", r.FallbackRate*100)

	if len(r.ByScenario) > 0 {
		fmt.Fprintf(&b, "\n按场景分解\n%s\n", strings.Repeat("-", 52))
		names := make([]string, 0, len(r.ByScenario))
		for name := range r.ByScenario {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			stat := r.ByScenario[name]
			fmt.Fprintf(&b, "%-26s %3d/%3d  %6.2f%%\n", name, stat.Passed, stat.Total, stat.Rate*100)
		}
	}

	if len(r.Failures) > 0 {
		fmt.Fprintf(&b, "\n未通过样本（最多列出 20 条）\n%s\n", strings.Repeat("-", 52))
		for i, failure := range r.Failures {
			if i >= 20 {
				fmt.Fprintf(&b, "... 另有 %d 条未列出\n", len(r.Failures)-20)
				break
			}
			fmt.Fprintf(&b, "%s [%s] %s\n", failure.CaseID, failure.Scenario, failure.FailureMsg)
		}
	}
	return b.String()
}
