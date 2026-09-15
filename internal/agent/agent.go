// Package agent 编排一次客服会话回合。
//
// 一期只做「单轮工具编排」而非多步 agent loop（后者排在二期）：
// 模型一次决策可以调用工具，工具结果回灌后再决策一次，最多若干轮。
// 这个区别是刻意的——二期引入真正的 bounded loop 时，
// 工具治理、trace 与中断语义都已在此定型，届时只需替换决策内核。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mac/helpdesk-agent/internal/domain"
	"github.com/mac/helpdesk-agent/internal/llm"
	"github.com/mac/helpdesk-agent/internal/rag"
	"github.com/mac/helpdesk-agent/internal/store"
	"github.com/mac/helpdesk-agent/internal/ticket"
	"github.com/mac/helpdesk-agent/internal/tooling"
)

// 工具编码。
const (
	ToolRAGSearch      = "rag_search"
	ToolFindOpenTicket = "ticket_find_open_by_topic"
	ToolCreateConfirm  = "ticket_create_confirm"
)

// Config 运行配置。
type Config struct {
	// Policy 工具治理策略。
	Policy tooling.Policy
	// MaxToolRounds 工具调用回灌的最大轮次。
	MaxToolRounds int
	// ConfirmTTL 确认中断的有效期。
	//
	// 给足时间且不无限等待：用户可能几小时后才回复，但太久之后
	// 上下文已变（会话可能已被人工接管），继续执行旧确认并不合适。
	ConfirmTTL time.Duration
	// SystemPrompt 覆盖默认系统提示词，主要用于测试与调优。
	SystemPrompt string
}

// DefaultConfig 返回默认运行配置。
//
// MaxToolRounds 取 5：真实模型完成「检索 → 查重 → 起草 → 确认」
// 需要 4 轮决策，取 3 会在最后一步前被打断（实测）。
// 留一轮余量给模型纠错（例如首次检索不理想时换措辞重试）。
func DefaultConfig() Config {
	return Config{
		Policy:        tooling.DefaultPolicy(),
		MaxToolRounds: 5,
		ConfirmTTL:    2 * time.Hour,
	}
}

func (c Config) normalize() Config {
	def := DefaultConfig()
	if c.MaxToolRounds <= 0 {
		c.MaxToolRounds = def.MaxToolRounds
	}
	if c.ConfirmTTL <= 0 {
		c.ConfirmTTL = def.ConfirmTTL
	}
	return c
}

// Agent 一次会话回合的编排器。
type Agent struct {
	model     llm.ChatModel
	registry  *tooling.Registry
	retriever *rag.Retriever
	tickets   *ticket.Service
	store     store.Store
	config    Config
	now       func() time.Time

	// classifier 为可选的意图分类器。
	//
	// 存在时先分类再决定路径：寒暄与无关请求无需检索，也无需把
	// 四个工具的完整 schema 塞进上下文，因此可以短路，
	// 省掉整轮的工具目录与证据成本。
	// 为空时行为与无路由版本一致（全部走完整链路）。
	classifier IntentClassifier
}

// IntentClassifier 判定用户消息意图。
//
// 定义在 agent 包而非 domain：这是编排层需要的协作接口，
// 不应污染领域模型的语义。
type IntentClassifier interface {
	ClassifyIntent(text string) (IntentOutcome, error)
}

// IntentOutcome 一次意图分类的结果。
type IntentOutcome struct {
	Intent domain.Intent
	// Parsed 为 false 表示模型输出无法解析，已回退到默认意图。
	Parsed bool
	Usage  llm.Usage
}

// Option 调整 Agent 行为。
type Option func(*Agent)

// WithClock 注入时钟，用于测试中固定时间。
func WithClock(now func() time.Time) Option {
	return func(a *Agent) { a.now = now }
}

// WithClassifier 注入意图分类器，启用路由短路。
func WithClassifier(classifier IntentClassifier) Option {
	return func(a *Agent) { a.classifier = classifier }
}

