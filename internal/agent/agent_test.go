package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mac/agentdesk/internal/assign"
	"github.com/mac/agentdesk/internal/domain"
	"github.com/mac/agentdesk/internal/llm"
	"github.com/mac/agentdesk/internal/rag"
	"github.com/mac/agentdesk/internal/seed"
	"github.com/mac/agentdesk/internal/store"
	"github.com/mac/agentdesk/internal/ticket"
	"github.com/mac/agentdesk/internal/tooling"
)

// fakeModel 是按脚本回放的假模型。
//
// 设计要点：整个工具链路（治理、确认中断、恢复、建单）都必须能在
// 无网络、无 API Key 的情况下确定性验证，因此模型侧完全可编排。
// 每次调用按顺序取出一个预置响应，用尽后重复最后一个。
type fakeModel struct {
	mu        sync.Mutex
	responses []*llm.Response
	calls     int
	requests  []llm.ToolRequest
	err       error
}

func (f *fakeModel) Chat(_ context.Context, _, _ string) (*llm.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &llm.Response{Content: "纯文本回复", Usage: llm.Usage{PromptTokens: 10, CompletionTokens: 5}}, nil
}

func (f *fakeModel) ChatWithTools(_ context.Context, req llm.ToolRequest) (*llm.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	if f.err != nil {
		return nil, f.err
	}
	index := f.calls
	f.calls++
	if index >= len(f.responses) {
		index = len(f.responses) - 1
	}
	if index < 0 {
		return nil, errors.New("假模型没有配置任何响应")
	}
	response := f.responses[index]
	// 回填用量，使 token 累加可被断言。
	if response.Usage.Total() == 0 {
		response.Usage = llm.Usage{PromptTokens: 100, CompletionTokens: 20}
	}
	return response, nil
}

// requestCount 返回模型被调用的次数。
func (f *fakeModel) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// lastRequest 返回最近一次请求，用于断言工具列表与提示词。
func (f *fakeModel) lastRequest() llm.ToolRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return llm.ToolRequest{}
	}
	return f.requests[len(f.requests)-1]
}

// toolCall 构造一个工具调用响应。
func toolCall(id, name, args string) *llm.Response {
	return &llm.Response{
		ToolCalls: []llm.ToolCall{{ID: id, Name: name, Arguments: args}},
		Usage:     llm.Usage{PromptTokens: 100, CompletionTokens: 20},
	}
}

// finalReply 构造一个最终回复响应。
func finalReply(text string) *llm.Response {
	return &llm.Response{
		Content: text,
		Usage:   llm.Usage{PromptTokens: 100, CompletionTokens: 20},
	}
}

// harness 组装一套完整但全部离线的测试装置。
type harness struct {
	agent    *Agent
	model    *fakeModel
	store    *store.Memory
	tickets  *ticket.Service
	testsNow time.Time
}

type harnessOption func(*harnessConfig)

type harnessConfig struct {
	policy    tooling.Policy
	retriever bool
	config    Config
}

// withoutRetriever 表示不配置知识库，用于验证 no_knowledge 分支。
func withoutRetriever() harnessOption {
	return func(c *harnessConfig) { c.retriever = false }
}

// withPolicy 覆盖治理策略。
func withPolicy(policy tooling.Policy) harnessOption {
	return func(c *harnessConfig) { c.policy = policy }
}

