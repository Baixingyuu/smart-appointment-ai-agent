package agent

import (
	"strings"
	"testing"

	"github.com/mac/helpdesk-agent/internal/domain"
	"github.com/mac/helpdesk-agent/internal/llm"
)

// 跨轮上下文注入（缺陷 D1）的回归测试。
//
// 为什么只能在这一层测：轨迹评测的 runner 直接调用 Agent.Run，
// 不经过 conversation.Service，因此消息根本不落库，
// 历史窗口在该轴上恒为空。要用评测轴验证 D1，
// 得先给 runner 补消息持久化。

// appendMessages 按给定顺序写入会话消息。
func appendMessages(t *testing.T, h *harness, conversationID int64, items ...domain.Message) {
	t.Helper()
	for i := range items {
		items[i].ConversationID = conversationID
		if err := h.store.AppendMessage(&items[i]); err != nil {
			t.Fatalf("写入消息失败: %v", err)
		}
	}
}

func TestHistoryWindowMapsSendersAndKeepsChronology(t *testing.T) {
	h := newHarness(t, nil)
	const conv = int64(7)
	appendMessages(t, h, conv,
		domain.Message{Sender: domain.SenderCustomer, Content: "接口一直返回 401"},
		domain.Message{Sender: domain.SenderAI, Content: "请确认 Authorization 头是否携带有效令牌"},
		domain.Message{Sender: domain.SenderHuman, Content: "该客户由人工跟进"},
		domain.Message{Sender: domain.SenderSystem, Content: "工单已指派"},
		domain.Message{Sender: domain.SenderCustomer, Content: "这个问题没解决"},
	)

	got := h.agent.historyWindow(conv, "帮我建个单跟进")
	if len(got) != 5 {
		t.Fatalf("期望 5 条历史，实际 %d：%+v", len(got), got)
	}
	if got[0].Content != "接口一直返回 401" {
		t.Errorf("历史必须按时间正序，首条实际 %q", got[0].Content)
	}

	// 人工与系统消息不能投成 assistant：否则模型会认为那些话是自己说的。
	if got[1].Role != llm.RoleAssistant {
		t.Errorf("AI 历史应投为 assistant，实际 %s", got[1].Role)
	}
	if got[2].Role != llm.RoleUser || !strings.HasPrefix(got[2].Content, "【人工客服】") {
		t.Errorf("人工客服应投为带标记的 user，实际 %s / %q", got[2].Role, got[2].Content)
	}
	if got[3].Role != llm.RoleUser || !strings.HasPrefix(got[3].Content, "【系统】") {
		t.Errorf("系统消息应投为带标记的 user，实际 %s / %q", got[3].Role, got[3].Content)
	}
}

func TestHistoryWindowDropsOnlyOneCurrentTail(t *testing.T) {
	h := newHarness(t, nil)
	const conv = int64(8)
	// 用户确实重复说过同一句话：只应去掉最后那条（本次回合已落库的副本），
	// 前一条是有效历史。
	appendMessages(t, h, conv,
		domain.Message{Sender: domain.SenderCustomer, Content: "还是不行"},
		domain.Message{Sender: domain.SenderAI, Content: "已重启服务"},
		domain.Message{Sender: domain.SenderCustomer, Content: "还是不行"},
	)

	got := h.agent.historyWindow(conv, "还是不行")
	if len(got) != 2 {
		t.Fatalf("期望保留 2 条（用户重复说过一次），实际 %d：%+v", len(got), got)
	}
	if got[0].Content != "还是不行" {
		t.Errorf("用户重复说过的那条必须保留，实际首条 %q", got[0].Content)
	}
}