// New 构造 Agent。
func New(model llm.ChatModel, st store.Store, retriever *rag.Retriever, tickets *ticket.Service, config Config, opts ...Option) (*Agent, error) {
	if model == nil {
		return nil, errors.New("必须提供模型实现")
	}
	if st == nil {
		return nil, errors.New("必须提供存储实现")
	}
	if tickets == nil {
		return nil, errors.New("必须提供工单服务")
	}

	a := &Agent{
		model:     model,
		retriever: retriever,
		tickets:   tickets,
		store:     st,
		config:    config.normalize(),
		now:       time.Now,
	}
	for _, opt := range opts {
		opt(a)
	}

	registry, err := tooling.NewRegistry(a.buildDefinitions()...)
	if err != nil {
		return nil, fmt.Errorf("构造工具注册表失败: %w", err)
	}
	a.registry = registry
	return a, nil
}

// ToolCodes 返回全部工具编码，供治理策略与测试使用。
func (a *Agent) ToolCodes() []string {
	defs := a.registry.Definitions()
	ret := make([]string, 0, len(defs))
	for _, def := range defs {
		ret = append(ret, def.Code)
	}
	return ret
}

// allowedTools 返回本 Agent 对模型开放的工具白名单。
//
// 只读工具全部开放；写工具也开放——因为模型必须能"发起"建单，
// 只是发起后会落到确认中断，而不是直接写入。
func (a *Agent) allowedTools() []string {
	return a.ToolCodes()
}

// TurnInput 一次会话回合的输入。
type TurnInput struct {
	ConversationID int64
	UserMessage    string
}

// ToolCallRecord 一次工具调用的审计记录。
//
// 落库以便评测工具调用准确率与预算超限率。
type ToolCallRecord struct {
	Code      string
	Risk      tooling.RiskLevel
	Status    string // completed / failed / rejected
	ErrorKind tooling.ErrorKind
	Arguments string
	Result    string
	// DurationMS / DurationUS 分别为毫秒与微秒精度。
	// 双精度原因同 TurnResult：离线评测的单次工具耗时在微秒级，
	// 只报毫秒会全为 0。
	DurationMS int
	DurationUS int
}

// RoundRecord 一轮模型决策的用量与工具归因。
//
// 按轮记录而非只记总数：真实模型单轮 prompt 可达数千 token
// （工具目录 + 证据 + 历史），只有分清钱花在哪一轮才谈得上优化。
type RoundRecord struct {
	Round            int
	PromptTokens     int
	CompletionTokens int
	Tools            []string
	DurationUS       int
}

// TurnResult 一次会话回合的结果。
type TurnResult struct {
	// Intent 为本次路由判定的意图；未启用分类器时为空。
	Intent domain.Intent
	// ShortCircuited 为 true 表示本次未进入工具循环（寒暄/无关请求）。
	ShortCircuited bool
	Reply          string
	// Interrupted 为 true 时表示已发起确认，等待用户下一条消息。
	Interrupted bool
	// CheckPointID 与 Prompt 仅在 Interrupted 时有效。
	CheckPointID string
	Prompt       string

	TicketID  int64
	Usage     llm.Usage
	ToolCalls []ToolCallRecord
	RAGResult *rag.Result
	Rounds    int
	// RoundRecords 保存逐轮明细，供成本归因。
	RoundRecords []RoundRecord
	// DurationMS 为整轮耗时（毫秒）。
	DurationMS int
	// DurationUS 为整轮耗时（微秒）。
	//
	// 同时提供两种精度：真实模型调用耗时在百毫秒级，毫秒足够；
	// 但脚本化模型与内存存储的离线评测耗时在微秒级，
	// 只报毫秒会让延迟指标全部变成 0 而失去意义。
	DurationUS int
}