func newHarness(t *testing.T, responses []*llm.Response, opts ...harnessOption) *harness {
	t.Helper()

	cfg := harnessConfig{retriever: true}
	for _, opt := range opts {
		opt(&cfg)
	}

	st := store.NewMemory()
	now := time.Date(2024, 5, 1, 10, 0, 0, 0, time.UTC)
	st.SetClock(func() time.Time { return now })
	if err := seed.Load(st); err != nil {
		t.Fatalf("加载种子数据失败: %v", err)
	}

	tickets := ticket.New(st, assign.New(assign.DefaultWeights()), ticket.WithClock(func() time.Time { return now }))

	var retriever *rag.Retriever
	if cfg.retriever {
		retriever = rag.New(seedKnowledgeChunks(), rag.NewHashEmbedder(512), rag.Options{
			TopK:            5,
			ScoreThreshold:  0.01, // 放宽阈值，让用例聚焦在编排而非检索质量
			MinCoverage:     0.01,
			MaxContextItems: 3,
			MaxPerDoc:       2,
		})
	}

	model := &fakeModel{responses: responses}
	if len(responses) == 0 {
		model.responses = []*llm.Response{finalReply("默认回复")}
	}

	config := cfg.config
	if config.Policy.MaxTotalCalls == 0 {
		config.Policy = cfg.policy
	}
	if config.Policy.MaxTotalCalls == 0 {
		config.Policy = tooling.DefaultPolicy()
	}

	ag, err := New(model, st, retriever, tickets, config, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("构造 Agent 失败: %v", err)
	}
	return &harness{agent: ag, model: model, store: st, tickets: tickets, testsNow: now}
}

// seedKnowledgeChunks 提供小规模知识库，与 RAG 包中的测试数据保持一致。
func seedKnowledgeChunks() []rag.Chunk {
	return []rag.Chunk{
		{
			ID: "k1", DocID: "d1", Title: "接口鉴权失败排查",
			Content:  "接口返回 401 表示鉴权令牌过期或签名不正确。请检查 Authorization 头是否携带有效令牌，并确认系统时间准确。",
			Keywords: []string{"接口", "401", "鉴权", "令牌"},
		},
		{
			ID: "k2", DocID: "d2", Title: "账号被锁定处理",
			Content:  "连续多次输入错误密码会触发账号锁定，通常锁定十五分钟。管理员可在成员管理中重置密码并解锁账号。",
			Keywords: []string{"账号", "锁定", "密码", "解锁"},
		},
	}
}

// runTurn 执行一次会话回合。
func (h *harness) runTurn(t *testing.T, conversationID int64, message string) *TurnResult {
	t.Helper()
	result, err := h.agent.Run(context.Background(), TurnInput{
		ConversationID: conversationID,
		UserMessage:    message,
	})
	if err != nil {
		t.Fatalf("回合执行失败: %v", err)
	}
	return result
}

func TestRunReturnsReplyWithoutToolCalls(t *testing.T) {
	h := newHarness(t, []*llm.Response{finalReply("你好，请描述你遇到的问题。")})

	result := h.runTurn(t, 1, "你好")
	if result.Reply != "你好，请描述你遇到的问题。" {
		t.Fatalf("回复不符：%s", result.Reply)
	}
	if result.Interrupted {
		t.Error("无工具调用时不应产生中断")
	}
	if result.Usage.Total() == 0 {
		t.Error("必须回传 token 用量，否则成本无法归因")
	}
}

func TestToolSchemasAreExposedWithRealParameters(t *testing.T) {
	h := newHarness(t, []*llm.Response{finalReply("好的")})
	h.runTurn(t, 1, "接口报错")

	req := h.model.lastRequest()
	if len(req.Tools) != 4 {
		t.Fatalf("应对模型开放 4 个工具，实际 %d", len(req.Tools))
	}
	// 参数必须是真实 schema。参考实现把参数写成泛型对象，
	// 模型看不到参数含义只能靠猜，工具调用准确率因此无法提升。
	for _, schema := range req.Tools {
		if schema.Parameters == nil {
			t.Errorf("工具 %s 缺少参数 schema", schema.Name)
			continue
		}
		properties, ok := schema.Parameters["properties"].(map[string]any)
		if !ok || len(properties) == 0 {
			t.Errorf("工具 %s 的 schema 没有声明任何参数", schema.Name)
		}
	}
	// 系统提示词里不应泄露工具实现细节之外的东西，但要包含使用原则。
	if req.System == "" {
		t.Error("必须提供系统提示词")
	}
}

