package conversation

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mac/agentdesk/internal/domain"
	"github.com/mac/agentdesk/internal/store"
)

func fixedClock() func() time.Time {
	base := time.Date(2024, 5, 1, 10, 0, 0, 0, time.UTC)
	n := 0
	return func() time.Time {
		n++
		return base.Add(time.Duration(n) * time.Second)
	}
}

func newService(t *testing.T, executor TurnExecutor) (*Service, store.Store) {
	t.Helper()
	st := store.NewMemory()
	st.SetClock(fixedClock())
	return New(st, executor, WithClock(fixedClock())), st
}

// echoExecutor 把客户消息原样回显，便于断言回复落库位置。
func echoExecutor() TurnExecutor {
	return TurnExecutorFunc(func(_ int64, message string) (TurnOutcome, error) {
		return TurnOutcome{Reply: "回复：" + message}, nil
	})
}

func TestStartCreatesActiveConversation(t *testing.T) {
	svc, _ := newService(t, nil)

	conv, err := svc.Start(StartInput{Title: "咨询接口问题", SourceChannel: "web", CustomerRef: "c-1"})
	if err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	if conv.ID == 0 {
		t.Fatal("应分配会话 ID")
	}
	if conv.Status != domain.ConversationActive {
		t.Fatalf("新会话应为 active，实际 %s", conv.Status)
	}
	if conv.CreatedAt.IsZero() {
		t.Error("应记录创建时间")
	}
}

func TestStartDefaultsChannelAndTitle(t *testing.T) {
	svc, _ := newService(t, nil)

	conv, err := svc.Start(StartInput{})
	if err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	// 空渠道与空标题必须有合理默认值，否则列表页会显示空白项。
	if conv.SourceChannel != "web" {
		t.Errorf("渠道应默认为 web，实际 %q", conv.SourceChannel)
	}
	if conv.Title == "" {
		t.Error("标题应有默认值")
	}
}

func TestSendStoresCustomerMessageAndReplyInOrder(t *testing.T) {
	svc, st := newService(t, echoExecutor())

	conv, err := svc.Start(StartInput{})
	if err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	result, err := svc.Send(conv.ID, "接口返回 401", "req-1")
	if err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if result.Duplicate {
		t.Error("首次发送不应判为重复")
	}
	if result.Reply == nil || !strings.Contains(result.Reply.Content, "接口返回 401") {
		t.Fatalf("应写入 AI 回复，实际 %+v", result.Reply)
	}

	messages := st.MessagesByConversation(conv.ID)
	if len(messages) != 2 {
		t.Fatalf("应落库 2 条消息，实际 %d", len(messages))
	}
	// 顺序必须是「客户 → AI」，否则前端渲染会颠倒对话。
	if messages[0].Sender != domain.SenderCustomer {
		t.Errorf("第 1 条应为客户消息，实际 %s", messages[0].Sender)
	}
	if messages[1].Sender != domain.SenderAI {
		t.Errorf("第 2 条应为 AI 消息，实际 %s", messages[1].Sender)
	}
	if messages[0].ID >= messages[1].ID {
		t.Error("消息 ID 应递增，保证按 ID 排序即为时间序")
	}
}

func TestSendReturnsPopulatedTimestamps(t *testing.T) {
	// 回归用例：AppendMessage 曾按值接收消息，存储层填的 ID 与时间
	// 没有回传调用方，导致接口响应里消息时间戳是零值
	// （只在真实跑服务时才暴露，单测没断言到）。
	svc, _ := newService(t, echoExecutor())

	conv, _ := svc.Start(StartInput{})
	result, err := svc.Send(conv.ID, "接口报错", "req-ts")
	if err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if result.CustomerMessage.ID == 0 {
		t.Error("客户消息 ID 应被回填")
	}
	if result.CustomerMessage.CreatedAt.IsZero() {
		t.Error("客户消息时间不应为零值")
	}
	if result.CustomerMessage.CreatedAt.Year() < 2000 {
		t.Errorf("客户消息时间明显异常：%v", result.CustomerMessage.CreatedAt)
	}
	if result.Reply == nil {
		t.Fatal("应有回复")
	}
	if result.Reply.ID == 0 {
		t.Error("回复 ID 应被回填")
	}
	if result.Reply.CreatedAt.IsZero() || result.Reply.CreatedAt.Year() < 2000 {
		t.Errorf("回复时间应被回填，实际 %v", result.Reply.CreatedAt)
	}
	// 回复必须晚于客户消息：ID 单调递增即可保证按 ID 排序等于时间序。
	if result.Reply.ID <= result.CustomerMessage.ID {
		t.Error("回复 ID 应大于客户消息 ID")
	}
}

