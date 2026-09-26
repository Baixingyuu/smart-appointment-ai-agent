// 建单交互里的"阻塞性槽位"规则表。
//
// 只在缺阻塞性槽位时才向用户追问，其余信息一律次要 → 建单后记 MissingInfo。
// 规则一期手写常量（不推导，见 TICKET_INTAKE §10：槽位种类 > 12 或服务字典
// 明显变大时，才值得改成从服务字典推导）。
//
// 为什么每类只放 1~2 个：多了是设计者贪心，用户一轮答不完，反而抬高放弃率。
package ticket

import (
	"strings"

	"github.com/mac/helpdesk-agent/internal/domain"
)

// ExtractedSlot 是模型声称"这个阻塞槽位我已经从用户话里填上了"时给出的证据。
//
// 重版：必须带 quote（逐字摘录的用户原话）。轻版只让模型给 slotFilled: bool
// 会退化成模型自评，追问判定就建在沙上。quote 是否真出自用户原话，
// 由 agent 侧的 TraceabilityChecker 判定，本包只负责结构与"有没有给证据"。
type ExtractedSlot struct {
	Name  string `json:"name"`
	Quote string `json:"quote"`
}

// BlockingSlots 返回某分类的阻塞性槽位（按追问优先级排序）。
func BlockingSlots(category domain.Category) []string {
	switch category {
	case domain.CategoryIncident:
		// 不知道哪个系统出问题，处理人无从查起，也接不上派单的服务归属。
		return []string{"affected_system"}
	case domain.CategoryConsultation:
		return []string{"asked_topic"}
	case domain.CategoryRequest:
		return []string{"requested_action"}
	case domain.CategoryChange:
		// 变更：改哪 + 什么时候，缺任一都无法评估风险。
		return []string{"target_service", "planned_window"}
	default:
		return nil
	}
}

// SlotLabel 把槽位名翻译成追问话术里的一句人话。
func SlotLabel(name string) string {
	switch name {
	case "affected_system":
		return "是哪个系统/服务出的问题"
	case "asked_topic":
		return "您想咨询的具体问题是什么"
	case "requested_action":
		return "您希望我们执行什么操作"
	case "target_service":
		return "这次变更针对哪个服务"
	case "planned_window":
		return "计划的变更时间窗口"
	default:
		return "请补充：" + name
	}
}

// ParseExtractedSlots 从工具参数原始 JSON 数组解析槽位证据。
//
// 容忍脏输入：非对象元素、name 缺失一律跳过；name 归一化为小写去空白，
// 因为槽位名是稳定标识、要和 BlockingSlots 对齐比对。
func ParseExtractedSlots(raw []any) []ExtractedSlot {
	out := make([]ExtractedSlot, 0, len(raw))
	for _, item := range raw {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := object["name"].(string)
		quote, _ := object["quote"].(string)
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		out = append(out, ExtractedSlot{Name: name, Quote: strings.TrimSpace(quote)})
	}
	return out
}

// SlotsWithoutQuote 返回 required 里没有被"非空 quote"支撑的槽位。
//
// 只判有没有给证据；证据是否可追溯由 agent 的 TraceabilityChecker 决定。
// 保留 required 的顺序并去重，使追问话术稳定可断言。
func SlotsWithoutQuote(required []string, extracted []ExtractedSlot) []string {
	filled := make(map[string]bool, len(extracted))
	for _, slot := range extracted {
		if slot.Quote != "" {
			filled[slot.Name] = true
		}
	}
	return MissingBlockingSlots(required, filled)
}

// MissingBlockingSlots 返回 required 中未被 filled 覆盖的槽位，保持 required 顺序、去重。
//
// filled 的语义由调用方决定（一期为"该槽位给出了可追溯的 quote"）。
// 本函数只做集合差，不判定证据真假——那是 checker 的职责。
func MissingBlockingSlots(required []string, filled map[string]bool) []string {
	seen := make(map[string]bool, len(required))
	missing := make([]string, 0, len(required))
	for _, slot := range required {
		if seen[slot] {
			continue
		}
		seen[slot] = true
		if !filled[slot] {
			missing = append(missing, slot)
		}
	}
	return missing
}