func TestReadToolExecutesAndFeedsObservationBack(t *testing.T) {
	h := newHarness(t, []*llm.Response{
		toolCall("call-1", ToolRAGSearch, `{"query":"接口返回 401 鉴权失败"}`),
		finalReply("请检查 Authorization 头中的令牌是否过期。"),
	})

	result := h.runTurn(t, 1, "接口返回 401 怎么办")

	if len(result.ToolCalls) != 1 {
		t.Fatalf("期望 1 次工具调用，实际 %d", len(result.ToolCalls))
	}
	record := result.ToolCalls[0]
	if record.Status != "completed" {
		t.Fatalf("只读工具应执行成功，实际 %s（%s）", record.Status, record.Result)
	}
	if record.Risk != tooling.RiskRead {
		t.Errorf("rag_search 应为只读工具，实际 %s", record.Risk)
	}
	if result.RAGResult == nil {
		t.Fatal("应保留检索详情供评测使用")
	}
	// 工具结果必须回灌给模型：第二轮请求应包含 tool 角色的消息。
	req := h.model.lastRequest()
	var hasToolMessage bool
	for _, msg := range req.Messages {
		if msg.Role == llm.RoleTool {
			hasToolMessage = true
		}
	}
	if !hasToolMessage {
		t.Error("工具观察结果必须回灌给模型，否则模型无法基于检索作答")
	}
}

func TestWriteToolCreatesInterruptInsteadOfWriting(t *testing.T) {
	h := newHarness(t, []*llm.Response{
		toolCall("call-1", ToolCreateConfirm, `{"title":"接口持续报错","description":"下单接口错误率上升","category":"incident","priority":"P0"}`),
	})

	result := h.runTurn(t, 100, "帮我建个工单")

	if !result.Interrupted {
		t.Fatal("写操作必须转为确认中断")
	}
	if result.CheckPointID == "" {
		t.Error("中断必须带 CheckPointID 以便恢复")
	}
	if result.Prompt == "" {
		t.Error("中断必须给出确认文案")
	}
	// 关键：工单此时不应被创建。
	if tickets := h.store.ListTickets(); len(tickets) != 0 {
		t.Fatalf("确认前不应创建工单，实际 %d 张", len(tickets))
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].ErrorKind != tooling.KindNeedsConfirm {
		t.Fatalf("应记录为需要确认，实际 %+v", result.ToolCalls)
	}

	pending, ok := h.store.FindPendingInterrupt(100)
	if !ok {
		t.Fatal("应落库一条待确认中断")
	}
	if pending.Kind != domain.InterruptTicketCreation {
		t.Errorf("中断类型不符：%s", pending.Kind)
	}
	// 恢复所需的全部数据必须落库：恢复可能发生在进程重启之后。
	if pending.Payload.Title != "接口持续报错" {
		t.Errorf("中断载荷必须完整落库，实际标题 %q", pending.Payload.Title)
	}
}

func TestConfirmCreatesTicketAndAssignsSpecialist(t *testing.T) {
	h := newHarness(t, []*llm.Response{
		toolCall("call-1", ToolCreateConfirm, `{"title":"接口鉴权异常","description":"401 报错","category":"incident","priority":"P1","skillIds":[2,3]}`),
	})

	if first := h.runTurn(t, 200, "建个工单"); !first.Interrupted {
		t.Fatal("应先发起确认")
	}

	// 用户确认：走恢复路径，不再调用模型。
	result := h.runTurn(t, 200, "确认")

	if result.Interrupted {
		t.Fatalf("确认后不应再中断：%s", result.Prompt)
	}
	if result.TicketID == 0 {
		t.Fatal("确认后应创建工单")
	}

	tickets := h.store.ListTickets()
	if len(tickets) != 1 {
		t.Fatalf("期望 1 张工单，实际 %d", len(tickets))
	}
	ticket := tickets[0]
	// 技能 [2,3] 对应接口专才（张伟 101）。
	if ticket.AssigneeID != 101 {
		t.Fatalf("应按技能指派 101，实际 %d", ticket.AssigneeID)
	}
	if ticket.Status != domain.TicketStatusPending {
		t.Errorf("新建工单应为 pending，实际 %s", ticket.Status)
	}

	// 中断状态必须推进，否则用户重复回复会重复建单。
	if _, stillPending := h.store.FindPendingInterrupt(200); stillPending {
		t.Error("已确认的中断不应仍是 pending")
	}
}