func TestHistoryWindowRespectsCaps(t *testing.T) {
	t.Run("条数上限", func(t *testing.T) {
		h := newHarness(t, nil)
		const conv = int64(9)
		appendMessages(t, h, conv,
			domain.Message{Sender: domain.SenderCustomer, Content: "第一条"},
			domain.Message{Sender: domain.SenderCustomer, Content: "第二条"},
			domain.Message{Sender: domain.SenderCustomer, Content: "第三条"},
		)
		h.agent.config.HistoryMaxMessages = 1
		got := h.agent.historyWindow(conv, "当前消息")
		if len(got) != 1 || got[0].Content != "第三条" {
			t.Fatalf("超限时必须保留最近的，实际 %+v", got)
		}
	})

	t.Run("单条上限", func(t *testing.T) {
		h := newHarness(t, nil)
		const conv = int64(10)
		long := strings.Repeat("报", 300)
		appendMessages(t, h, conv, domain.Message{Sender: domain.SenderCustomer, Content: long})
		got := h.agent.historyWindow(conv, "当前消息")
		if len(got) != 1 {
			t.Fatalf("期望 1 条，实际 %d", len(got))
		}
		if n := len([]rune(got[0].Content)); n > h.agent.config.HistoryMaxItemRunes+1 {
			t.Errorf("单条未截断：%d rune", n)
		}
		if !strings.HasSuffix(got[0].Content, "…") {
			t.Errorf("截断必须留下省略号，实际结尾 %q", got[0].Content[len(got[0].Content)-8:])
		}
	})

	t.Run("总量上限丢掉最早的历史而非最近的", func(t *testing.T) {
		h := newHarness(t, nil)
		const conv = int64(11)
		item := strings.Repeat("详", 200)
		appendMessages(t, h, conv,
			domain.Message{Sender: domain.SenderCustomer, Content: item},
			domain.Message{Sender: domain.SenderAI, Content: item},
			domain.Message{Sender: domain.SenderCustomer, Content: item},
			domain.Message{Sender: domain.SenderAI, Content: item},
		)
		h.agent.config.HistoryMaxRunes = 420
		got := h.agent.historyWindow(conv, "当前消息")
		if len(got) != 2 {
			t.Fatalf("预算 420 应只装得下 2 条，实际 %d", len(got))
		}
		// 近因优先：装不下时留下的必须是最近两条。
		if got[1].Role != llm.RoleAssistant {
			t.Errorf("留下的必须是最近的消息，实际末条角色 %s", got[1].Role)
		}
	})
}

func TestHistoryWindowDisabled(t *testing.T) {
	h := newHarness(t, nil)
	const conv = int64(12)
	appendMessages(t, h, conv, domain.Message{Sender: domain.SenderCustomer, Content: "磁盘满了"})
	h.agent.config.HistoryMaxMessages = -1
	if got := h.agent.historyWindow(conv, "帮我建个单"); got != nil {
		t.Errorf("显式关闭历史后应返回 nil，实际 %+v", got)
	}
}

// TestRunSendsHistoryToModel 端到端确认历史真的进了模型请求，
// 而不只是 historyWindow 自己算得对。
func TestRunSendsHistoryToModel(t *testing.T) {
	h := newHarness(t, []*llm.Response{finalReply("已了解前因，正在为你登记")})
	const conv = int64(13)
	appendMessages(t, h, conv,
		domain.Message{Sender: domain.SenderCustomer, Content: "报销系统提示金额超限"},
		domain.Message{Sender: domain.SenderAI, Content: "请问是差旅费还是办公费？"},
	)

	h.runTurn(t, conv, "差旅费，还是不行")

	if len(h.model.requests) == 0 {
		t.Fatal("模型未被调用")
	}
	messages := h.model.requests[0].Messages
	if len(messages) != 3 {
		t.Fatalf("期望 2 条历史 + 1 条当前，实际 %d：%+v", len(messages), contents(messages))
	}
	if messages[2].Content != "差旅费，还是不行" {
		t.Errorf("当前消息必须排在最后，实际 %q", messages[2].Content)
	}
	// 当前这条已先落库，若没去掉尾部副本，模型会看到同一句说两遍。
	if count := countContent(messages, "差旅费，还是不行"); count != 1 {
		t.Errorf("当前消息重复送给了模型 %d 次", count)
	}
}

// TestShortCircuitPathKeepsSingleMessage 守住一条设计取舍：
// 短路路径不注入历史——它压根不进工具循环，省掉的正是 schema 与证据的开销。
func TestShortCircuitPathKeepsSingleMessage(t *testing.T) {
	classifier := &stubClassifier{byText: map[string]domain.Intent{"你好": domain.IntentChitchat}}
	h := newHarness(t, nil, withClassifier(classifier))
	const conv = int64(14)
	appendMessages(t, h, conv, domain.Message{Sender: domain.SenderCustomer, Content: "报销系统提示金额超限"})

	result := h.runTurn(t, conv, "你好")
	if !result.ShortCircuited {
		t.Fatal("寒暄必须短路，否则本用例没有检验到目标路径")
	}
	if len(h.model.requests) != 0 {
		t.Errorf("短路不应进入工具循环，实际调用 %d 次", len(h.model.requests))
	}
}

func contents(messages []llm.Message) []string {
	out := make([]string, 0, len(messages))
	for _, m := range messages {
		out = append(out, m.Content)
	}
	return out
}

func countContent(messages []llm.Message, want string) int {
	n := 0
	for _, m := range messages {
		if m.Content == want {
			n++
		}
	}
	return n
}