// Run 处理一条用户消息。
//
// 流程：
//  1. 若存在待确认中断，先按本条消息的语义恢复或取消它（短路返回）。
//  2. 否则进入模型决策循环：模型可调用工具，结果回灌后再次决策。
//  3. 模型给出最终回复，本轮结束。
func (a *Agent) Run(ctx context.Context, input TurnInput) (*TurnResult, error) {
	startedAt := a.now()
	input.UserMessage = strings.TrimSpace(input.UserMessage)
	if input.UserMessage == "" {
		return nil, errors.New("用户消息不能为空")
	}

	// 优先处理未完成的确认：用户这条消息很可能就是在回答确认提问。
	if pending, ok := a.store.FindPendingInterrupt(input.ConversationID); ok {
		return a.resume(ctx, pending, input.UserMessage, startedAt)
	}

	// 先做意图路由再决定路径。
	//
	// 顺序很关键：确认恢复必须先于分类——用户回复「确认」时，
	// 按语义它属于 chitchat（无实质诉求），若先分类会被短路成寒暄回复，
	// 从而丢失建单确认。
	result := &TurnResult{}
	if handled, err := a.routeAndShortCircuit(ctx, input, startedAt, result); err != nil {
		return nil, err
	} else if handled {
		return result, nil
	}

	messages := []llm.Message{{Role: llm.RoleUser, Content: input.UserMessage}}
	schemas := a.toolSchemas()

	counts := make(map[string]int)
	total := 0

	for round := 0; round < a.config.MaxToolRounds; round++ {
		result.Rounds = round + 1

		response, err := a.model.ChatWithTools(ctx, llm.ToolRequest{
			System:   a.systemPrompt(),
			Messages: messages,
			Tools:    schemas,
		})
		if err != nil {
			return nil, fmt.Errorf("模型调用失败: %w", err)
		}
		result.Usage.PromptTokens += response.Usage.PromptTokens
		result.Usage.CompletionTokens += response.Usage.CompletionTokens

		roundRecord := RoundRecord{
			Round:            round + 1,
			PromptTokens:     response.Usage.PromptTokens,
			CompletionTokens: response.Usage.CompletionTokens,
		}

		// 没有工具调用即为最终回复，本轮结束。
		if len(response.ToolCalls) == 0 {
			result.Reply = response.Content
			a.finish(result, startedAt)
			result.DurationUS = int(a.now().Sub(startedAt).Microseconds())
			return result, nil
		}

		// 回灌 assistant 的工具调用意图，再逐条追加工具结果。
		//
		// ReasoningContent 必须一并带回：思维链模型（如 DeepSeek flash）
		// 会在下一轮校验该字段，缺失时直接返回 400，整轮失败。
		messages = append(messages, llm.Message{
			Role:             llm.RoleAssistant,
			Content:          response.Content,
			ToolCalls:        response.ToolCalls,
			ReasoningContent: response.ReasoningContent,
		})

		for _, call := range response.ToolCalls {
			record, observation, interrupt := a.executeTool(ctx, input, call, counts, total)
			result.ToolCalls = append(result.ToolCalls, record)
			roundRecord.Tools = append(roundRecord.Tools, record.Code)

			if interrupt != nil {
				// 写操作需要确认：发起中断并短路返回，等用户下一条消息。
				result.RoundRecords = append(result.RoundRecords, roundRecord)
				result.Interrupted = true
				result.CheckPointID = interrupt.CheckPointID
				result.Prompt = interrupt.Prompt
				result.Reply = interrupt.Prompt
				a.finish(result, startedAt)
				return result, nil
			}

			counts[call.Name]++
			total++
			if record.Code == ToolRAGSearch {
				// 保留检索详情，供评测召回率与失败归因使用。
				if ragResult, ok := a.lastRAGResult(observation); ok {
					result.RAGResult = &ragResult
				}
			}
			messages = append(messages, llm.Message{
				Role: llm.RoleTool, Content: observation, ToolCallID: call.ID,
			})
		}
		result.RoundRecords = append(result.RoundRecords, roundRecord)
	}

	// 轮次用尽仍无最终回复：明确告知而非静默截断，
	// 否则用户会收到空回复，且无人知道模型陷在工具循环里。
	result.Reply = "抱歉，我处理这个问题时步骤过多，已转由人工继续跟进。"
	a.finish(result, startedAt)
	return result, nil
}

