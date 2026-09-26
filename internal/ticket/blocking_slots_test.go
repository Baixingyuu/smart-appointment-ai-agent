package ticket

import (
	"reflect"
	"testing"

	"github.com/mac/helpdesk-agent/internal/domain"
)

func TestBlockingSlotsPerCategory(t *testing.T) {
	if got := BlockingSlots(domain.CategoryIncident); !reflect.DeepEqual(got, []string{"affected_system"}) {
		t.Errorf("incident 阻塞槽位应为 affected_system，实际 %v", got)
	}
	// 变更要求"改哪 + 什么时候"两个都阻塞。
	if got := BlockingSlots(domain.CategoryChange); len(got) != 2 {
		t.Errorf("change 应有 2 个阻塞槽位，实际 %v", got)
	}
	// 未知分类不应凭空要求槽位。
	if got := BlockingSlots(domain.Category("")); len(got) != 0 {
		t.Errorf("空分类不应有阻塞槽位，实际 %v", got)
	}
}

func TestParseExtractedSlotsToleratesDirtyInput(t *testing.T) {
	raw := []any{
		map[string]any{"name": " Affected_System ", "quote": "核心下单接口"},
		map[string]any{"name": "asked_topic", "quote": ""}, // 有槽位名但没证据
		map[string]any{"quote": "没有名字"},                    // 缺 name，跳过
		"不是对象",                                             // 脏元素，跳过
	}
	got := ParseExtractedSlots(raw)
	if len(got) != 2 {
		t.Fatalf("应解析出 2 条有效槽位，实际 %d：%+v", len(got), got)
	}
	// name 归一化为小写去空白，用于和 BlockingSlots 对齐。
	if got[0].Name != "affected_system" {
		t.Errorf("name 应归一化，实际 %q", got[0].Name)
	}
	if got[1].Quote != "" {
		t.Errorf("空 quote 应保留为空串（表示没给证据），实际 %q", got[1].Quote)
	}
}

func TestSlotsWithoutQuoteKeepsRequiredOrder(t *testing.T) {
	required := BlockingSlots(domain.CategoryChange) // target_service, planned_window
	extracted := []ExtractedSlot{
		{Name: "planned_window", Quote: "下周三凌晨"}, // 只填了第二个
	}
	got := SlotsWithoutQuote(required, extracted)
	if !reflect.DeepEqual(got, []string{"target_service"}) {
		t.Errorf("应报缺 target_service 且保序，实际 %v", got)
	}
}

func TestMissingBlockingSlotsFilledSet(t *testing.T) {
	required := []string{"affected_system"}
	filled := map[string]bool{"affected_system": true}
	if got := MissingBlockingSlots(required, filled); len(got) != 0 {
		t.Errorf("已填齐时不应报缺失，实际 %v", got)
	}
	if got := MissingBlockingSlots(required, nil); !reflect.DeepEqual(got, []string{"affected_system"}) {
		t.Errorf("未填时应报缺 affected_system，实际 %v", got)
	}
}

func TestSlotLabelCoversAllBlockingSlots(t *testing.T) {
	// 每个分类的每个阻塞槽位都要有话术，否则追问会退化成裸英文字段名。
	for _, category := range domain.AllCategories {
		for _, slot := range BlockingSlots(category) {
			label := SlotLabel(slot)
			if label == "" || label == "请补充："+slot {
				t.Errorf("槽位 %s（%s）缺少专用追问话术", slot, category)
			}
		}
	}
}
