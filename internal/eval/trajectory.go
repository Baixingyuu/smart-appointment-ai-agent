// Package eval 实现评测框架。
//
// 设计约束（红线）：
//  1. 评测是观察者：业务代码不得 import 本包，本包也不写业务表。
//  2. 指标必须可复现：不使用 LLM 作为评判者。
//  3. 报告同时输出 JSON（机读、供跨版本回归对比）与人读文本。
package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// TrajectoryCase 一条轨迹评测样本。
//
// 轨迹评测关心的是「怎么做的」而非「说了什么」：
// 应该调用哪些工具、不该调用哪些、用了几步、花了多少。
type TrajectoryCase struct {
	ID          string `json:"id"`
	Scenario    string `json:"scenario"`
	Description string `json:"description,omitempty"`
	// ConversationID 让同一条用例内的多轮消息共享会话，
	// 从而能覆盖确认中断的恢复路径。
	ConversationID int64    `json:"conversationId"`
	Messages       []string `json:"messages"`
	Expect         Expected `json:"expect"`
}

// Expected 轨迹期望。
//
// 全部字段可选，未声明的维度不参与判定——避免为了填满 schema
// 而写出无意义的期望值。
type Expected struct {
	// Tools 期望被调用的工具集合（无序比较）。
	Tools []string `json:"tools,omitempty"`
	// OrderedTools 期望的工具调用顺序（有序比较，前缀匹配）。
	OrderedTools []string `json:"orderedTools,omitempty"`
	// ForbiddenTools 明确不应被调用的工具。
	//
	// 这是安全维度的断言：例如知识库已充分时不建单、
	// 已有未关闭工单时不重复建单。
	ForbiddenTools []string `json:"forbiddenTools,omitempty"`
	// MinRounds / MaxRounds 模型决策轮次的上下界。
	MinRounds int `json:"minRounds,omitempty"`
	MaxRounds int `json:"maxRounds,omitempty"`
	// ExpectInterrupted 期望最终处于等待确认状态。
	ExpectInterrupted *bool `json:"expectInterrupted,omitempty"`
	// ExpectTicketCreated 期望最终创建了工单。
	ExpectTicketCreated *bool `json:"expectTicketCreated,omitempty"`
	// MaxPromptTokens 单条用例的 prompt token 上限，用于成本回归。
	MaxPromptTokens int `json:"maxPromptTokens,omitempty"`
}

// TrajectoryDataset 轨迹评测集。
type TrajectoryDataset struct {
	Version     int              `json:"version"`
	Description string           `json:"description"`
	Cases       []TrajectoryCase `json:"cases"`
}

// LoadTrajectoryDataset 读取轨迹评测集。
func LoadTrajectoryDataset(path string) (*TrajectoryDataset, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取评测集 %s: %w", path, err)
	}
	var dataset TrajectoryDataset
	if err := json.Unmarshal(data, &dataset); err != nil {
		return nil, fmt.Errorf("解析评测集 %s: %w", path, err)
	}
	if len(dataset.Cases) == 0 {
		return nil, fmt.Errorf("评测集 %s 不含任何样本", path)
	}
	return &dataset, nil
}

// ToolInvocation 一次工具调用的观测记录。
//
// 由 runner 从 Agent 侧转换而来，评测包本身不认识 agent 包的类型，
// 从而保持「业务不依赖评测、评测也不依赖具体业务实现」。
type ToolInvocation struct {
	Code string
	// Status 取值 completed / failed / awaiting_confirmation / rejected。
	Status string
	// ErrorKind 治理拒绝的结构化归因，为空表示未被拒绝。
	ErrorKind string
}

// TurnObservation 一次会话回合的观测结果。
type TurnObservation struct {
	Reply       string
	Interrupted bool
	TicketID    int64
	Tools       []ToolInvocation
	Rounds      int
	Usage       TokenUsage
	DurationMS  int
	// DurationUS 为微秒精度耗时。
	//
	// 离线评测（脚本化模型 + 内存存储）单轮耗时在微秒级，
	// 只报毫秒会让整个延迟指标全是 0，无法区分快慢。
	DurationUS int
}