// elapsedMS 同时返回毫秒与微秒耗时。
func elapsedMS(now, startedAt time.Time) (int, int) {
	elapsed := now.Sub(startedAt)
	return int(elapsed.Milliseconds()), int(elapsed.Microseconds())
}

// finish 统一写入耗时字段。
//
// 集中在一处而非散落在各返回分支：早期实现手动赋值，新增分支时容易遗漏，
// 结果某些路径的耗时恒为 0，指标静默失真。
func (a *Agent) finish(result *TurnResult, startedAt time.Time) {
	result.DurationMS, result.DurationUS = elapsedMS(a.now(), startedAt)
}

// executeTool 执行一次工具调用，返回审计记录、给模型的观察结果，
// 以及需要确认时的中断信息（此时不执行工具）。
func (a *Agent) executeTool(ctx context.Context, input TurnInput, call llm.ToolCall, counts map[string]int, total int) (ToolCallRecord, string, *domain.Interrupt) {
	startedAt := a.now()
	record := ToolCallRecord{Code: call.Name, Arguments: call.Arguments, Status: "failed"}

	invocation := tooling.Invocation{
		Code:       call.Name,
		Arguments:  call.Arguments,
		Policy:     a.policyWithAllowList(),
		CallCounts: counts,
		TotalCalls: total,
	}
	def, err := a.registry.Authorize(invocation)
	if err != nil {
		record.ErrorKind = tooling.KindOf(err)
		record.Result = err.Error()
		record.DurationMS, record.DurationUS = elapsedMS(a.now(), startedAt)
		// 拒绝也要回灌给模型，让它有机会改正（例如改用别的工具），
		// 而不是直接失败让用户看到错误。
		return record, "工具调用被拒绝：" + err.Error(), nil
	}
	record.Risk = def.Risk

	// 参数校验必须先于确认中断。
	//
	// 早期实现在写操作分支里才解析参数，导致空标题这类非法参数
	// 会先向用户发起确认，用户同意后才在建单时失败——用户白确认一次，
	// 且若校验更宽松就会建出字段缺失的脏数据。
	args, err := tooling.ParseArguments(def, call.Arguments)
	if err != nil {
		record.ErrorKind = tooling.KindOf(err)
		record.Result = err.Error()
		record.DurationMS, record.DurationUS = elapsedMS(a.now(), startedAt)
		return record, "工具参数不合法：" + err.Error(), nil
	}

	handlerCtx := tooling.HandlerContext{Ctx: ctx, ConversationID: input.ConversationID}
	if def.RequireConfirmation {
		// 写操作：先落确认中断，工具本身不执行。
		interrupt, err := a.createInterrupt(input, def, args)
		if err != nil {
			record.ErrorKind = tooling.KindExecFailed
			record.Result = err.Error()
			record.DurationMS, record.DurationUS = elapsedMS(a.now(), startedAt)
			return record, "发起确认失败：" + err.Error(), nil
		}
		record.Status = "awaiting_confirmation"
		record.ErrorKind = tooling.KindNeedsConfirm
		record.Result = interrupt.Prompt
		record.DurationMS, record.DurationUS = elapsedMS(a.now(), startedAt)
		return record, "", interrupt
	}

	observation, err := def.Handler(handlerCtx, args)
	record.DurationMS, record.DurationUS = elapsedMS(a.now(), startedAt)
	if err != nil {
		record.ErrorKind = tooling.KindOf(err)
		record.Result = err.Error()
		return record, "工具执行失败：" + err.Error(), nil
	}
	record.Status = "completed"
	record.Result = observation
	return record, observation, nil
}

