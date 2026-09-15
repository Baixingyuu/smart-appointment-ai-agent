package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/mac/helpdesk-agent/internal/assign"
	"github.com/mac/helpdesk-agent/internal/conversation"
	"github.com/mac/helpdesk-agent/internal/domain"
	"github.com/mac/helpdesk-agent/internal/seed"
	"github.com/mac/helpdesk-agent/internal/store"
	"github.com/mac/helpdesk-agent/internal/ticket"
)

// newTestServer 组装一套完整但全离线的 HTTP 测试环境。
func newTestServer(t *testing.T) (http.Handler, store.Store) {
	t.Helper()

	st := store.NewMemory()
	if err := seed.Load(st); err != nil {
		t.Fatalf("加载种子数据失败: %v", err)
	}
	tickets := ticket.New(st, assign.New(assign.DefaultWeights()))

	// 桩执行器：回显消息，并在消息里含「工单」时模拟建单。
	executor := conversation.TurnExecutorFunc(func(conversationID int64, message string) (conversation.TurnOutcome, error) {
		outcome := conversation.TurnOutcome{Reply: "回复：" + message}
		if strings.Contains(message, "建单") {
			created, err := tickets.Create(ticketInput(conversationID, message))
			if err != nil {
				return conversation.TurnOutcome{}, err
			}
			outcome.TicketID = created.Ticket.ID
			outcome.Reply = "已创建工单"
		}
		if strings.Contains(message, "确认") {
			outcome.Interrupted = true
			outcome.Reply = "请确认是否创建工单"
		}
		return outcome, nil
	})

	conversations := conversation.New(st, executor)
	server := New(st, conversations, tickets, "测试模式")
	return server.Handler(), st
}

// ticketInput 构造一份合法的建单输入。
func ticketInput(conversationID int64, title string) domain.TicketInput {
	return domain.TicketInput{
		Title:          title,
		Description:    "由测试构造的工单描述",
		Category:       domain.CategoryIncident,
		Priority:       domain.PriorityP2,
		RequiredSkill:  domain.NewSkillSet(2, 3),
		ConversationID: conversationID,
		SourceChannel:  "web",
	}
}

func doJSON(t *testing.T, handler http.Handler, method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化请求体失败: %v", err)
		}
		reader = bytes.NewReader(data)
	}

	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	payload := make(map[string]any)
	if recorder.Body.Len() > 0 {
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatalf("响应不是合法 JSON: %v\n原始内容: %s", err, recorder.Body.String())
		}
	}
	return recorder, payload
}

func TestHealthReportsModeAndCounts(t *testing.T) {
	handler, _ := newTestServer(t)

	recorder, payload := doJSON(t, handler, http.MethodGet, "/api/health", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", recorder.Code)
	}
	if payload["status"] != "ok" {
		t.Errorf("status 应为 ok，实际 %v", payload["status"])
	}
	// 模式必须暴露：使用者据此判断当前是真实模型还是离线脚本，
	// 避免把演示效果误当作模型能力。
	if payload["mode"] != "测试模式" {
		t.Errorf("应返回模型模式，实际 %v", payload["mode"])
	}
	counts, ok := payload["counts"].(map[string]any)
	if !ok {
		t.Fatalf("应返回计数信息，实际 %v", payload["counts"])
	}
	if counts["employees"].(float64) == 0 {
		t.Error("种子员工应被计入")
	}
}

