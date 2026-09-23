package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mac/helpdesk-agent/internal/assign"
	"github.com/mac/helpdesk-agent/internal/domain"
	"github.com/mac/helpdesk-agent/internal/llm"
	"github.com/mac/helpdesk-agent/internal/rag"
	"github.com/mac/helpdesk-agent/internal/seed"
	"github.com/mac/helpdesk-agent/internal/store"
	"github.com/mac/helpdesk-agent/internal/ticket"
	"github.com/mac/helpdesk-agent/internal/tooling"
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
	policy     tooling.Policy
	retriever  bool
	config     Config
	classifier IntentClassifier
}

// withoutRetriever 表示不配置知识库，用于验证 no_knowledge 分支。
func withoutRetriever() harnessOption {
	return func(c *harnessConfig) { c.retriever = false }
}

// withPolicy 覆盖治理策略。
func withPolicy(policy tooling.Policy) harnessOption {
	return func(c *harnessConfig) { c.policy = policy }
}

// withClassifier 注入意图分类器。
func withClassifier(classifier IntentClassifier) harnessOption {
	return func(c *harnessConfig) { c.classifier = classifier }
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

	agentOpts := []Option{WithClock(func() time.Time { return now })}
	if cfg.classifier != nil {
		agentOpts = append(agentOpts, WithClassifier(cfg.classifier))
	}
	ag, err := New(model, st, retriever, tickets, config, agentOpts...)
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
	if len(req.Tools) != 3 {
		t.Fatalf("应对模型开放 3 个工具，实际 %d", len(req.Tools))
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

func TestReasoningContentIsPassedBackToNextRound(t *testing.T) {
	// 回归用例：思维链模型（DeepSeek flash 等）要求把上一轮返回的
	// reasoning_content 原样带回，缺失时 API 直接返回 400，
	// 「工具调用 → 回灌 → 再决策」整条链路会失败。
	// 参考实现丢弃了该字段，因此完全无法在这些模型上运行。
	h := newHarness(t, []*llm.Response{
		{
			ToolCalls:        []llm.ToolCall{{ID: "c1", Name: ToolRAGSearch, Arguments: `{"query":"接口 401"}`}},
			ReasoningContent: "思考：先检索知识库确认原因。",
			Usage:            llm.Usage{PromptTokens: 100, CompletionTokens: 40},
		},
		finalReply("请检查令牌是否过期。"),
	})

	h.runTurn(t, 1, "接口返回 401 怎么办")

	// 第二轮请求里必须带着上一轮的思考过程。
	req := h.model.lastRequest()
	var found bool
	for _, msg := range req.Messages {
		if msg.Role == llm.RoleAssistant && msg.ReasoningContent == "思考：先检索知识库确认原因。" {
			found = true
		}
	}
	if !found {
		t.Fatal("工具调用后的 assistant 消息必须携带 reasoning_content，否则思维链模型会拒绝请求")
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

// TestUnknownReplyGoesToModelWithPendingContext 锁住 D2 修复后的契约。
//
// 这里原先的断言是"语义不明时不应调用模型、只重新回显确认提问"——
// 那条纯状态机路径正是缺陷本身：待确认期间用户说的其他话被静默吞掉
// （实测 promptTokens=0、rounds=0）。省一次调用换来丢一句话，不划算。
func TestUnknownReplyGoesToModelWithPendingContext(t *testing.T) {
	h := newHarness(t, []*llm.Response{
		toolCall("call-1", ToolCreateConfirm, `{"title":"某个问题","description":"描述","category":"incident","priority":"P2"}`),
		finalReply("好，那这份先不动；你说的另一件事我再帮你登记。"),
	})

	if first := h.runTurn(t, 400, "建个工单"); !first.Interrupted {
		t.Fatal("应先发起确认")
	}
	callsBefore := h.model.requestCount()

	result := h.runTurn(t, 400, "我想想，另外端口怎么开")

	if result.TicketID != 0 {
		t.Error("语义不明时不应创建工单")
	}
	if result.Decision != domain.DecisionUnknown {
		t.Errorf("应记录判定为 unknown，实际 %s", result.Decision)
	}
	// 关键区别：这条消息必须真的被模型读到，而不是被一行固定文案挡掉。
	if after := h.model.requestCount(); after != callsBefore+1 {
		t.Errorf("语义不明时应交回模型，调用次数 %d → %d", callsBefore, after)
	}
	request := h.model.lastRequest()
	if !containsMessage(request.Messages, "我想想，另外端口怎么开") {
		t.Errorf("用户原话必须进模型请求，实际消息 %+v", contents(request.Messages))
	}
	if !hasPendingDraftNote(request.Messages) {
		t.Error("模型必须被告知仍有一份未决草案，否则它会重复起草或忘掉确认")
	}
	// 中断保持 pending，用户之后仍可一句「确认」完成建单。
	if _, ok := h.store.FindPendingInterrupt(400); !ok {
		t.Error("语义不明时中断应保持 pending")
	}
}

// TestOnlyExplicitConfirmCreatesTicket 是写操作的唯一入口这条不变式的落库版断言：
// 不看回复文案、不看调用次数，只看工单表——四种判定里只有明确确认能建出单。
func TestOnlyExplicitConfirmCreatesTicket(t *testing.T) {
	cases := []struct {
		name     string
		reply    string
		want     domain.ConfirmationDecision
		wantCall bool
	}{
		{"明确确认", "确认", domain.DecisionConfirm, false},
		{"明确取消", "取消", domain.DecisionCancel, false},
		{"语义不明", "我想想", domain.DecisionUnknown, true},
		{"夹带新诉求", "还是没弄好，帮我建个单跟进", domain.DecisionHasNewDemand, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, []*llm.Response{
				toolCall("call-1", ToolCreateConfirm, `{"title":"不变式用例","description":"描述","category":"incident","priority":"P2"}`),
				finalReply("收到，我先不动这份草案。"),
			})
			if first := h.runTurn(t, 900, "建个工单"); !first.Interrupted {
				t.Fatal("应先发起确认")
			}
			createdByDraft := len(h.store.ListTickets())
			if createdByDraft != 0 {
				t.Fatalf("确认前应无工单，实际 %d", createdByDraft)
			}

			result := h.runTurn(t, 900, tc.reply)
			if result.Decision != tc.want {
				t.Errorf("判定应为 %s，实际 %s", tc.want, result.Decision)
			}
			// 只有"明确确认"这一格允许出现工单，其余三格建出单都是越界。
			if got := len(h.store.ListTickets()); (got > 0) != (tc.want == domain.DecisionConfirm) {
				t.Errorf("工单数 %d 与判定 %s 不匹配：只有明确确认可建单", got, tc.want)
			}
			if calls := h.model.requestCount(); calls != 1+boolToInt(tc.wantCall) {
				t.Errorf("模型调用次数 %d 不符：该分支应交回模型=%v", calls, tc.wantCall)
			}
		})
	}
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// containsMessage 判断请求消息中是否有内容包含原文的条目。
func containsMessage(messages []llm.Message, needle string) bool {
	for _, m := range messages {
		if strings.Contains(m.Content, needle) {
			return true
		}
	}
	return false
}

// hasPendingDraftNote 判断模型是否被告知了草案的状态（未决或已过期）。
func hasPendingDraftNote(messages []llm.Message) bool {
	return containsMessage(messages, "待确认的工单草案") || containsMessage(messages, "一份工单等待用户确认")
}

func TestExpiredInterruptDoesNotCreateTicket(t *testing.T) {
	h := newHarness(t, []*llm.Response{
		toolCall("call-1", ToolCreateConfirm, `{"title":"过期测试","description":"描述","category":"incident","priority":"P2"}`),
		finalReply("那份草案已经超时作废了，我重新帮你看看现在的问题。"),
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
	// 过期草案必须就此失效，不能留在 pending 里等下一次被"确认"。
	if _, ok := h.store.FindPendingInterrupt(500); ok {
		t.Error("过期后不应仍有待确认中断")
	}
	// 旧实现在这里只回一句"已过期"就把消息丢了；新契约是交回模型继续处理。
	if calls := h.model.requestCount(); calls != 2 {
		t.Errorf("过期后这条消息应交回模型，实际调用 %d 次", calls)
	}
	if !containsMessage(h.model.lastRequest().Messages, "已过期") {
		t.Error("模型必须被告知那份草案已过期，否则它可能继续就旧草案征询确认")
	}
}

// TestRedraftedInterruptSupersedesPrevious 守住一份幽灵确认隐患：
// 同一会话重新起草后，旧草案必须作废。待确认中断取的是「最新的 pending」，
// 旧草案若仍是 pending，新草案一旦被确认，下一次查找就回落到那份
// 用户从没见过的草案——他再说「确认」就凭空多出一张单。
func TestRedraftedInterruptSupersedesPrevious(t *testing.T) {
	h := newHarness(t, []*llm.Response{
		toolCall("call-1", ToolCreateConfirm, `{"title":"第一份草案","description":"描述","category":"incident","priority":"P2"}`),
		toolCall("call-2", ToolCreateConfirm, `{"title":"登录一直转圈","description":"描述","category":"incident","priority":"P1"}`),
	})

	if first := h.runTurn(t, 700, "建个工单"); !first.Interrupted {
		t.Fatal("第一轮应发起确认")
	}
	// 用户没批准第一份，而是提出了新的登记诉求 → 模型重新起草。
	second := h.runTurn(t, 700, "还是没弄好，帮我建个单跟进登录问题")
	if !second.Interrupted {
		t.Fatal("第二轮应就新草案再次确认")
	}
	if got := len(h.store.ListTickets()); got != 0 {
		t.Fatalf("两轮都不该建单，实际 %d 张", got)
	}

	result := h.runTurn(t, 700, "确认")
	tickets := h.store.ListTickets()
	if len(tickets) != 1 {
		t.Fatalf("应只建出一张单，实际 %d", len(tickets))
	}
	if tickets[0].Title != "登录一直转圈" {
		t.Errorf("建出的必须是用户见过并确认的那份，实际《%s》", tickets[0].Title)
	}
	if result.TicketID != tickets[0].ID {
		t.Errorf("返回的工单 ID %d 与落库 %d 不一致", result.TicketID, tickets[0].ID)
	}
	// 关键：确认后不得再有任何待确认中断（旧草案已被取代，不是仍挂着）。
	if stale, ok := h.store.FindPendingInterrupt(700); ok {
		t.Errorf("确认后仍残留待确认中断《%s》", stale.Payload.Title)
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
		toolCall("c1", ToolCreateConfirm, `{"title":""}`),
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

func TestConfirmRejectsEmptyRequiredField(t *testing.T) {
	// 回归用例：参数校验必须先于确认中断。
	//
	// 早期实现在写操作分支里才解析参数，空标题会先向用户发起确认，
	// 用户同意后才在建单时失败——用户白确认一次，还可能建出脏数据。
	h := newHarness(t, []*llm.Response{
		toolCall("c1", ToolCreateConfirm, `{"title":""}`),
		finalReply("请提供问题标题。"),
	})

	result := h.runTurn(t, 1400, "建单")
	if len(result.ToolCalls) != 1 {
		t.Fatalf("期望 1 次调用，实际 %d", len(result.ToolCalls))
	}
	if result.ToolCalls[0].ErrorKind != tooling.KindInvalidArgs {
		t.Errorf("空标题应归因为 invalid_args，实际 %s", result.ToolCalls[0].ErrorKind)
	}
	if result.Interrupted {
		t.Error("参数非法时不应发起确认——用户不应为一个必然失败的操作做确认")
	}
	if _, pending := h.store.FindPendingInterrupt(1400); pending {
		t.Error("参数非法时不应留下待确认中断")
	}
}

func TestPartialInfoStillAllowsTicketCreation(t *testing.T) {
	// 移除 ticket_create_draft 后必须保住的行为：信息不全不阻塞建单。
	//
	// 仅有标题、没有描述时仍应发起确认——真实客服场景中用户常无法一次说清，
	// 卡住不建单会让问题丢失。缺失项通过 missingInfo 展示给用户。
	h := newHarness(t, []*llm.Response{
		toolCall("c1", ToolCreateConfirm,
			`{"title":"只有标题的问题","missingInfo":["description","复现步骤"]}`),
	})

	result := h.runTurn(t, 1500, "帮我建个工单")

	if !result.Interrupted {
		t.Fatal("信息不全但标题有效时应仍能发起确认")
	}
	if result.TicketID != 0 {
		t.Error("确认前不应创建工单")
	}

	pending, ok := h.store.FindPendingInterrupt(1500)
	if !ok {
		t.Fatal("应落库待确认中断")
	}
	if len(pending.Payload.MissingInfo) != 2 {
		t.Errorf("缺失信息应被带入建单载荷，实际 %v", pending.Payload.MissingInfo)
	}
	// 缺失项必须展示给用户，否则用户不知道还要补充什么。
	if !containsAny(result.Prompt, "description") {
		t.Errorf("确认提示应列出缺失信息，实际：%s", result.Prompt)
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