func TestCancelDoesNotCreateTicket(t *testing.T) {
	h := newHarness(t, []*llm.Response{
		toolCall("call-1", ToolCreateConfirm, `{"title":"咨询私有化部署","description":"问硬件要求","category":"consultation","priority":"P3"}`),
	})

	if first := h.runTurn(t, 300, "帮我建个工单"); !first.Interrupted {
		t.Fatal("应先发起确认")
	}
	result := h.runTurn(t, 300, "取消")

	if result.TicketID != 0 {
		t.Error("取消后不应创建工单")
	}
	if tickets := h.store.ListTickets(); len(tickets) != 0 {
		t.Fatalf("取消后工单数应为 0，实际 %d", len(tickets))
	}
	if result.Reply == "" {
		t.Error("取消后应给出明确回复")
	}
}

func TestUnknownReplyReasksWithoutCallingModel(t *testing.T) {
	h := newHarness(t, []*llm.Response{
		toolCall("call-1", ToolCreateConfirm, `{"title":"某个问题","description":"描述","category":"incident","priority":"P2"}`),
	})

	if first := h.runTurn(t, 400, "建个工单"); !first.Interrupted {
		t.Fatal("应先发起确认")
	}
	callsBefore := h.model.requestCount()

	// 语义不明的回复不应被猜测为确认或取消。
	result := h.runTurn(t, 400, "我想想")

	if result.TicketID != 0 {
		t.Error("语义不明时不应创建工单")
	}
	if !result.Interrupted {
		t.Error("语义不明时应重新提问")
	}
	// 重新提问不应再消耗模型调用：这是一次纯状态机交互。
	if after := h.model.requestCount(); after != callsBefore {
		t.Errorf("语义不明时不应调用模型，调用次数 %d → %d", callsBefore, after)
	}
	// 中断应保持 pending，使用户仍可确认。
	if _, ok := h.store.FindPendingInterrupt(400); !ok {
		t.Error("语义不明时中断应保持 pending")
	}
}

func TestExpiredInterruptDoesNotCreateTicket(t *testing.T) {
	h := newHarness(t, []*llm.Response{
		toolCall("call-1", ToolCreateConfirm, `{"title":"过期测试","description":"描述","category":"incident","priority":"P2"}`),
	})

	if first := h.runTurn(t, 500, "建个工单"); !first.Interrupted {
		t.Fatal("应先发起确认")
	}

	// 把中断改成已过期。
	pending, _ := h.store.FindPendingInterrupt(500)
	pending.ExpiresAt = h.testsNow.Add(-time.Minute)
	if _, err := h.store.SaveInterrupt(pending); err != nil {
		t.Fatalf("更新中断失败: %v", err)
	}

	result := h.runTurn(t, 500, "确认")

	if result.TicketID != 0 {
		t.Error("过期确认不应创建工单")
	}
	if tickets := h.store.ListTickets(); len(tickets) != 0 {
		t.Fatalf("过期确认后工单数应为 0，实际 %d", len(tickets))
	}
	if result.Reply == "" {
		t.Error("过期应给出明确提示")
	}
}

func TestDuplicateConfirmDoesNotCreateSecondTicket(t *testing.T) {
	h := newHarness(t, []*llm.Response{
		toolCall("call-1", ToolCreateConfirm, `{"title":"幂等测试","description":"描述","category":"incident","priority":"P2"}`),
		finalReply("还有什么可以帮你的？"),
	})

	if first := h.runTurn(t, 600, "建个工单"); !first.Interrupted {
		t.Fatal("应先发起确认")
	}
	firstResult := h.runTurn(t, 600, "确认")
	if firstResult.TicketID == 0 {
		t.Fatal("首次确认应建单")
	}

	// 再次确认：中断已 resolved，不应重复建单。
	h.runTurn(t, 600, "确认")

	if tickets := h.store.ListTickets(); len(tickets) != 1 {
		t.Fatalf("重复确认不应重复建单，期望 1 张，实际 %d", len(tickets))
	}
}

