// Package tooling 实现工具注册与调用治理。
//
// 治理是「模型侧工具」与「业务操作」之间的唯一通道，负责：
//
//	① 白名单  —— 该工具是否对本 Agent 开放
//	② 风险分级 —— read 直接执行，write 必须先经用户确认
//	③ 预算     —— 单轮调用总数与单次参数大小上限
//	④ 归因     —— 拒绝原因结构化，使「工具为什么没执行」可被统计
//
// 与参考实现的差异：它把策略拒绝压成一个字符串错误消息，
// 因而无法统计「越权尝试率」「预算超限率」等评测指标。
package tooling

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// RiskLevel 工具风险等级。
type RiskLevel string

const (
	RiskRead  RiskLevel = "read"  // 只读，无副作用
	RiskWrite RiskLevel = "write" // 有副作用，必须经过确认
)

// ErrorKind 治理拒绝原因。
//
// 结构化而非自由文本：评测需要按原因统计（越权尝试率、预算超限率），
// 若只给一句话描述，就无法区分「模型选错了工具」和「模型调用太多次」。
type ErrorKind string

const (
	KindUnknownTool    ErrorKind = "unknown_tool"    // 工具不存在
	KindNotAllowed     ErrorKind = "not_allowed"     // 不在白名单
	KindBudgetExceeded ErrorKind = "budget_exceeded" // 超出调用次数预算
	KindArgsTooLarge   ErrorKind = "args_too_large"  // 参数体过大
	KindInvalidArgs    ErrorKind = "invalid_args"    // 参数不是合法 JSON 或缺少必填项
	KindNeedsConfirm   ErrorKind = "needs_confirm"   // 写操作需要用户确认
	KindExecFailed     ErrorKind = "exec_failed"     // 执行期错误
)

// ToolError 带归因的工具错误。
type ToolError struct {
	Kind ErrorKind
	Tool string
	Msg  string
}

func (e *ToolError) Error() string {
	return fmt.Sprintf("[%s] 工具 %s: %s", e.Kind, e.Tool, e.Msg)
}

// KindOf 提取错误归因；非工具错误归为 exec_failed。
func KindOf(err error) ErrorKind {
	var toolErr *ToolError
	if errors.As(err, &toolErr) {
		return toolErr.Kind
	}
	return KindExecFailed
}

// Handler 工具执行函数。
//
// 入参是已解析并校验过大小与必填项的 JSON 对象；
// 返回值是给模型看的文本观察结果。
type Handler func(ctx HandlerContext, args map[string]any) (string, error)

// HandlerContext 执行上下文，承载工具需要的运行时信息。
//
// 用结构体而非长参数列表：工具数量增加时不必改签名。
type HandlerContext struct {
	// Ctx 用于支持取消与超时，工具内部的长耗时操作应透传它。
	Ctx context.Context
	// ConversationID 当前会话。
	ConversationID int64
	// ResumeText 仅在确认恢复路径上非空。
	ResumeText string
}

// Definition 一个工具的完整定义。
type Definition struct {
	Code        string
	Description string
	Risk        RiskLevel
	// RequireConfirmation 为 true 时，写操作必须先落确认中断。
	RequireConfirmation bool
	// Parameters 是给模型看的 JSON Schema。
	//
	// 必须是真实 schema：参考实现把兼容别名的参数统一写成
	// {"additionalProperties": true}，模型看不到参数含义只能靠猜，
	// 工具调用准确率因此无法提升。
	Parameters map[string]any
	// Required 必填参数名，用于执行前校验。
	Required []string
	Handler  Handler
}