// TokenUsage token 用量。
type TokenUsage struct {
	PromptTokens     int
	CompletionTokens int
}

// Total 返回总 token 数。
func (u TokenUsage) Total() int { return u.PromptTokens + u.CompletionTokens }

// Runner 执行一条用例的多轮消息并返回逐轮观测。
//
// 抽象为接口而非直接依赖 agent 包：评测框架必须能被独立测试，
// 也避免评测与具体编排实现互相耦合。
type Runner interface {
	RunTurn(conversationID int64, message string) (TurnObservation, error)
}

// RunnerFunc 便于用函数构造 Runner。
type RunnerFunc func(conversationID int64, message string) (TurnObservation, error)

// RunTurn 实现 Runner。
func (f RunnerFunc) RunTurn(conversationID int64, message string) (TurnObservation, error) {
	return f(conversationID, message)
}

// CaseResult 单条用例的评测结果。
type CaseResult struct {
	CaseID   string   `json:"caseId"`
	Scenario string   `json:"scenario"`
	Passed   bool     `json:"passed"`
	Failures []string `json:"failures,omitempty"`

	// 观测值
	CalledTools   []string `json:"calledTools"`
	Rounds        int      `json:"rounds"`
	Interrupted   bool     `json:"interrupted"`
	TicketCreated bool     `json:"ticketCreated"`
	PromptTokens  int      `json:"promptTokens"`
	TotalTokens   int      `json:"totalTokens"`
	// DurationUS 为该用例全部轮次的累计耗时（微秒）。
	DurationUS int `json:"durationUs"`

	// 诊断值
	RejectedCalls int      `json:"rejectedCalls"`
	BudgetRejects int      `json:"budgetRejects"`
	ForbiddenHits []string `json:"forbiddenHits,omitempty"`
	MissingTools  []string `json:"missingTools,omitempty"`
	OrderViolated bool     `json:"orderViolated,omitempty"`
}

// LatencyStat 延迟统计（单位：微秒）。
//
// 用微秒而非毫秒：离线评测的耗时落在微秒量级，毫秒精度下全是 0，
// 指标退化为常量。接入真实模型后数值自然升到数十万微秒，
// 单位不变即可直接比较。
type LatencyStat struct {
	P50 int `json:"p50Us"`
	P95 int `json:"p95Us"`
	Max int `json:"maxUs"`
}

// TrajectoryReport 轨迹评测报告。
type TrajectoryReport struct {
	Total  int `json:"total"`
	Passed int `json:"passed"`
	// PassRate 整体通过率：全部断言（工具、轮次、终态、成本）均满足的比例。
	PassRate float64 `json:"passRate"`
	// ToolAccuracy 工具选择准确率：**仅**统计工具集合是否命中预期。
	//
	// 与 PassRate 分开统计是刻意的：若把工具选择与其他断言混在一个数字里，
	// 就无法判断一次退化究竟是「模型选错了工具」还是「步骤多了一步」，
	// 而这两者的改进方向完全不同。
	ToolAccuracy float64 `json:"toolAccuracy"`
	// OrderAccuracy 工具顺序准确率：声明了 orderedTools 的样本中顺序正确的比例。
	OrderAccuracy float64 `json:"orderAccuracy"`
	// SafetyViolationRate 越界率：调用了 forbiddenTools 中任一工具的比例。
	SafetyViolationRate float64 `json:"safetyViolationRate"`
	// BudgetRejectRate 预算超限率。
	BudgetRejectRate float64 `json:"budgetRejectRate"`
	// AvgRounds / AvgTools 平均轮次与平均工具调用次数。
	AvgRounds float64 `json:"avgRounds"`
	AvgTools  float64 `json:"avgTools"`
	// Latency 端到端延迟分布（按用例计，含全部轮次）。
	Latency LatencyStat `json:"latency"`
	// PromptTokens / TotalTokens 累计用量，用于成本回归。
	PromptTokens int `json:"promptTokens"`
	TotalTokens  int `json:"totalTokens"`
	// ByScenario 按场景聚合，用于定位是哪类场景退化。
	ByScenario map[string]ScenarioStat `json:"byScenario"`

	Results  []CaseResult `json:"results,omitempty"`
	Failures []CaseResult `json:"failures,omitempty"`
}

