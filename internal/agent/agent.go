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
	"sync"
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
	// HistoryMaxMessages 送给模型的历史消息条数上限（不含当前这条）。
	//
	// 取 0 走默认值；显式取负值表示关闭历史——保留一个开关，
	// 便于对照实验判断某次行为变化是否来自历史注入。
	HistoryMaxMessages int
	// HistoryMaxItemRunes 单条历史消息投影后的 rune 上限。
	HistoryMaxItemRunes int
	// HistoryMaxRunes 历史消息投影后的总 rune 上限。
	//
	// 三重上限不是优化而是硬约束：本地模型默认 4k 上下文，
	// 而系统提示词与工具 schema 已占掉约 850 token。
	HistoryMaxRunes int
}

// DefaultConfig 返回默认运行配置。
//
// MaxToolRounds 取 5：真实模型完成「检索 → 查重 → 起草 → 确认」
// 需要 4 轮决策，取 3 会在最后一步前被打断（实测）。
// 留一轮余量给模型纠错（例如首次检索不理想时换措辞重试）。
//
// 历史窗口取 6 条：约三轮问答，够覆盖「报障 → 追问 → 再报障」这条
// 真实主链，再多就开始挤占检索证据的位置。
func DefaultConfig() Config {
	return Config{
		Policy:              tooling.DefaultPolicy(),
		MaxToolRounds:       10,
		ConfirmTTL:          2 * time.Hour,
		HistoryMaxMessages:  6,
		HistoryMaxItemRunes: 220,
		HistoryMaxRunes:     900,
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
	// 用 == 0 而非 <= 0：负值是"显式关闭历史"的有效输入。
	if c.HistoryMaxMessages == 0 {
		c.HistoryMaxMessages = def.HistoryMaxMessages
	}
	if c.HistoryMaxItemRunes <= 0 {
		c.HistoryMaxItemRunes = def.HistoryMaxItemRunes
	}
	if c.HistoryMaxRunes <= 0 {
		c.HistoryMaxRunes = def.HistoryMaxRunes
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

	// traceability 为可选的可追溯性判定器，启用建单交互（追问 + 忠实度标注）。
	//
	// 为 nil（默认）时建单链路与一期上线时完全一致：模型说建就建，不做槽位
	// 可追溯性门、也不标注描述忠实度。只有显式注入才启用，保证既有评测/测试零改动。
	traceability TraceabilityChecker
	// confirmationJudge 为可选的确认判定器（LLM 主判，见 confirmation.go）。
	// 为 nil 时确认判定回退 domain.ParseConfirmationDecision 关键词阶梯，行为与既有版本一致。
	confirmationJudge ConfirmationJudge
	// observer 为可选的回合事件观察者：把「第几轮 / 调了哪个工具 / 结果如何」实时外显，
	// 减少用户对 agent 内部流程的未知性。为 nil 时不产生任何额外开销。
	observer EventObserver
	// intakeMaxRounds 是单会话追问的安全阀上限（防 bug，非业务规则）。
	intakeMaxRounds int
	// intakeGating 为 true 时才做"缺可追溯证据 → 追问而非直接建单"的门控。
	intakeGating bool

	// intakeMu 保护 intakeStates。
	intakeMu sync.Mutex
	// intakeStates 按会话记录追问进度。一期仅内存持久化（与 store 现状一致），
	// 建单成功后清除；重启后从"没追问过"重来是一期已知且可接受的边界。
	intakeStates map[int64]domain.IntakeProgress
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

// IntakeConfig 配置建单交互（可追溯性判定 + 进展驱动追问）。
type IntakeConfig struct {
	// Checker 为 nil 表示不启用建单交互——建单链路保持一期原样。
	Checker TraceabilityChecker
	// MaxAskRounds 为安全阀上限；<=0 时取 DefaultIntakeMaxAskRounds。
	MaxAskRounds int
	// Gating 为 true 时，缺可追溯证据的阻塞槽位会触发追问而非直接建单。
	// 为 false 时只做事后忠实度标注，不改变"信息不全也建单"的旧行为。
	Gating bool
}

// WithIntake 启用建单交互。见 IntakeConfig。
func WithIntake(cfg IntakeConfig) Option {
	return func(a *Agent) {
		a.traceability = cfg.Checker
		a.intakeMaxRounds = cfg.MaxAskRounds
		if a.intakeMaxRounds <= 0 {
			a.intakeMaxRounds = DefaultIntakeMaxAskRounds
		}
		a.intakeGating = cfg.Checker != nil && cfg.Gating
	}
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
		model:           model,
		retriever:       retriever,
		tickets:         tickets,
		store:           st,
		config:          config.normalize(),
		now:             time.Now,
		intakeMaxRounds: DefaultIntakeMaxAskRounds,
		intakeStates:    make(map[int64]domain.IntakeProgress),
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
	// Stream 为可选的回复文本流式回调：模型产生最终回复文本时逐段调用。
	// 为 nil 时退化为非流式（整段返回后由调用方一次性处理）。
	// 只承载最终回复文本；中间轮次的模型前言若存在也会流式外发。
	Stream func(delta string)
}

// TurnEvent 回合内的一次可观测事件，供交互窗口实时外显 agent 内部流程。
//
// Kind 取值：
//   - "round"   第 Round 轮开始（正在请求模型，模型调用是回合内最耗时的黑洞）
//   - "tool"    一个工具执行完毕，Tool/Status 标明是哪个工具、什么结果
//   - "confirm" 确认判定完成，Detail 为判定结论（confirm/cancel/...）
type TurnEvent struct {
	Kind   string `json:"kind"`
	Round  int    `json:"round"`
	Tool   string `json:"tool"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// EventObserver 回合事件观察者。实现应尽快返回、不阻塞主流程，
// 只做展示或追加日志，不应参与业务决策。
type EventObserver func(TurnEvent)

// WithEventObserver 注入回合事件观察者（如交互窗口的实时进度打印）。
func WithEventObserver(observer EventObserver) Option {
	return func(a *Agent) { a.observer = observer }
}

// emit 派发回合事件：优先构造时注入的 observer（chat.go），其次 ctx 注入的
// StreamSink（SSE）。两者都没有时零开销。
func (a *Agent) emit(ctx context.Context, e TurnEvent) {
	if a.observer != nil {
		a.observer(e)
		return
	}
	if sink := streamSinkFrom(ctx); sink != nil && sink.OnEvent != nil {
		sink.OnEvent(e)
	}
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
	// Interrupted 为 true 表示本轮新发起了一次确认。
	Interrupted bool
	// AwaitingConfirmation 为 true 表示本回合结束后会话仍停在等待确认状态。
	//
	// 两者必须分开：用户在待确认期间追问别的事情时，本轮没有"新发起确认"，
	// 但会话依然在等那句确认。业务终态要的是后者，用 Interrupted 代理会误判。
	AwaitingConfirmation bool
	// CheckPointID 与 Prompt 仅在 Interrupted 时有效。
	CheckPointID string
	Prompt       string
	// Decision 为存在待确认中断时对本条消息的语义判定；无中断时为空。
	//
	// 对外暴露而非只用于内部分支：确认误判此前只能靠"单建没建"倒推，
	// 有了判定值，对话窗口与评测能直接看出是哪一类误判。
	Decision domain.ConfirmationDecision

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
//  1. 若存在待确认中断，先判定这条消息是否就是对该确认的表态。
//     明确确认/取消则就此返回；其余情况带上一段"草案仍未决"的说明回落主循环。
//  2. 主循环里模型可调用工具，结果回灌后再次决策。
//  3. 模型给出最终回复，本轮结束。
func (a *Agent) Run(ctx context.Context, input TurnInput) (*TurnResult, error) {
	startedAt := a.now()
	input.UserMessage = strings.TrimSpace(input.UserMessage)
	if input.UserMessage == "" {
		return nil, errors.New("用户消息不能为空")
	}

	// 确认恢复必须先于路由，但不再无条件短路：
	// 只有明确确认与明确取消才由中断消化，其余一律交给带上下文的模型。
	var prelude string
	var decision domain.ConfirmationDecision
	if pending, ok := a.store.FindPendingInterrupt(input.ConversationID); ok {
		outcome, err := a.resume(ctx, pending, input, startedAt)
		if err != nil {
			return nil, err
		}
		if !outcome.Handled {
			prelude = outcome.Prelude
			decision = outcome.Result.Decision
		} else {
			return outcome.Result, nil
		}
	}

	// 先做意图路由再决定路径。
	//
	// 顺序很关键：确认恢复必须先于分类——用户回复「确认」时，
	// 按语义它属于 chitchat（无实质诉求），若先分类会被短路成寒暄回复，
	// 从而丢失建单确认。
	result := &TurnResult{Decision: decision}
	if prelude == "" {
		if handled, err := a.routeAndShortCircuit(ctx, input, startedAt, result); err != nil {
			return nil, err
		} else if handled {
			return result, nil
		}
	}

	messages := a.turnMessages(input)
	if prelude != "" {
		messages = append([]llm.Message{{Role: llm.RoleUser, Content: prelude}}, messages...)
	}
	return a.runToolLoop(ctx, input, messages, startedAt, result)
}

// callChat 无工具补全，模型支持流式时逐段外发文本。
func (a *Agent) callChat(ctx context.Context, system, user string, onDelta func(string)) (*llm.Response, error) {
	if s, ok := a.model.(llm.StreamingChatModel); ok {
		return s.ChatStream(ctx, system, user, onDelta)
	}
	resp, err := a.model.Chat(ctx, system, user)
	if err != nil {
		return nil, err
	}
	if resp.Content != "" && onDelta != nil {
		onDelta(resp.Content)
	}
	return resp, nil
}

// callChatWithTools 带工具补全，模型支持流式时逐段外发文本。
//
// 非流式回退时把整段内容一次性交给 onDelta，使调用方感知不到两种路径的差异。
func (a *Agent) callChatWithTools(ctx context.Context, req llm.ToolRequest, onDelta func(string)) (*llm.Response, error) {
	if s, ok := a.model.(llm.StreamingChatModel); ok {
		return s.ChatWithToolsStream(ctx, req, onDelta)
	}
	resp, err := a.model.ChatWithTools(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.Content != "" && onDelta != nil {
		onDelta(resp.Content)
	}
	return resp, nil
}

// runToolLoop 模型决策循环：调用模型 → 执行工具 → 回灌观察 → 再次决策。
//
// 收 messages 而非自己构造：确认澄清回合需要在消息里预置未决草案的说明，
// 两处共用同一个循环，治理约束（写工具必经确认、预算上限）才只有一份实现。
func (a *Agent) runToolLoop(ctx context.Context, input TurnInput, messages []llm.Message, startedAt time.Time, result *TurnResult) (*TurnResult, error) {
	schemas := a.toolSchemas()

	counts := make(map[string]int)
	total := 0

	onDelta := a.streamDelta(ctx, input)

	for round := 0; round < a.config.MaxToolRounds; round++ {
		result.Rounds = round + 1
		a.emit(ctx, TurnEvent{Kind: "round", Round: round + 1, Detail: "正在请求模型决策"})

		response, err := a.callChatWithTools(ctx, llm.ToolRequest{
			System:   a.systemPrompt(),
			Messages: messages,
			Tools:    schemas,
		}, onDelta)
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
			a.finish(result, input, startedAt)
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
			a.emit(ctx, TurnEvent{Kind: "tool", Round: round + 1, Tool: record.Code, Status: record.Status, Detail: toolEventDetail(record)})

			if interrupt != nil {
				// 写操作需要确认：发起中断并短路返回，等用户下一条消息。
				result.RoundRecords = append(result.RoundRecords, roundRecord)
				result.Interrupted = true
				result.CheckPointID = interrupt.CheckPointID
				result.Prompt = interrupt.Prompt
				result.Reply = interrupt.Prompt
				a.finish(result, input, startedAt)
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
	a.finish(result, input, startedAt)
	return result, nil
}

// toolEventDetail 取工具事件里值得外显的原因文本。
//
// 只有"未完成"且"有解释价值"的状态才填：failed（拒绝/失败原因）与
// awaiting_user_info（追问原因）。完成的观察结果（检索内容）太长，且会体现在
// 后续回复里，不在工具事件行重复。
func toolEventDetail(record ToolCallRecord) string {
	switch record.Status {
	case "failed", "awaiting_user_info":
		return record.Result
	default:
		return ""
	}
}

// elapsedMS 同时返回毫秒与微秒耗时。
func elapsedMS(now, startedAt time.Time) (int, int) {
	elapsed := now.Sub(startedAt)
	return int(elapsed.Milliseconds()), int(elapsed.Microseconds())
}

// finish 统一写入回合收尾字段：耗时与「会话是否仍在等待确认」。
//
// 集中在一处而非散落在各返回分支：早期实现手动赋值，新增分支时容易遗漏，
// 结果某些路径的耗时恒为 0，指标静默失真。
func (a *Agent) finish(result *TurnResult, input TurnInput, startedAt time.Time) {
	result.DurationMS, result.DurationUS = elapsedMS(a.now(), startedAt)
	result.AwaitingConfirmation = a.awaitingConfirmation(input.ConversationID)
}

// awaitingConfirmation 会话当前是否仍有一份能被确认的草案。
func (a *Agent) awaitingConfirmation(conversationID int64) bool {
	pending, ok := a.store.FindPendingInterrupt(conversationID)
	if !ok {
		return false
	}
	// 用 CanResume 而非只看 Status：过期作废的草案不该被算作"还在等确认"。
	return pending.CanResume(a.now()) == nil
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
		// 建单交互前置钩子（仅在建单确认工具、且注入了判定器时生效）。
		// 默认 traceability 为 nil → 整段跳过，写操作链路与一期完全一致。
		var gate *intakeGate
		if a.traceability != nil && call.Name == ToolCreateConfirm {
			g := a.evaluateIntake(ctx, input.ConversationID, args)
			if g.ask {
				// 缺可追溯证据的阻塞槽位：本轮先向用户追问，不建草案。
				// 把"还缺哪些"作为观察回灌给模型，由它生成一句自然的问句收尾，
				// 不改有界循环本体——这正是把"追问"接进既有工具循环的最小接缝。
				record.Status = "awaiting_user_info"
				record.Result = clarifyObservation(g.askSlots)
				record.DurationMS, record.DurationUS = elapsedMS(a.now(), startedAt)
				return record, record.Result, nil
			}
			gate = &g
		}
		// 写操作：先落确认中断，工具本身不执行。
		interrupt, err := a.createInterrupt(input, def, args, gate)
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

// resumeOutcome 一次确认中断恢复的结果。
type resumeOutcome struct {
	// Result 为本回合结果。Handled 为 false 时只携带 Decision，
	// 其余字段交给回落后的主循环填写。
	Result *TurnResult
	// Handled 为 true 表示这条消息确实是对该确认的表态（明确确认或明确取消），
	// 回合可就此返回。
	Handled bool
	// Prelude 非空表示需要回到主循环，并把这段说明作为一条 user 消息
	// 预置在历史之前。不加这段，模型并不知道有一份草案悬着。
	Prelude string
}

// resume 处理用户对确认提问的答复。
//
// 语义：
//   - 明确确认 → 执行写操作并回复结果（唯一能触达建单的分支）
//   - 明确取消 → 标记取消并回复取消
//   - 其余（新诉求 / 语义不明 / 已过期）→ 交回主循环，带上下文处理
//
// 第三条是本轮修复的核心。旧实现在语义不明时重新回显确认提问并直接返回，
// 既不调模型也不读这条消息的内容，于是"待确认期间用户说的其他话"
// 被静默吞掉（实测连吃两轮 promptTokens=0、rounds=0）。
// 旧注释的理由是"避免额外成本"——省一次模型调用换来丢用户一句话，不划算。
func (a *Agent) resume(ctx context.Context, pending domain.Interrupt, input TurnInput, startedAt time.Time) (resumeOutcome, error) {
	result := &TurnResult{}

	if err := pending.CanResume(a.now()); err != nil {
		// 已过期或状态异常：作废草案，再按普通消息处理。
		// 旧注释在这里写着"回到正常流程"，实现却只回了一句提示——
		// 注释与行为不一致本身就是那个缺陷。
		pending.Status = domain.InterruptExpired
		if _, saveErr := a.store.SaveInterrupt(pending); saveErr != nil {
			return resumeOutcome{}, saveErr
		}
		return resumeOutcome{
			Result: result,
			Prelude: "上一轮那份待确认的工单草案已过期，不要再就它征询确认；" +
				"按用户这条消息正常处理。",
		}, nil
	}

	// LLM 主判（2026-09-25）：带草案与用户原话上下文判定表态；
	// 未注入 judge 或调用失败时 judgeConfirmation 内部回退关键词阶梯。
	decision, judgeUsage := a.judgeConfirmation(ctx, pending, input)
	result.Decision = decision
	result.Usage.PromptTokens += judgeUsage.PromptTokens
	result.Usage.CompletionTokens += judgeUsage.CompletionTokens
	a.emit(ctx, TurnEvent{Kind: "confirm", Detail: string(decision)})

	switch decision {
	case domain.DecisionCancel:
		pending.Status = domain.InterruptCancelled
		pending.ResumeCount++
		if _, err := a.store.SaveInterrupt(pending); err != nil {
			return resumeOutcome{}, err
		}
		result.Reply = "已取消本次工单创建。如仍需协助，请继续说明。"
		a.finish(result, input, startedAt)
		return resumeOutcome{Result: result, Handled: true}, nil

	case domain.DecisionConfirm, domain.DecisionForceCommit:
		if decision == domain.DecisionForceCommit {
			// 用户主动要求"别问了直接建"：尊重其决定，但把这份自主略过追问
			// 的痕迹留在工单上，处理人能看到信息是用户自己选择没补全的。
			pending.Payload.MissingInfo = append(pending.Payload.MissingInfo, missingUserForcedCommit)
		}
		created, err := a.tickets.Create(ctx, pending.Payload)
		pending.ResumeCount++
		if err != nil {
			// 执行失败：保留 pending 让用户可以重试，而不是静默丢弃建单意图。
			if _, saveErr := a.store.SaveInterrupt(pending); saveErr != nil {
				return resumeOutcome{}, saveErr
			}
			result.Reply = "工单创建失败，请稍后重试或联系人工客服。"
			a.finish(result, input, startedAt)
			return resumeOutcome{Result: result, Handled: true}, nil
		}
		pending.Status = domain.InterruptResolved
		pending.ResultTicketID = created.Ticket.ID
		if _, err := a.store.SaveInterrupt(pending); err != nil {
			return resumeOutcome{}, err
		}
		// 建单成功即收尾：清空该会话的追问进度，否则下一件事会继承旧的"已问过"记录。
		a.clearIntakeState(input.ConversationID)

		result.TicketID = created.Ticket.ID
		result.ToolCalls = append(result.ToolCalls, ToolCallRecord{
			Code: ToolCreateConfirm, Risk: tooling.RiskWrite, Status: "completed",
			Result: fmt.Sprintf("工单 T%d 已创建", created.Ticket.ID),
		})
		result.Reply = a.ticketCreatedReply(created)
		a.finish(result, input, startedAt)
		return resumeOutcome{Result: result, Handled: true}, nil

	default:
		// 新诉求或语义不明：草案保持待确认并续期——旧实现把 ExpiresAt
		// 固定在中断创建时刻，多轮澄清会被自己的超时打断。
		// 重问只用一行说明，整份草案交给模型自己复述：旧实现把约 240 rune
		// 的草案原样回显两遍，在 4k 上下文里挤掉的是检索证据。
		pending.ResumeCount++
		pending.ExpiresAt = a.now().Add(a.config.ConfirmTTL)
		if _, err := a.store.SaveInterrupt(pending); err != nil {
			return resumeOutcome{}, err
		}
		return resumeOutcome{
			Result: result,
			Prelude: "上一轮已起草一份工单等待用户确认，但这条消息没有明确表示确认或取消。" +
				"请先回应用户这条消息里的诉求；若需要登记，重新起草并再次征询确认，" +
				"不要拿旧草案直接建单。",
		}, nil
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
			fmt.Fprintf(&b, "已指派给 %s。", name)
		case domain.OutcomeFallbackPool:
			// 如实说明没定到对口负责人，而不是含糊其辞地声称"已指派"。
			fmt.Fprintf(&b, "暂未确定对口负责人，已进入待认领池（建议 %s）。", name)
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
//
// gate 为建单交互的前置判定结果（可追溯性 / 追问进度）；未启用建单交互时为 nil，
// 此时行为与一期完全一致。
func (a *Agent) createInterrupt(input TurnInput, def tooling.Definition, args map[string]any, gate *intakeGate) (*domain.Interrupt, error) {
	payload, prompt, err := a.buildTicketPayload(input.ConversationID, args, gate)
	if err != nil {
		return nil, err
	}

	now := a.now()
	// 同一会话只允许一份待确认草案：先作废旧的。
	// 待确认中断按「最新的 pending」取，旧草案若仍留在 pending，
	// 这份一旦被确认或取消，下一次就会回落到那份用户没见过的草案并据其建单。
	if previous, ok := a.store.FindPendingInterrupt(input.ConversationID); ok {
		previous.Status = domain.InterruptSuperseded
		if _, err := a.store.SaveInterrupt(previous); err != nil {
			return nil, err
		}
	}
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
//
// gate 非空时把建单交互的标注并入 MissingInfo，并在确认文案里列出追溯不到的
// 描述声明（只提示核对、不拦单，见 TICKET_INTAKE §3.3）。
func (a *Agent) buildTicketPayload(conversationID int64, args map[string]any, gate *intakeGate) (domain.TicketInput, string, error) {
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
	var untraced []string
	var progress domain.IntakeProgress
	if gate != nil {
		missing = appendMissingNotes(missing, gate.notes)
		untraced = gate.untracedDescs
		progress = gate.progress
	}

	payload := domain.TicketInput{
		Title:          title,
		Description:    description,
		Category:       category,
		Priority:       priority,
		ConversationID: conversationID,
		SourceChannel:  "web",
		MissingInfo:    missing,
		Intake:         progress,
	}

	var b strings.Builder
	b.WriteString("我将为你创建以下工单，请确认：\n")
	fmt.Fprintf(&b, "标题：%s\n", title)
	if description != "" {
		fmt.Fprintf(&b, "描述：%s\n", description)
	}
	fmt.Fprintf(&b, "类型：%s　优先级：%s\n", category, priority)
	if len(untraced) > 0 {
		// 忠实度提示：把"用户原话里没有、我仍写进描述"的声明显式列出请其核对，
		// 但照常建单——拦单会把判定的假阳变成丢单。
		b.WriteString("以下几处我在您的描述里没直接看到，请核对是否属实：")
		b.WriteString(strings.Join(untraced, "；"))
		b.WriteString("\n")
	}
	if len(missing) > 0 {
		fmt.Fprintf(&b, "（尚缺信息：%s）\n", strings.Join(missing, "、"))
	}
	b.WriteString("请回复“确认”创建，或回复“取消”放弃。")
	return payload, b.String(), nil
}

// appendMissingNotes 去重地把建单交互标注追加进 MissingInfo，保留既有项顺序。
func appendMissingNotes(missing, notes []string) []string {
	if len(notes) == 0 {
		return missing
	}
	seen := make(map[string]bool, len(missing)+len(notes))
	out := make([]string, 0, len(missing)+len(notes))
	for _, item := range missing {
		if !seen[item] {
			seen[item] = true
			out = append(out, item)
		}
	}
	for _, note := range notes {
		if !seen[note] {
			seen[note] = true
			out = append(out, note)
		}
	}
	return out
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
   - reason 为 no_hit 时，说明知识库没有这条知识，不要编造答案，也不要只在回复里
     口头引导用户建单——用户报告了故障或求助时，按第 5 条当轮发起建单确认。
   - reason 为 low_coverage 或 low_score 时，可以先尝试换一种说法再检索一次。
3. 一条消息里同时包含提问与登记诉求时，逐一处理：先检索作答，再对建单诉求发起确认。
4. 决定建单前，先用 ticket_find_open_by_topic 检查是否已有未关闭的同类工单，
   避免重复创建。若已存在，应告知用户已有工单在处理中。
5. 建单时机：用户消息里已有问题描述时，当轮就调用 ticket_create_confirm 发起确认，
   把缺失字段填进 missingInfo——不要因为缺少次要信息就拒绝创建，确认提示会把缺失项一并展示。
   用户仅表达建单意愿（如「帮我建个工单」）但没说问题是什么时，先追问问题现象，
   得到描述后再发起确认，不要带空描述直接建单。
6. 调用 ticket_create_confirm 只会向用户发起确认，不会立即创建。用户确认后才会建单。
   重要：确认需要建单时必须调用 ticket_create_confirm，不要只把工单内容写在回复文本里
   就结束——那样用户会以为工单已经提交，实际并没有创建。
7. 工具不可用时不要假装成功。若工具返回被拒绝或失败，如实告知用户。

回复要求：简洁、专业、直接给出可执行的下一步。不要暴露内部工具名与实现细节。`