func TestDuplicateTicketInSameConversationIsDeduped(t *testing.T) {
	h := newHarness(t, []*llm.Response{
		toolCall("c1", ToolCreateConfirm, `{"title":"会话去重测试","description":"描述","category":"incident","priority":"P2"}`),
		toolCall("c2", ToolCreateConfirm, `{"title":"会话去重测试","description":"描述","category":"incident","priority":"P2"}`),
	})

	if r := h.runTurn(t, 700, "建个工单"); !r.Interrupted {
		t.Fatal("应先发起确认")
	}
	if r := h.runTurn(t, 700, "确认"); r.TicketID == 0 {
		t.Fatal("首次应建单")
	}

	if r := h.runTurn(t, 700, "再建一个工单"); !r.Interrupted {
		t.Fatal("应先发起确认")
	}
	second := h.runTurn(t, 700, "确认")

	// 同一会话已有未关闭工单：应复用而非新建。
	if tickets := h.store.ListTickets(); len(tickets) != 1 {
		t.Fatalf("同会话重复建单应被去重，期望 1 张，实际 %d", len(tickets))
	}
	if second.TicketID == 0 {
		t.Error("去重后仍应返回被复用的工单 ID")
	}
}

func TestBudgetExceededIsRejectedWithStructuredReason(t *testing.T) {
	// 预算设为 1 次：模型连续调用两次，第二次必须被拒绝，
	// 且拒绝原因可结构化统计（这是评测「预算超限率」的前提）。
	policy := tooling.Policy{MaxTotalCalls: 1, MaxArgumentBytes: 4096, MaxCallsPerTool: 1}
	h := newHarness(t, []*llm.Response{
		toolCall("c1", ToolRAGSearch, `{"query":"接口 401"}`),
		toolCall("c2", ToolRAGSearch, `{"query":"账号锁定"}`),
		finalReply("已尽力检索。"),
	}, withPolicy(policy))

	result := h.runTurn(t, 800, "两个问题")

	if len(result.ToolCalls) < 2 {
		t.Fatalf("应有两次工具调用尝试，实际 %d", len(result.ToolCalls))
	}
	second := result.ToolCalls[1]
	if second.ErrorKind != tooling.KindBudgetExceeded {
		t.Fatalf("第二次调用应因预算超限被拒绝，实际 %s（%s）", second.ErrorKind, second.Result)
	}
	if second.Status != "failed" {
		t.Errorf("被拒绝的调用状态应为 failed，实际 %s", second.Status)
	}
}

func TestUnknownToolIsRejectedNotPanicked(t *testing.T) {
	h := newHarness(t, []*llm.Response{
		toolCall("c1", "make_up_a_tool", `{}`),
		finalReply("我没有这个能力。"),
	})

	result := h.runTurn(t, 900, "试试不存在的工具")

	if len(result.ToolCalls) != 1 {
		t.Fatalf("期望 1 次调用记录，实际 %d", len(result.ToolCalls))
	}
	if result.ToolCalls[0].ErrorKind != tooling.KindUnknownTool {
		t.Fatalf("应归因为 unknown_tool，实际 %s", result.ToolCalls[0].ErrorKind)
	}
	// 拒绝后仍应拿到最终回复，而不是整个回合失败。
	if result.Reply == "" {
		t.Error("工具被拒后仍应给出回复")
	}
}

func TestInvalidArgumentsAreRejectedBeforeHandler(t *testing.T) {
	h := newHarness(t, []*llm.Response{
		toolCall("c1", ToolCreateDraft, `{"title":""}`),
		finalReply("请提供问题标题。"),
	})

	result := h.runTurn(t, 1000, "建单")

	if len(result.ToolCalls) != 1 {
		t.Fatalf("期望 1 次调用记录，实际 %d", len(result.ToolCalls))
	}
	if result.ToolCalls[0].ErrorKind != tooling.KindInvalidArgs {
		t.Fatalf("空必填项应归因为 invalid_args，实际 %s（%s）",
			result.ToolCalls[0].ErrorKind, result.ToolCalls[0].Result)
	}
}