// Run 执行轨迹评测。
func Run(dataset *TrajectoryDataset, runner Runner) TrajectoryReport {
	report := TrajectoryReport{
		Total:      len(dataset.Cases),
		ByScenario: make(map[string]ScenarioStat),
	}

	var roundsTotal, toolsTotal int
	var orderDeclared, orderCorrect int
	var safetyViolations, budgetRejects int
	var toolDeclared, toolCorrect int
	durations := make([]int, 0, len(dataset.Cases))

	for _, item := range dataset.Cases {
		result := runCase(item, runner)
		if result.Passed {
			report.Passed++
		} else {
			report.Failures = append(report.Failures, result)
		}

		roundsTotal += result.Rounds
		toolsTotal += len(result.CalledTools)
		report.PromptTokens += result.PromptTokens
		report.TotalTokens += result.TotalTokens
		durations = append(durations, result.DurationUS)

		if len(item.Expect.OrderedTools) > 0 {
			orderDeclared++
			if !result.OrderViolated {
				orderCorrect++
			}
		}
		if len(item.Expect.Tools) > 0 {
			toolDeclared++
			if len(result.MissingTools) == 0 && len(result.ForbiddenHits) == 0 {
				toolCorrect++
			}
		}
		if len(result.ForbiddenHits) > 0 {
			safetyViolations++
		}
		budgetRejects += result.BudgetRejects

		stat := report.ByScenario[item.Scenario]
		stat.Total++
		if result.Passed {
			stat.Passed++
		}
		report.ByScenario[item.Scenario] = stat

		report.Results = append(report.Results, result)
	}

	if report.Total > 0 {
		report.PassRate = float64(report.Passed) / float64(report.Total)
		report.SafetyViolationRate = float64(safetyViolations) / float64(report.Total)
		report.BudgetRejectRate = float64(budgetRejects) / float64(report.Total)
		report.AvgRounds = float64(roundsTotal) / float64(report.Total)
		report.AvgTools = float64(toolsTotal) / float64(report.Total)
		report.Latency = latencyStat(durations)
	}
	if toolDeclared > 0 {
		report.ToolAccuracy = float64(toolCorrect) / float64(toolDeclared)
	}
	if orderDeclared > 0 {
		report.OrderAccuracy = float64(orderCorrect) / float64(orderDeclared)
	}
	for name, stat := range report.ByScenario {
		if stat.Total > 0 {
			stat.Rate = float64(stat.Passed) / float64(stat.Total)
			report.ByScenario[name] = stat
		}
	}
	return report
}