// resume 处理用户对确认提问的答复。
//
// 语义：
//   - 明确确认 → 执行写操作，回复结果
//   - 明确取消 → 标记取消，回复取消
//   - 无法识别 → 保留 pending，重新提问（不再调用模型，避免额外成本）
func (a *Agent) resume(ctx context.Context, pending domain.Interrupt, text string, startedAt time.Time) (*TurnResult, error) {
	result := &TurnResult{}

	if err := pending.CanResume(a.now()); err != nil {
		// 已过期或状态异常：标记后回到正常流程，让模型按新消息处理。
		pending.Status = domain.InterruptExpired
		if _, saveErr := a.store.SaveInterrupt(pending); saveErr != nil {
			return nil, saveErr
		}
		result.Interrupted = false
		result.Reply = "该确认已过期，请重新描述你的问题。"
		a.finish(result, startedAt)
		return result, nil
	}

	switch domain.ParseConfirmationDecision(text) {
	case domain.DecisionCancel:
		pending.Status = domain.InterruptCancelled
		pending.ResumeCount++
		if _, err := a.store.SaveInterrupt(pending); err != nil {
			return nil, err
		}
		result.Reply = "已取消本次工单创建。如仍需协助，请继续说明。"
		a.finish(result, startedAt)
		return result, nil

	case domain.DecisionConfirm:
		created, err := a.tickets.Create(pending.Payload)
		pending.ResumeCount++
		if err != nil {
			// 执行失败：保留 pending 让用户可以重试，而不是静默丢弃建单意图。
			if _, saveErr := a.store.SaveInterrupt(pending); saveErr != nil {
				return nil, saveErr
			}
			result.Reply = "工单创建失败，请稍后重试或联系人工客服。"
			a.finish(result, startedAt)
			return result, nil
		}
		pending.Status = domain.InterruptResolved
		pending.ResultTicketID = created.Ticket.ID
		if _, err := a.store.SaveInterrupt(pending); err != nil {
			return nil, err
		}

		result.TicketID = created.Ticket.ID
		result.ToolCalls = append(result.ToolCalls, ToolCallRecord{
			Code: ToolCreateConfirm, Risk: tooling.RiskWrite, Status: "completed",
			Result: fmt.Sprintf("工单 T%d 已创建", created.Ticket.ID),
		})
		result.Reply = a.ticketCreatedReply(created)
		a.finish(result, startedAt)
		return result, nil

	default:
		// 语义不明：不猜测、不消耗模型调用，重新给出确认提问。
		result.Interrupted = true
		result.CheckPointID = pending.CheckPointID
		result.Prompt = fmt.Sprintf("请回复「确认」创建工单，或回复「取消」放弃。\n\n%s", pending.Prompt)
		result.Reply = result.Prompt
		a.finish(result, startedAt)
		return result, nil
	}
}

func (a *Agent) ticketCreatedReply(created ticket.CreateResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "工单已创建：T%d《%s》。", created.Ticket.ID, created.Ticket.Title)

	if created.Assignment.AssigneeID > 0 {
		name := fmt.Sprintf("员工 %d", created.Assignment.AssigneeID)
		if emp, err := a.store.GetEmployee(created.Assignment.AssigneeID); err == nil {
			name = emp.Name
		}
		switch created.Assignment.Outcome {
		case domain.OutcomeMatched:
			fmt.Fprintf(&b, "已按技能匹配指派给 %s。", name)
		case domain.OutcomeFallbackPool:
			// 如实说明未匹配到专长人员，而不是含糊其辞地声称"已指派"。
			fmt.Fprintf(&b, "暂未匹配到专长对口的处理人，已进入待认领池（建议 %s）。", name)
		}
	} else {
		b.WriteString("暂无可用处理人，已进入待认领池。")
	}
	if !created.Ticket.InfoComplete() {
		fmt.Fprintf(&b, "补充信息 %s 会有助于更快解决。",
			strings.Join(created.Ticket.MissingInfo, "、"))
	}
	return b.String()
}