func TestToolRoundsExhaustedGivesFallbackReply(t *testing.T) {
	// 模型持续调用工具不肯收敛：必须给出明确回复而不是空回复。
	responses := []*llm.Response{
		toolCall("c1", ToolRAGSearch, `{"query":"a"}`),
		toolCall("c2", ToolRAGSearch, `{"query":"b"}`),
		toolCall("c3", ToolRAGSearch, `{"query":"c"}`),
		toolCall("c4", ToolRAGSearch, `{"query":"d"}`),
	}
	h := newHarness(t, responses, withPolicy(tooling.Policy{
		MaxTotalCalls: 10, MaxArgumentBytes: 4096, MaxCallsPerTool: 10,
	}))

	result := h.runTurn(t, 1100, "一直检索")

	if result.Reply == "" {
		t.Fatal("轮次用尽也必须给出回复，不能返回空字符串")
	}
	if !containsAny(result.Reply, "人工", "步骤") {
		t.Errorf("轮次用尽时应说明已转人工或步骤过多，实际：%s", result.Reply)
	}
}

func TestWithoutKnowledgeBaseTellsModelToCreateTicket(t *testing.T) {
	h := newHarness(t, []*llm.Response{
		toolCall("c1", ToolRAGSearch, `{"query":"任意问题"}`),
		finalReply("我帮你建个工单。"),
	}, withoutRetriever())

	result := h.runTurn(t, 1200, "问个问题")

	if len(result.ToolCalls) != 1 {
		t.Fatalf("期望 1 次调用，实际 %d", len(result.ToolCalls))
	}
	// 未配置知识库是正常业务状态，不是错误：工具应成功返回并告知模型。
	if result.ToolCalls[0].Status != "completed" {
		t.Fatalf("未配置知识库不应算工具失败，实际 %s", result.ToolCalls[0].Status)
	}
	if result.RAGResult == nil || result.RAGResult.Gate.Sufficient {
		t.Error("未配置知识库时必须判定为证据不充分，避免模型凭空作答")
	}
}

func TestModelErrorIsPropagated(t *testing.T) {
	h := newHarness(t, nil)
	h.model.err = errors.New("上游模型不可用")

	_, err := h.agent.Run(context.Background(), TurnInput{ConversationID: 1, UserMessage: "你好"})
	if err == nil {
		t.Fatal("模型错误必须向上返回，不能静默吞掉")
	}
}

func TestEmptyUserMessageIsRejected(t *testing.T) {
	h := newHarness(t, nil)
	if _, err := h.agent.Run(context.Background(), TurnInput{ConversationID: 1, UserMessage: "   "}); err == nil {
		t.Fatal("空消息应被拒绝")
	}
}

func TestFindOpenTicketToolReportsExistingTicket(t *testing.T) {
	h := newHarness(t, []*llm.Response{
		toolCall("c1", ToolCreateConfirm, `{"title":"已有工单","description":"描述","category":"incident","priority":"P2"}`),
		toolCall("c2", ToolFindOpenTicket, `{"topic":"接口报错"}`),
		finalReply("已有工单在处理中。"),
	})

	if r := h.runTurn(t, 1300, "建个工单"); !r.Interrupted {
		t.Fatal("应先发起确认")
	}
	if r := h.runTurn(t, 1300, "确认"); r.TicketID == 0 {
		t.Fatal("应建单")
	}

	result := h.runTurn(t, 1300, "还有个类似问题")
	if len(result.ToolCalls) == 0 {
		t.Fatal("应记录工具调用")
	}
	// 查重工具必须报告已存在的工单，否则模型会重复建单。
	if !containsAny(result.ToolCalls[0].Result, "\"found\":true") {
		t.Errorf("查重工具应报告已存在工单，实际：%s", result.ToolCalls[0].Result)
	}
}

func TestCreateDraftMarksMissingDescription(t *testing.T) {
	h := newHarness(t, []*llm.Response{
		toolCall("c1", ToolCreateDraft, `{"title":"只有标题"}`),
		finalReply("请补充问题描述。"),
	})

	result := h.runTurn(t, 1400, "建单")
	if len(result.ToolCalls) != 1 {
		t.Fatalf("期望 1 次调用，实际 %d", len(result.ToolCalls))
	}
	// 服务端自行判定必要字段，不采信模型自称完整。
	if !containsAny(result.ToolCalls[0].Result, "description") {
		t.Errorf("草案工具应自行识别缺失的描述字段，实际：%s", result.ToolCalls[0].Result)
	}
}

func containsAny(text string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}