// runCase 执行单条用例：按顺序发送全部消息，聚合观测结果。
func runCase(item TrajectoryCase, runner Runner) CaseResult {
	result := CaseResult{CaseID: item.ID, Scenario: item.Scenario}

	var called []string
	var invocations []ToolInvocation
	interrupted := false
	ticketCreated := false

	for _, message := range item.Messages {
		observation, err := runner.RunTurn(item.ConversationID, message)
		if err != nil {
			result.Failures = append(result.Failures, "第 "+itoa(len(result.CalledTools)+1)+" 轮执行失败: "+err.Error())
			result.Passed = false
			return result
		}
		for _, tool := range observation.Tools {
			called = append(called, tool.Code)
			invocations = append(invocations, tool)
		}
		result.Rounds += observation.Rounds
		result.PromptTokens += observation.Usage.PromptTokens
		result.TotalTokens += observation.Usage.Total()
		result.DurationUS += observation.DurationUS
		interrupted = observation.Interrupted
		if observation.TicketID > 0 {
			ticketCreated = true
		}
	}

	result.CalledTools = called
	result.Interrupted = interrupted
	result.TicketCreated = ticketCreated

	// 治理拒绝的统计：区分「预算超限」与其他拒绝原因，
	// 因为二者对应的改进方向完全不同（调预算 vs 改工具设计）。
	for _, tool := range invocations {
		if tool.ErrorKind == "" {
			continue
		}
		result.RejectedCalls++
		if tool.ErrorKind == "budget_exceeded" {
			result.BudgetRejects++
		}
	}

	result.Passed, result.Failures = judgeTrajectory(item.Expect, &result, invocations)
	return result
}

// judgeTrajectory 判定单条轨迹用例是否通过，并给出可读的失败原因。
//
// result 传指针：判定过程会回填诊断字段（MissingTools / ForbiddenHits /
// OrderViolated），若按值传递这些字段会写在副本上而全部丢失，
// 导致报告里永远看不到「到底缺了哪个工具」（实测踩过这个坑）。
func judgeTrajectory(expect Expected, result *CaseResult, invocations []ToolInvocation) (bool, []string) {
	var failures []string
	passed := true
	fail := func(format string, args ...any) {
		passed = false
		failures = append(failures, fmt.Sprintf(format, args...))
	}

	// 工具集合：期望必须被完整命中。
	if len(expect.Tools) > 0 {
		missing := missingTools(expect.Tools, result.CalledTools)
		if len(missing) > 0 {
			result.MissingTools = missing
			fail("期望调用的工具未被调用: %s（实际调用 %v）", strings.Join(missing, ", "), result.CalledTools)
		}
	}

	// 禁用工具：安全维度，命中即失败。
	for _, code := range expect.ForbiddenTools {
		if containsString(result.CalledTools, code) {
			result.ForbiddenHits = append(result.ForbiddenHits, code)
		}
	}
	if len(result.ForbiddenHits) > 0 {
		fail("调用了被禁止的工具: %s", strings.Join(result.ForbiddenHits, ", "))
	}

	// 顺序：按前缀匹配，允许期望序列之后还有额外调用。
	if len(expect.OrderedTools) > 0 && !isSubsequence(expect.OrderedTools, result.CalledTools) {
		result.OrderViolated = true
		fail("工具调用顺序不符：期望 %v，实际 %v", expect.OrderedTools, result.CalledTools)
	}

	// 轮次上下界。
	if expect.MinRounds > 0 && result.Rounds < expect.MinRounds {
		fail("轮次 %d 少于下限 %d", result.Rounds, expect.MinRounds)
	}
	if expect.MaxRounds > 0 && result.Rounds > expect.MaxRounds {
		fail("轮次 %d 超过上限 %d（步骤过多）", result.Rounds, expect.MaxRounds)
	}

	// 终态。
	if expect.ExpectInterrupted != nil && result.Interrupted != *expect.ExpectInterrupted {
		fail("终态中断状态不符：期望 %v，实际 %v", *expect.ExpectInterrupted, result.Interrupted)
	}
	if expect.ExpectTicketCreated != nil && result.TicketCreated != *expect.ExpectTicketCreated {
		fail("工单创建状态不符：期望 %v，实际 %v", *expect.ExpectTicketCreated, result.TicketCreated)
	}

	// 成本回归：token 超限即失败，防止上下文悄悄膨胀。
	if expect.MaxPromptTokens > 0 && result.PromptTokens > expect.MaxPromptTokens {
		fail("prompt token %d 超过上限 %d", result.PromptTokens, expect.MaxPromptTokens)
	}

	return passed, failures
}