func TestSendIsIdempotentByRequestID(t *testing.T) {
	svc, st := newService(t, echoExecutor())

	conv, _ := svc.Start(StartInput{})
	if _, err := svc.Send(conv.ID, "接口报错", "req-dup"); err != nil {
		t.Fatalf("首次发送失败: %v", err)
	}
	second, err := svc.Send(conv.ID, "接口报错", "req-dup")
	if err != nil {
		t.Fatalf("重复发送不应返回错误: %v", err)
	}
	// 幂等的关键：重复投递不得再落消息，也不得再触发 AI。
	// 否则客户端重试会重复建单。
	if !second.Duplicate {
		t.Error("相同 RequestID 应判为重复")
	}
	if second.Reply != nil {
		t.Error("重复请求不应产生新回复")
	}
	if messages := st.MessagesByConversation(conv.ID); len(messages) != 2 {
		t.Fatalf("重复请求后消息数应仍为 2，实际 %d", len(messages))
	}
}

func TestSendDifferentRequestIDCreatesNewMessages(t *testing.T) {
	svc, st := newService(t, echoExecutor())

	conv, _ := svc.Start(StartInput{})
	if _, err := svc.Send(conv.ID, "第一个问题", "req-1"); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if _, err := svc.Send(conv.ID, "第二个问题", "req-2"); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if messages := st.MessagesByConversation(conv.ID); len(messages) != 4 {
		t.Fatalf("两次提问应产生 4 条消息，实际 %d", len(messages))
	}
}

func TestSendRejectsClosedConversation(t *testing.T) {
	svc, _ := newService(t, echoExecutor())

	conv, _ := svc.Start(StartInput{})
	if _, err := svc.Close(conv.ID); err != nil {
		t.Fatalf("关闭会话失败: %v", err)
	}

	// 已关闭会话不得再接受消息：否则新问题会挂在旧会话上，
	// 导致「同会话未关闭工单」的去重判断误伤。
	_, err := svc.Send(conv.ID, "还有问题", "req-3")
	if !errors.Is(err, domain.ErrConversationClosed) {
		t.Fatalf("应返回 ErrConversationClosed，实际 %v", err)
	}
}

func TestSendRejectsUnknownConversation(t *testing.T) {
	svc, _ := newService(t, echoExecutor())
	if _, err := svc.Send(999, "你好", "req-x"); err == nil {
		t.Fatal("不存在的会话应报错")
	}
}

func TestSendRejectsEmptyContent(t *testing.T) {
	svc, _ := newService(t, echoExecutor())
	conv, _ := svc.Start(StartInput{})
	if _, err := svc.Send(conv.ID, "   ", "req-y"); err == nil {
		t.Fatal("空消息应被拒绝")
	}
}

func TestSendWithoutExecutorOnlyStoresMessage(t *testing.T) {
	// 未配置执行器时应只落库消息，便于离线导入历史对话。
	svc, st := newService(t, nil)

	conv, _ := svc.Start(StartInput{})
	result, err := svc.Send(conv.ID, "历史消息", "req-hist")
	if err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if result.Reply != nil || result.Turn != nil {
		t.Error("未配置执行器时不应产生回复")
	}
	if messages := st.MessagesByConversation(conv.ID); len(messages) != 1 {
		t.Fatalf("应只落库 1 条消息，实际 %d", len(messages))
	}
}

func TestSendKeepsCustomerMessageWhenExecutorFails(t *testing.T) {
	// AI 失败不得丢失客户消息：消息已落库，错误向上报，
	// 由调用方决定是否转人工，而不是把整条消息回滚掉。
	failing := TurnExecutorFunc(func(int64, string) (TurnOutcome, error) {
		return TurnOutcome{}, errors.New("模型不可用")
	})
	svc, st := newService(t, failing)

	conv, _ := svc.Start(StartInput{})
	_, err := svc.Send(conv.ID, "接口报错", "req-fail")
	if err == nil {
		t.Fatal("执行器失败应向上返回错误")
	}
	messages := st.MessagesByConversation(conv.ID)
	if len(messages) != 1 {
		t.Fatalf("客户消息必须保留，实际落库 %d 条", len(messages))
	}
	if messages[0].Sender != domain.SenderCustomer {
		t.Errorf("保留的应是客户消息，实际 %s", messages[0].Sender)
	}
}