// Validate 校验工具定义自身是否完整。
func (d Definition) Validate() error {
	if strings.TrimSpace(d.Code) == "" {
		return errors.New("工具必须有 Code")
	}
	if strings.TrimSpace(d.Description) == "" {
		return fmt.Errorf("工具 %s 必须有描述（模型据此决定是否调用）", d.Code)
	}
	if d.Risk != RiskRead && d.Risk != RiskWrite {
		return fmt.Errorf("工具 %s 的风险等级非法: %q", d.Code, d.Risk)
	}
	if d.Handler == nil {
		return fmt.Errorf("工具 %s 缺少执行函数", d.Code)
	}
	// 只读工具不应要求确认，写工具必须要求确认：
	// 允许「写操作但无需确认」等于把副作用交给模型自行决定。
	if d.Risk == RiskWrite && !d.RequireConfirmation {
		return fmt.Errorf("工具 %s 是写操作，必须要求确认", d.Code)
	}
	if d.Risk == RiskRead && d.RequireConfirmation {
		return fmt.Errorf("工具 %s 是只读操作，不应要求确认", d.Code)
	}
	return nil
}

// Policy 治理策略。
type Policy struct {
	// AllowedTools 白名单。为空表示不开放任何工具。
	AllowedTools []string
	// MaxTotalCalls 单轮工具调用总数上限。
	MaxTotalCalls int
	// MaxArgumentBytes 单次调用参数体字节上限。
	MaxArgumentBytes int
	// MaxCallsPerTool 单个工具在同一轮内的调用次数上限。
	MaxCallsPerTool int
}

// DefaultPolicy 返回默认策略。
//
// 取值的取舍：预算过松会让模型陷入反复调用（既慢又贵），
// 过紧则会打断正常的"检索→查重→起草→确认"四步链路。
// 3 次调用恰好覆盖一次完整建单流程，留一次余量给纠错。
func DefaultPolicy() Policy {
	return Policy{
		MaxTotalCalls:    4,
		MaxArgumentBytes: 8 * 1024,
		MaxCallsPerTool:  2,
	}
}

func (p Policy) normalize() Policy {
	def := DefaultPolicy()
	if p.MaxTotalCalls <= 0 {
		p.MaxTotalCalls = def.MaxTotalCalls
	}
	if p.MaxArgumentBytes <= 0 {
		p.MaxArgumentBytes = def.MaxArgumentBytes
	}
	if p.MaxCallsPerTool <= 0 {
		p.MaxCallsPerTool = def.MaxCallsPerTool
	}
	return p
}

// Registry 工具注册表与治理执行器。
type Registry struct {
	definitions map[string]Definition
	order       []string
}

// NewRegistry 构造注册表，注册时即校验定义合法性。
func NewRegistry(definitions ...Definition) (*Registry, error) {
	r := &Registry{definitions: make(map[string]Definition, len(definitions))}
	for _, def := range definitions {
		if err := def.Validate(); err != nil {
			return nil, err
		}
		if _, exists := r.definitions[def.Code]; exists {
			return nil, fmt.Errorf("工具 %s 重复注册", def.Code)
		}
		r.definitions[def.Code] = def
		r.order = append(r.order, def.Code)
	}
	sort.Strings(r.order)
	return r, nil
}

// Definitions 返回全部工具定义，按编码升序。
//
// 顺序稳定很重要：工具列表进入 prompt 前缀，顺序漂移会破坏
// 上游的 prompt 缓存，也会让评测结果不可比对。
func (r *Registry) Definitions() []Definition {
	ret := make([]Definition, 0, len(r.order))
	for _, code := range r.order {
		ret = append(ret, r.definitions[code])
	}
	return ret
}

// Get 按编码取工具定义。
func (r *Registry) Get(code string) (Definition, bool) {
	def, ok := r.definitions[code]
	return def, ok
}

// ConfirmationRequired 判断该工具是否需要用户确认。
func (r *Registry) ConfirmationRequired(code string) bool {
	def, ok := r.definitions[code]
	return ok && def.RequireConfirmation
}

// Invocation 一次治理校验所需的全部信息。
type Invocation struct {
	Code      string
	Arguments string
	Policy    Policy
	// CallCounts 记录本轮已发生的调用次数，键为工具编码。
	CallCounts map[string]int
	// TotalCalls 本轮已发生的调用总数。
	TotalCalls int
	// 说明：此处刻意不含 Confirmed 字段。
	//
	// 「写操作需确认」不作为授权判据，而是由编排层（agent 包）在看到
	// RiskWrite 工具时落确认中断、待用户同意后再执行。授权只判断
	// 「能不能调用」，不判断「流程走到哪一步」——混在一起会让
	// 确认流程失去落点（实测踩过这个坑）。
}