// missingTools 返回期望中未被调用的工具。
func missingTools(expected, called []string) []string {
	var ret []string
	for _, code := range expected {
		if !containsString(called, code) {
			ret = append(ret, code)
		}
	}
	return ret
}

// isSubsequence 判断 expected 是否为 actual 的子序列（保持相对顺序即可）。
func isSubsequence(expected, actual []string) bool {
	index := 0
	for _, code := range actual {
		if index < len(expected) && expected[index] == code {
			index++
		}
	}
	return index == len(expected)
}

// latencyStat 计算延迟分位数。
//
// 用最近秩法（nearest-rank）：样本量小的时候，插值会产生
// 「比所有实际观测都快」的虚构值，而这里报告的是真实发生过的耗时。
func latencyStat(durations []int) LatencyStat {
	if len(durations) == 0 {
		return LatencyStat{}
	}
	sorted := append([]int(nil), durations...)
	sort.Ints(sorted)
	return LatencyStat{
		P50: percentile(sorted, 50),
		P95: percentile(sorted, 95),
		Max: sorted[len(sorted)-1],
	}
}

func percentile(sorted []int, p int) int {
	if len(sorted) == 0 {
		return 0
	}
	index := (len(sorted)*p+99)/100 - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func itoa(value int) string { return fmt.Sprintf("%d", value) }

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

// Text 渲染人读报告。
func (r TrajectoryReport) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "轨迹评测报告\n")
	fmt.Fprintf(&b, "%s\n", strings.Repeat("=", 60))
	fmt.Fprintf(&b, "样本总数          %d\n", r.Total)
	fmt.Fprintf(&b, "通过              %d\n", r.Passed)
	fmt.Fprintf(&b, "整体通过率        %.2f%%\n", r.PassRate*100)
	fmt.Fprintf(&b, "工具选择准确率    %.2f%%\n", r.ToolAccuracy*100)
	fmt.Fprintf(&b, "工具顺序准确率    %.2f%%\n", r.OrderAccuracy*100)
	fmt.Fprintf(&b, "安全越界率        %.2f%%\n", r.SafetyViolationRate*100)
	fmt.Fprintf(&b, "预算超限率        %.2f%%\n", r.BudgetRejectRate*100)
	fmt.Fprintf(&b, "平均轮次          %.2f\n", r.AvgRounds)
	fmt.Fprintf(&b, "平均工具调用      %.2f\n", r.AvgTools)
	fmt.Fprintf(&b, "端到端延迟 P50    %d µs\n", r.Latency.P50)
	fmt.Fprintf(&b, "端到端延迟 P95    %d µs\n", r.Latency.P95)
	fmt.Fprintf(&b, "端到端延迟 Max    %d µs\n", r.Latency.Max)
	fmt.Fprintf(&b, "prompt token 合计 %d\n", r.PromptTokens)
	fmt.Fprintf(&b, "token 合计        %d\n", r.TotalTokens)

	if len(r.ByScenario) > 0 {
		fmt.Fprintf(&b, "\n按场景分解\n%s\n", strings.Repeat("-", 60))
		names := make([]string, 0, len(r.ByScenario))
		for name := range r.ByScenario {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			stat := r.ByScenario[name]
			fmt.Fprintf(&b, "%-30s %3d/%3d  %6.2f%%\n", name, stat.Passed, stat.Total, stat.Rate*100)
		}
	}

	if len(r.Failures) > 0 {
		fmt.Fprintf(&b, "\n未通过样本\n%s\n", strings.Repeat("-", 60))
		for i, failure := range r.Failures {
			if i >= 20 {
				fmt.Fprintf(&b, "... 另有 %d 条未列出\n", len(r.Failures)-20)
				break
			}
			fmt.Fprintf(&b, "%s [%s]\n", failure.CaseID, failure.Scenario)
			for _, reason := range failure.Failures {
				fmt.Fprintf(&b, "    - %s\n", reason)
			}
		}
	}
	return b.String()
}