func TestSendUpdatesDefaultTitleFromFirstMessage(t *testing.T) {
	svc, st := newService(t, echoExecutor())

	conv, _ := svc.Start(StartInput{})
	if conv.Title != "新的咨询" {
		t.Fatalf("初始标题应为默认值，实际 %q", conv.Title)
	}
	if _, err := svc.Send(conv.ID, "核心接口持续返回 500 错误", "req-title"); err != nil {
		t.Fatalf("发送失败: %v", err)
	}

	updated, err := st.GetConversation(conv.ID)
	if err != nil {
		t.Fatalf("读取会话失败: %v", err)
	}
	// 标题应取自首条消息：否则会话列表里全是「新的咨询」无法分辨。
	if updated.Title == "新的咨询" || !strings.Contains(updated.Title, "接口") {
		t.Fatalf("标题应由首条消息生成，实际 %q", updated.Title)
	}
}

func TestSendKeepsExplicitTitle(t *testing.T) {
	svc, st := newService(t, echoExecutor())

	conv, _ := svc.Start(StartInput{Title: "人工指定的标题"})
	if _, err := svc.Send(conv.ID, "消息内容", "req-t2"); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	updated, _ := st.GetConversation(conv.ID)
	if updated.Title != "人工指定的标题" {
		t.Errorf("显式标题不应被覆盖，实际 %q", updated.Title)
	}
}

func TestSummarizeTitleTruncatesLongContent(t *testing.T) {
	long := strings.Repeat("很长的描述", 20)
	title := summarizeTitle(long)
	if len([]rune(title)) > 25 {
		t.Fatalf("标题应被截断，实际长度 %d", len([]rune(title)))
	}
	if !strings.HasSuffix(title, "…") {
		t.Errorf("截断后应有省略号，实际 %q", title)
	}
	// 换行应被折叠为空格，避免标题在列表里折行。
	if got := summarizeTitle("第一行\n第二行"); strings.Contains(got, "\n") {
		t.Errorf("标题不应含换行，实际 %q", got)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	svc, _ := newService(t, nil)
	conv, _ := svc.Start(StartInput{})

	first, err := svc.Close(conv.ID)
	if err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	second, err := svc.Close(conv.ID)
	if err != nil {
		t.Fatalf("重复关闭不应报错: %v", err)
	}
	if first.Status != domain.ConversationClosed || second.Status != domain.ConversationClosed {
		t.Error("关闭后状态应为 closed")
	}
}

func TestGetReturnsMessagesInOrder(t *testing.T) {
	svc, _ := newService(t, echoExecutor())
	conv, _ := svc.Start(StartInput{})

	for i, text := range []string{"问题一", "问题二", "问题三"} {
		if _, err := svc.Send(conv.ID, text, "req-"+string(rune('a'+i))); err != nil {
			t.Fatalf("发送失败: %v", err)
		}
	}
	detail, err := svc.Get(conv.ID)
	if err != nil {
		t.Fatalf("读取详情失败: %v", err)
	}
	if len(detail.Messages) != 6 {
		t.Fatalf("三次提问应产生 6 条消息，实际 %d", len(detail.Messages))
	}
	for i := 1; i < len(detail.Messages); i++ {
		if detail.Messages[i-1].ID >= detail.Messages[i].ID {
			t.Fatal("消息必须按 ID 升序返回")
		}
	}
}

func TestTurnOutcomeIsPropagated(t *testing.T) {
	// 中断与建单结果必须从 Agent 传到调用方，
	// 否则 HTTP 层无法告知客户端「正在等待确认」。
	executor := TurnExecutorFunc(func(int64, string) (TurnOutcome, error) {
		return TurnOutcome{Reply: "请确认", Interrupted: true, TicketID: 42}, nil
	})
	svc, _ := newService(t, executor)

	conv, _ := svc.Start(StartInput{})
	result, err := svc.Send(conv.ID, "建个工单", "req-turn")
	if err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if result.Turn == nil {
		t.Fatal("应回传回合结果")
	}
	if !result.Turn.Interrupted {
		t.Error("中断标记应被透传")
	}
	if result.Turn.TicketID != 42 {
		t.Errorf("工单 ID 应被透传，实际 %d", result.Turn.TicketID)
	}
}