func TestCreateAndGetConversation(t *testing.T) {
	handler, _ := newTestServer(t)

	recorder, created := doJSON(t, handler, http.MethodPost, "/api/conversations",
		map[string]any{"title": "接口问题", "sourceChannel": "web"})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("期望 201，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	id := int64(created["id"].(float64))

	recorder, detail := doJSON(t, handler, http.MethodGet, "/api/conversations/"+itoa(id), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", recorder.Code)
	}
	conv, ok := detail["conversation"].(map[string]any)
	if !ok {
		t.Fatalf("应返回会话对象，实际 %v", detail)
	}
	if conv["title"] != "接口问题" {
		t.Errorf("标题不符：%v", conv["title"])
	}
	if conv["status"] != "active" {
		t.Errorf("新会话应为 active，实际 %v", conv["status"])
	}
}

func TestSendMessageReturnsReplyAndStoresBoth(t *testing.T) {
	handler, st := newTestServer(t)
	convID := createConversation(t, handler)

	recorder, payload := doJSON(t, handler, http.MethodPost,
		"/api/conversations/"+itoa(convID)+"/messages",
		map[string]any{"content": "接口返回 401", "requestId": "req-1"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if payload["reply"] != "回复：接口返回 401" {
		t.Errorf("回复不符：%v", payload["reply"])
	}
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("应返回客户消息与回复两条，实际 %v", payload["messages"])
	}
	if got := len(st.MessagesByConversation(convID)); got != 2 {
		t.Fatalf("应落库 2 条消息，实际 %d", got)
	}
}

func TestSendMessageIsIdempotent(t *testing.T) {
	handler, st := newTestServer(t)
	convID := createConversation(t, handler)

	path := "/api/conversations/" + itoa(convID) + "/messages"
	body := map[string]any{"content": "接口报错", "requestId": "req-dup"}

	// 第一次与第二次都发出，第三次用于断言重复标记。
	doJSON(t, handler, http.MethodPost, path, body)
	doJSON(t, handler, http.MethodPost, path, body)

	recorder, payload := doJSON(t, handler, http.MethodPost, path, body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("重复请求应返回 200 而非错误，实际 %d", recorder.Code)
	}
	if payload["duplicate"] != true {
		t.Errorf("应标记为重复，实际 %v", payload["duplicate"])
	}
	// 幂等的核心：重复投递不得再落消息，否则会重复触发 AI 甚至重复建单。
	if got := len(st.MessagesByConversation(convID)); got != 2 {
		t.Fatalf("重复请求后消息数应仍为 2，实际 %d", got)
	}
}

func TestSendMessageWithoutRequestIDIsStillIdempotent(t *testing.T) {
	// 客户端未传幂等键时，服务端按「会话 + 内容」派生，
	// 至少能拦住同一句话被重复提交这种最常见的重复。
	handler, st := newTestServer(t)
	convID := createConversation(t, handler)

	path := "/api/conversations/" + itoa(convID) + "/messages"
	body := map[string]any{"content": "同一句话"}
	doJSON(t, handler, http.MethodPost, path, body)
	_, payload := doJSON(t, handler, http.MethodPost, path, body)

	if payload["duplicate"] != true {
		t.Errorf("内容相同的重复提交应被判为重复，实际 %v", payload)
	}
	if got := len(st.MessagesByConversation(convID)); got != 2 {
		t.Fatalf("应仍只有 2 条消息，实际 %d", got)
	}
}

func TestSendMessagePropagatesInterruptAndTicket(t *testing.T) {
	handler, _ := newTestServer(t)
	convID := createConversation(t, handler)

	// 中断标记必须透传到 HTTP 层，否则前端无法提示「正在等待确认」。
	_, payload := doJSON(t, handler, http.MethodPost,
		"/api/conversations/"+itoa(convID)+"/messages",
		map[string]any{"content": "请确认", "requestId": "req-int"})
	turn, ok := payload["turn"].(map[string]any)
	if !ok {
		t.Fatalf("应返回 turn 信息，实际 %v", payload)
	}
	if turn["interrupted"] != true {
		t.Errorf("中断标记应透传，实际 %v", turn)
	}

	// 建单结果同样应透传。
	_, payload = doJSON(t, handler, http.MethodPost,
		"/api/conversations/"+itoa(convID)+"/messages",
		map[string]any{"content": "帮我建单", "requestId": "req-tkt"})
	turn = payload["turn"].(map[string]any)
	if turn["ticketId"].(float64) == 0 {
		t.Errorf("工单 ID 应透传，实际 %v", turn)
	}
}

func TestSendMessageToClosedConversationReturnsConflict(t *testing.T) {
	handler, _ := newTestServer(t)
	convID := createConversation(t, handler)

	if recorder, _ := doJSON(t, handler, http.MethodPost,
		"/api/conversations/"+itoa(convID)+"/close", nil); recorder.Code != http.StatusOK {
		t.Fatalf("关闭会话失败：%d", recorder.Code)
	}

	// 409 而非 500：这是客户端可纠正的状态冲突，不是服务端错误。
	recorder, payload := doJSON(t, handler, http.MethodPost,
		"/api/conversations/"+itoa(convID)+"/messages",
		map[string]any{"content": "还有问题", "requestId": "req-after-close"})
	if recorder.Code != http.StatusConflict {
		t.Fatalf("已关闭会话应返回 409，实际 %d", recorder.Code)
	}
	if payload["error"] == nil {
		t.Error("应返回可读错误信息")
	}
}

func TestUnknownConversationReturns404(t *testing.T) {
	handler, _ := newTestServer(t)
	recorder, _ := doJSON(t, handler, http.MethodGet, "/api/conversations/9999", nil)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("不存在的会话应返回 404，实际 %d", recorder.Code)
	}
}

func TestInvalidPathIDReturns400(t *testing.T) {
	handler, _ := newTestServer(t)
	for _, path := range []string{"/api/conversations/abc", "/api/conversations/0", "/api/conversations/-1"} {
		recorder, _ := doJSON(t, handler, http.MethodGet, path, nil)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s 应返回 400，实际 %d", path, recorder.Code)
		}
	}
}