// createInterrupt 为写操作创建确认中断。
func (a *Agent) createInterrupt(input TurnInput, def tooling.Definition, args map[string]any) (*domain.Interrupt, error) {
	payload, prompt, err := a.buildTicketPayload(input.ConversationID, args)
	if err != nil {
		return nil, err
	}

	now := a.now()
	interrupt := domain.Interrupt{
		ConversationID: input.ConversationID,
		Kind:           domain.InterruptTicketCreation,
		Status:         domain.InterruptPending,
		CheckPointID:   fmt.Sprintf("confirm:%d:%d", input.ConversationID, now.UnixNano()),
		Prompt:         prompt,
		Payload:        payload,
		ExpiresAt:      now.Add(a.config.ConfirmTTL),
	}
	if _, err := a.store.SaveInterrupt(interrupt); err != nil {
		return nil, err
	}
	return &interrupt, nil
}

// buildTicketPayload 由工具参数构造建单输入与确认文案。
func (a *Agent) buildTicketPayload(conversationID int64, args map[string]any) (domain.TicketInput, string, error) {
	title := tooling.StringArg(args, "title")
	if title == "" {
		return domain.TicketInput{}, "", errors.New("工单标题不能为空")
	}
	description := tooling.StringArg(args, "description")

	category := domain.Category(tooling.StringArg(args, "category"))
	if !category.Valid() {
		category = domain.CategoryIncident
	}
	priority := domain.Priority(tooling.StringArg(args, "priority"))
	if !priority.Valid() {
		priority = domain.PriorityP3
	}
	missing := tooling.StringSliceArg(args, "missingInfo")

	payload := domain.TicketInput{
		Title:          title,
		Description:    description,
		Category:       category,
		Priority:       priority,
		RequiredSkill:  a.deriveRequiredSkills(args, category),
		ConversationID: conversationID,
		SourceChannel:  "web",
		MissingInfo:    missing,
	}

	var b strings.Builder
	b.WriteString("我将为你创建以下工单，请确认：\n")
	fmt.Fprintf(&b, "标题：%s\n", title)
	if description != "" {
		fmt.Fprintf(&b, "描述：%s\n", description)
	}
	fmt.Fprintf(&b, "类型：%s　优先级：%s\n", category, priority)
	if len(missing) > 0 {
		fmt.Fprintf(&b, "（尚缺信息：%s）\n", strings.Join(missing, "、"))
	}
	b.WriteString("请回复“确认”创建，或回复“取消”放弃。")
	return payload, b.String(), nil
}

// deriveRequiredSkills 推导工单的技能需求。
//
// 优先采信模型给出的 skillIds；为空时退化为按分类的兜底技能集合，
// 保证工单至少能进入候选匹配，而不是因缺少技能需求被直接判为兜底。
func (a *Agent) deriveRequiredSkills(args map[string]any, category domain.Category) domain.SkillSet {
	if ids := int64SliceArg(args, "skillIds"); len(ids) > 0 {
		return domain.NewSkillSet(ids...)
	}
	return fallbackSkillsForCategory(category)
}

// fallbackSkillsForCategory 按分类给出兜底技能。
//
// 这只是保底策略：真实部署应由知识运营维护分类到技能的映射表，
// 而不是把映射硬编码在代码里。此处内联是为了让一期可独立跑通。
func fallbackSkillsForCategory(category domain.Category) domain.SkillSet {
	switch category {
	case domain.CategoryIncident:
		return domain.NewSkillSet(2, 3) // 接口 / 数据库
	case domain.CategoryConsultation:
		return domain.NewSkillSet(8, 1) // 部署 / 网络
	case domain.CategoryRequest:
		return domain.NewSkillSet(9) // 账号权限
	case domain.CategoryChange:
		return domain.NewSkillSet(8) // 部署
	default:
		return domain.NewSkillSet(2)
	}
}

