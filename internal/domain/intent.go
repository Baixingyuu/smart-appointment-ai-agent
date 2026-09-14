package domain

import (
	"fmt"
	"strings"
)

// Intent 用户消息的意图。
//
// 与 Category（工单分类）的区别：Category 描述「工单属于哪类问题」，
// 在问题已被判定需要建单之后才确定；Intent 描述「用户此刻想要什么」，
// 在收到消息的第一时间就要判定，用于决定走哪条处理路径。
//
// 两者不可混用：一句「接口报错」的 Intent 是 knowledge（在问原因）
// 或 incident（在报故障），而无论哪种，若最终建单其 Category 都是 incident。
type Intent string

const (
	// IntentChitchat 寒暄、致谢、告别、与技术无关的闲聊。
	IntentChitchat Intent = "chitchat"
	// IntentKnowledge 询问功能、用法、排查步骤，期望知识库作答。
	IntentKnowledge Intent = "knowledge"
	// IntentIncident 报告故障或明确要求登记问题。
	IntentIncident Intent = "incident"
	// IntentHandoff 明确要求转人工或找真人客服。
	IntentHandoff Intent = "handoff"
	// IntentOutOfScope 与技术支持完全无关的请求。
	IntentOutOfScope Intent = "out_of_scope"
)

// AllIntents 返回全部意图，顺序固定以保证报告与混淆矩阵稳定。
func AllIntents() []Intent {
	return []Intent{
		IntentChitchat,
		IntentKnowledge,
		IntentIncident,
		IntentHandoff,
		IntentOutOfScope,
	}
}

// Valid 判断意图是否在已知取值内。
func (i Intent) Valid() bool {
	for _, item := range AllIntents() {
		if item == i {
			return true
		}
	}
	return false
}

// ParseIntent 把模型输出的字符串解析为意图。
//
// 容错是必要的：模型常返回 "Knowledge"、"knowledge."、
// 甚至带解释的 "knowledge（在问原因）"。直接严格匹配会让大量
// 本可用的结果被判为解析失败，使准确率指标失真。
func ParseIntent(raw string) (Intent, bool) {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "" {
		return "", false
	}
	// 去掉常见的包裹字符与尾随说明。
	value = strings.Trim(value, "\"'`。.，,：: ")
	for _, candidate := range AllIntents() {
		if value == string(candidate) {
			return candidate, true
		}
	}
	// 退一步：识别包含关系，兼容 "intent=knowledge" 这类输出。
	for _, candidate := range AllIntents() {
		if strings.Contains(value, string(candidate)) {
			return candidate, true
		}
	}
	return "", false
}

// NeedsToolLoop 判断该意图是否需要进入工具决策循环。
//
// 这是路由的核心收益：寒暄与无关请求无需检索知识库、
// 也无需把四个工具的完整 schema 塞进上下文，
// 因此可以短路处理，省掉整轮的工具目录与证据成本。
func (i Intent) NeedsToolLoop() bool {
	switch i {
	case IntentKnowledge, IntentIncident:
		return true
	default:
		return false
	}
}

// DefaultCategory 返回该意图对应的默认工单分类。
//
// 仅在用户未提供、且模型未给出有效分类时兜底使用。
func (i Intent) DefaultCategory() Category {
	switch i {
	case IntentIncident:
		return CategoryIncident
	case IntentKnowledge:
		return CategoryConsultation
	default:
		return CategoryRequest
	}
}

// String 实现 fmt.Stringer，便于日志与报告输出。
func (i Intent) String() string { return string(i) }

// Validate 校验意图合法性，返回可读错误。
func (i Intent) Validate() error {
	if !i.Valid() {
		return fmt.Errorf("无效的意图: %q", string(i))
	}
	return nil
}