func TestUnknownJSONFieldIsRejected(t *testing.T) {
	// 拼错字段名必须立即报错。若静默忽略，客户端会表现为
	// 「参数传了但没生效」，排查成本很高。
	handler, _ := newTestServer(t)
	recorder, payload := doJSON(t, handler, http.MethodPost, "/api/conversations",
		map[string]any{"titel": "拼错的字段"})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("未知字段应返回 400，实际 %d", recorder.Code)
	}
	if payload["error"] == nil {
		t.Error("应说明解析失败原因")
	}
}

func TestEmptyMessageIsRejected(t *testing.T) {
	handler, _ := newTestServer(t)
	convID := createConversation(t, handler)

	recorder, _ := doJSON(t, handler, http.MethodPost,
		"/api/conversations/"+itoa(convID)+"/messages",
		map[string]any{"content": "   ", "requestId": "req-empty"})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("空消息应返回 400，实际 %d", recorder.Code)
	}
}

func TestListTicketsAndDetail(t *testing.T) {
	handler, _ := newTestServer(t)
	convID := createConversation(t, handler)

	doJSON(t, handler, http.MethodPost, "/api/conversations/"+itoa(convID)+"/messages",
		map[string]any{"content": "帮我建单：接口 500", "requestId": "req-lt"})

	recorder, list := doJSON(t, handler, http.MethodGet, "/api/tickets", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", recorder.Code)
	}
	results := list["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("应返回 1 张工单，实际 %d", len(results))
	}
	first := results[0].(map[string]any)
	if first["assigneeId"].(float64) == 0 {
		t.Error("工单应已被指派")
	}
	// 技能需求必须返回：它是「按经验派单」的可解释依据。
	if first["requiredSkills"] == nil {
		t.Error("应返回技能需求")
	}

	ticketID := int64(first["id"].(float64))
	recorder, detail := doJSON(t, handler, http.MethodGet, "/api/tickets/"+itoa(ticketID), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", recorder.Code)
	}
	// 指派理由与进展时间线必须可查：业务方据此复核「为什么派给他」。
	if detail["assignments"] == nil {
		t.Error("应返回指派理由")
	}
	progress, ok := detail["progress"].([]any)
	if !ok || len(progress) == 0 {
		t.Fatalf("应返回进展记录，实际 %v", detail["progress"])
	}
}

func TestListSkillsAndEmployees(t *testing.T) {
	handler, _ := newTestServer(t)

	_, skills := doJSON(t, handler, http.MethodGet, "/api/skills", nil)
	if len(skills["results"].([]any)) == 0 {
		t.Error("应返回技能树")
	}

	_, employees := doJSON(t, handler, http.MethodGet, "/api/employees", nil)
	results := employees["results"].([]any)
	if len(results) == 0 {
		t.Fatal("应返回员工列表")
	}
	// 负载应为实时值（按未完成工单数计算），而不是静态字段。
	for _, item := range results {
		record := item.(map[string]any)
		if record["currentLoad"] == nil {
			t.Errorf("员工应返回实时负载：%v", record)
		}
	}
}

func TestMethodNotAllowed(t *testing.T) {
	handler, _ := newTestServer(t)
	recorder, _ := doJSON(t, handler, http.MethodDelete, "/api/conversations", nil)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("未注册的方法应返回 405，实际 %d", recorder.Code)
	}
}

func createConversation(t *testing.T, handler http.Handler) int64 {
	t.Helper()
	recorder, payload := doJSON(t, handler, http.MethodPost, "/api/conversations",
		map[string]any{"title": "测试会话"})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("创建会话失败：%d %s", recorder.Code, recorder.Body.String())
	}
	return int64(payload["id"].(float64))
}

func itoa(value int64) string { return strconv.FormatInt(value, 10) }