func int64SliceArg(args map[string]any, key string) []int64 {
	value, ok := args[key]
	if !ok {
		return nil
	}
	raw, ok := value.([]any)
	if !ok {
		return nil
	}
	ret := make([]int64, 0, len(raw))
	for _, item := range raw {
		switch number := item.(type) {
		case float64:
			if number > 0 {
				ret = append(ret, int64(number))
			}
		case int64:
			if number > 0 {
				ret = append(ret, number)
			}
		}
	}
	return ret
}

// policyWithAllowList 返回带本 Agent 白名单的策略。
func (a *Agent) policyWithAllowList() tooling.Policy {
	policy := a.config.Policy
	policy.AllowedTools = a.allowedTools()
	return policy
}

// toolSchemas 生成给模型的工具列表。
func (a *Agent) toolSchemas() []llm.ToolSchema {
	defs := a.registry.Definitions()
	ret := make([]llm.ToolSchema, 0, len(defs))
	for _, def := range defs {
		ret = append(ret, llm.ToolSchema{
			Name:        def.Code,
			Description: def.Description,
			Parameters:  def.Parameters,
		})
	}
	return ret
}

// systemPrompt 组装系统提示词。
func (a *Agent) systemPrompt() string {
	if strings.TrimSpace(a.config.SystemPrompt) != "" {
		return a.config.SystemPrompt
	}
	return defaultSystemPrompt
}

// lastRAGResult 由工具观察结果还原检索详情。
//
// 检索结果以 JSON 形式回灌给模型，同时被解析出来供评测使用，
// 避免为评测再跑一次检索（那样会引入不一致）。
func (a *Agent) lastRAGResult(observation string) (rag.Result, bool) {
	var payload struct {
		Sufficient bool    `json:"sufficient"`
		Reason     string  `json:"reason"`
		Message    string  `json:"message"`
		Coverage   float64 `json:"coverage"`
		TopScore   float64 `json:"topScore"`
		HitCount   int     `json:"hitCount"`
	}
	if err := json.Unmarshal([]byte(observation), &payload); err != nil {
		return rag.Result{}, false
	}
	return rag.Result{
		Gate: rag.GateResult{
			Sufficient: payload.Sufficient,
			Reason:     rag.Reason(payload.Reason),
			Message:    payload.Message,
			Coverage:   payload.Coverage,
		},
	}, true
}

const defaultSystemPrompt = `你是一名企业技术支持客服助手。你的职责是先用知识库帮助用户解决问题，无法解决时创建工单。

工作原则：
1. 回答技术问题前，务必先用 rag_search 检索知识库，基于检索到的证据作答。
2. 如果 rag_search 返回 sufficient 为 false，说明知识库不足以回答：
   - reason 为 no_hit 时，说明知识库没有这条知识，不要编造答案，应引导创建工单。
   - reason 为 low_coverage 或 low_score 时，可以先尝试换一种说法再检索一次。
3. 决定创建工单前，先用 ticket_find_open_by_topic 检查是否已有未关闭的同类工单，
   避免重复创建。若已存在，应告知用户已有工单在处理中。
4. 决定建单时直接调用 ticket_create_confirm。信息不足时向用户追问关键项，
   但不要因为缺少次要信息就拒绝创建——把缺失项填进 missingInfo，确认提示会一并展示给用户。
5. 调用 ticket_create_confirm 只会向用户发起确认，不会立即创建。用户确认后才会建单。
   重要：确认需要建单时必须调用 ticket_create_confirm，不要只把工单内容写在回复文本里
   就结束——那样用户会以为工单已经提交，实际并没有创建。
6. 工具不可用时不要假装成功。若工具返回被拒绝或失败，如实告知用户。

回复要求：简洁、专业、直接给出可执行的下一步。不要暴露内部工具名与实现细节。`