// Authorize 执行治理校验，不通过则返回带归因的 ToolError。
func (r *Registry) Authorize(inv Invocation) (Definition, error) {
	def, ok := r.definitions[inv.Code]
	if !ok {
		return Definition{}, &ToolError{
			Kind: KindUnknownTool, Tool: inv.Code,
			Msg: "不存在该工具，请勿编造工具名",
		}
	}

	policy := inv.Policy.normalize()

	if !containsString(policy.AllowedTools, inv.Code) {
		return def, &ToolError{
			Kind: KindNotAllowed, Tool: inv.Code,
			Msg: "该工具未对本 Agent 开放",
		}
	}
	if inv.TotalCalls >= policy.MaxTotalCalls {
		return def, &ToolError{
			Kind: KindBudgetExceeded, Tool: inv.Code,
			Msg: fmt.Sprintf("本轮工具调用已达上限 %d 次", policy.MaxTotalCalls),
		}
	}
	if inv.CallCounts[inv.Code] >= policy.MaxCallsPerTool {
		return def, &ToolError{
			Kind: KindBudgetExceeded, Tool: inv.Code,
			Msg: fmt.Sprintf("工具 %s 本轮调用已达上限 %d 次", inv.Code, policy.MaxCallsPerTool),
		}
	}
	if len(inv.Arguments) > policy.MaxArgumentBytes {
		return def, &ToolError{
			Kind: KindArgsTooLarge, Tool: inv.Code,
			Msg: fmt.Sprintf("参数体 %d 字节超过上限 %d 字节", len(inv.Arguments), policy.MaxArgumentBytes),
		}
	}
	// 注意：这里刻意不处理「写操作需要确认」。
	//
	// 确认不是授权失败，而是正常工作流的一个分支：调用方应据此落一个
	// 待确认中断，等用户同意后再带 Confirmed=true 重新进入。
	// 若在授权阶段就返回错误，调用方会在创建中断之前短路，
	// 结果是写操作永远无法发起（实测踩过这个坑：所有建单用例都拿不到中断）。
	return def, nil
}

// ParseArguments 解析并校验工具参数。
//
// 空参数视为空对象：无参工具允许模型传空字符串。
func ParseArguments(def Definition, raw string) (map[string]any, error) {
	args := make(map[string]any)
	trimmed := strings.TrimSpace(raw)
	if trimmed != "" {
		if err := json.Unmarshal([]byte(trimmed), &args); err != nil {
			return nil, &ToolError{
				Kind: KindInvalidArgs, Tool: def.Code,
				Msg: "参数不是合法 JSON: " + err.Error(),
			}
		}
	}
	if args == nil {
		args = make(map[string]any)
	}
	for _, field := range def.Required {
		value, ok := args[field]
		if !ok {
			return nil, &ToolError{
				Kind: KindInvalidArgs, Tool: def.Code,
				Msg: fmt.Sprintf("缺少必填参数 %s", field),
			}
		}
		// 显式区分「未提供」与「提供了空字符串」：
		// 后者多半是模型没想清楚就调用，直接拒绝比让业务层收到空值更好。
		if text, isString := value.(string); isString && strings.TrimSpace(text) == "" {
			return nil, &ToolError{
				Kind: KindInvalidArgs, Tool: def.Code,
				Msg: fmt.Sprintf("必填参数 %s 不能为空", field),
			}
		}
	}
	return args, nil
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

// StringArg 读取字符串参数并去除首尾空白。
func StringArg(args map[string]any, key string) string {
	value, ok := args[key]
	if !ok {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

// StringSliceArg 读取字符串数组参数。
func StringSliceArg(args map[string]any, key string) []string {
	value, ok := args[key]
	if !ok {
		return nil
	}
	raw, ok := value.([]any)
	if !ok {
		return nil
	}
	ret := make([]string, 0, len(raw))
	for _, item := range raw {
		if text, ok := item.(string); ok {
			if trimmed := strings.TrimSpace(text); trimmed != "" {
				ret = append(ret, trimmed)
			}
		}
	}
	return ret
}
