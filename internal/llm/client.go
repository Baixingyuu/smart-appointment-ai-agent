// Package llm 封装大模型调用。
//
// 设计要点：
//  1. 用官方 OpenAI Go SDK，通过 BaseURL 接入任意 OpenAI-compatible 服务，
//     不自己实现 HTTP 客户端。
//  2. 对外只暴露 ChatModel 接口而非具体类型：测试可注入假模型，
//     使全部业务逻辑可在无网络、无 API Key 的情况下确定性验证。
//  3. 每次调用都回传 token 用量：成本是评测的四个轴之一，
//     若框架把用量藏在内部，成本就无法归因（参考实现即栽在这里）。
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// Config 模型配置。
type Config struct {
	BaseURL     string
	APIKey      string
	Model       string
	MaxTokens   int64
	Temperature float64
	TimeoutMS   int
}

// Validate 校验必填项。
func (c Config) Validate() error {
	if strings.TrimSpace(c.APIKey) == "" {
		return errors.New("缺少 API Key")
	}
	if strings.TrimSpace(c.Model) == "" {
		return errors.New("缺少模型名称")
	}
	return nil
}

// Message 一条对话消息。
type Message struct {
	Role    string // system / user / assistant / tool
	Content string
	// ToolCalls 仅在 assistant 消息上出现。
	ToolCalls []ToolCall
	// ToolCallID 仅在 tool 消息上出现，指向被回应的调用。
	ToolCallID string
	// ReasoningContent 是思维链模型的思考过程。
	//
	// 必须原样回传：DeepSeek 的 thinking 模型会在下一轮请求中校验该字段，
	// 缺失时直接返回 400 invalid_request_error
	// （"The `reasoning_content` in the thinking mode must be passed back to the API."）。
	// 因此「工具调用 → 结果回灌 → 再决策」这条链路在思维链模型上
	// 依赖该字段，丢一次就整轮失败。
	ReasoningContent string
}

// 消息角色常量。
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// ToolCall 模型发起的一次工具调用。
type ToolCall struct {
	ID        string
	Name      string
	Arguments string // 原始 JSON 字符串，由工具自行解析并校验
}

// ToolSchema 工具的参数 schema。
//
// 必须给出真实 schema 而非 {"additionalProperties": true}：
// 参考实现把所有兼容别名的参数 schema 写成泛型对象，模型看不到参数含义，
// 只能靠猜，工具调用准确率因此无法提升。
type ToolSchema struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// Usage token 用量。
type Usage struct {
	PromptTokens     int
	CompletionTokens int
}

// Total 返回总 token 数。
func (u Usage) Total() int { return u.PromptTokens + u.CompletionTokens }

// Response 一次模型调用的结果。
type Response struct {
	Content   string
	ToolCalls []ToolCall
	Usage     Usage
	Model     string
	// ReasoningContent 由思维链模型返回的思考过程，需在后续请求中回传。
	ReasoningContent string
}

// ChatModel 定义模型能力。
//
// Chat 为无工具的纯文本补全；ChatWithTools 支持函数调用。
// 两个方法分开而非合成一个：RAG 生成与工具决策的失败处理与用量统计
// 要求不同，显式区分可避免调用方误传空工具列表。
type ChatModel interface {
	Chat(ctx context.Context, system, user string) (*Response, error)
	ChatWithTools(ctx context.Context, req ToolRequest) (*Response, error)
}

// ToolRequest 一次带工具的模型调用请求。
type ToolRequest struct {
	System   string
	Messages []Message
	Tools    []ToolSchema
}

// OpenAIModel 基于官方 SDK 的实现。
type OpenAIModel struct {
	client openai.Client
	config Config
}

// New 构造模型客户端。
//
// BaseURL 为空时使用 SDK 默认地址，便于直接接 OpenAI 官方服务。
func New(config Config) (*OpenAIModel, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	opts := []option.RequestOption{option.WithAPIKey(config.APIKey)}
	if strings.TrimSpace(config.BaseURL) != "" {
		opts = append(opts, option.WithBaseURL(strings.TrimSpace(config.BaseURL)))
	}
	return &OpenAIModel{client: openai.NewClient(opts...), config: config}, nil
}

// ModelName 返回配置的模型名。
func (m *OpenAIModel) ModelName() string { return m.config.Model }

// Chat 执行一次无工具的文本补全。
func (m *OpenAIModel) Chat(ctx context.Context, system, user string) (*Response, error) {
	params := openai.ChatCompletionNewParams{
		Model:    shared.ChatModel(m.config.Model),
		Messages: m.buildMessages(system, []Message{{Role: RoleUser, Content: user}}, false),
	}
	m.applySampling(&params)

	completion, err := m.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("模型调用失败: %w", err)
	}
	if len(completion.Choices) == 0 {
		return nil, errors.New("模型返回空 choices")
	}
	return &Response{
		Content: strings.TrimSpace(completion.Choices[0].Message.Content),
		Usage: Usage{
			PromptTokens:     int(completion.Usage.PromptTokens),
			CompletionTokens: int(completion.Usage.CompletionTokens),
		},
		Model: m.config.Model,
	}, nil
}

// ChatWithTools 执行一次带工具的模型调用。
func (m *OpenAIModel) ChatWithTools(ctx context.Context, req ToolRequest) (*Response, error) {
	if len(req.Tools) == 0 {
		return nil, errors.New("ChatWithTools 需要至少一个工具；无工具场景请用 Chat")
	}
	params := openai.ChatCompletionNewParams{
		Model:    shared.ChatModel(m.config.Model),
		Messages: m.buildMessages(req.System, req.Messages, true),
		Tools:    m.buildTools(req.Tools),
	}
	m.applySampling(&params)

	completion, err := m.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("模型调用失败: %w", err)
	}
	if len(completion.Choices) == 0 {
		return nil, errors.New("模型返回空 choices")
	}

	message := completion.Choices[0].Message
	response := &Response{
		Content: strings.TrimSpace(message.Content),
		Usage: Usage{
			PromptTokens:     int(completion.Usage.PromptTokens),
			CompletionTokens: int(completion.Usage.CompletionTokens),
		},
		Model:            m.config.Model,
		ReasoningContent: extractReasoningContent(completion.RawJSON()),
	}
	for _, call := range message.ToolCalls {
		function := call.AsFunction()
		if strings.TrimSpace(function.Function.Name) == "" {
			continue
		}
		response.ToolCalls = append(response.ToolCalls, ToolCall{
			ID:        function.ID,
			Name:      function.Function.Name,
			Arguments: function.Function.Arguments,
		})
	}

	// 空响应必须报错，不能当正常回复返回。
	//
	// 实测踩过：推理服务异常时（如上下文溢出、模型进程卡死）会返回
	// content 为空、无 tool_calls、usage 全 0 的响应。若按正常响应处理，
	// Agent 会把它当作「最终回复」，评测静默记 0 分——一整轮 live 评测
	// 的失败会被误读成「模型能力差」，而真正原因是服务故障。
	// 报错后由调用方的重试/告警机制接手。
	if response.Content == "" && len(response.ToolCalls) == 0 && response.ReasoningContent == "" {
		return nil, errors.New("模型返回空响应（无内容、无工具调用、无思考内容），疑似推理服务异常")
	}
	return response, nil
}

func (m *OpenAIModel) applySampling(params *openai.ChatCompletionNewParams) {
	if m.config.MaxTokens > 0 {
		params.MaxCompletionTokens = openai.Int(m.config.MaxTokens)
	}
	// 温度始终显式下发，缺省为 0。
	//
	// 此前的实现只在 Temperature > 0 时才发送该字段，缺省走服务端默认值，
	// 同一份 live 评测在不同服务商/不同默认配置下不可比，可复现性上限被锁死。
	// 统一为 0（尽量确定性）是评测基线的前提；个别拒绝 temperature 参数的
	// 模型再按需放开。
	params.Temperature = openai.Float(m.config.Temperature)
}

func (m *OpenAIModel) buildTools(schemas []ToolSchema) []openai.ChatCompletionToolUnionParam {
	ret := make([]openai.ChatCompletionToolUnionParam, 0, len(schemas))
	for _, schema := range schemas {
		parameters := schema.Parameters
		if parameters == nil {
			// 无参数工具显式声明空对象 schema，而不是放任为 nil：
			// nil 会让部分服务端拒绝请求。
			parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		ret = append(ret, openai.ChatCompletionToolUnionParam{
			OfFunction: &openai.ChatCompletionFunctionToolParam{
				Function: shared.FunctionDefinitionParam{
					Name:        schema.Name,
					Description: openai.String(schema.Description),
					Parameters:  shared.FunctionParameters(parameters),
				},
			},
		})
	}
	return ret
}

func (m *OpenAIModel) buildMessages(system string, messages []Message, withTools bool) []openai.ChatCompletionMessageParamUnion {
	ret := make([]openai.ChatCompletionMessageParamUnion, 0, len(messages)+1)
	if strings.TrimSpace(system) != "" {
		ret = append(ret, openai.SystemMessage(system))
	}
	for _, msg := range messages {
		switch msg.Role {
		case RoleAssistant:
			assistant := openai.AssistantMessage(msg.Content)
			// 思维链模型要求把上一轮的 reasoning_content 原样带回，
			// 否则整个请求被拒。SDK 未建模该字段，用 extra fields 透传。
			if msg.ReasoningContent != "" {
				assistant.OfAssistant.SetExtraFields(map[string]any{
					"reasoning_content": msg.ReasoningContent,
				})
			}
			if withTools && len(msg.ToolCalls) > 0 {
				calls := make([]openai.ChatCompletionMessageToolCallUnionParam, 0, len(msg.ToolCalls))
				for _, call := range msg.ToolCalls {
					calls = append(calls, openai.ChatCompletionMessageToolCallUnionParam{
						OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
							ID: call.ID,
							Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
								Name:      call.Name,
								Arguments: call.Arguments,
							},
						},
					})
				}
				assistant.OfAssistant.ToolCalls = calls
			}
			ret = append(ret, assistant)
		case RoleTool:
			ret = append(ret, openai.ToolMessage(msg.Content, msg.ToolCallID))
		case RoleSystem:
			ret = append(ret, openai.SystemMessage(msg.Content))
		default:
			ret = append(ret, openai.UserMessage(msg.Content))
		}
	}
	return ret
}

// extractReasoningContent 从原始响应 JSON 中提取 reasoning_content。
//
// 为什么要读原始 JSON：该字段是 DeepSeek 等思维链模型的扩展，
// 官方 SDK 未为其建模，结构化字段里取不到。
// 解析失败不影响主流程——最坏情况是下一轮因缺少该字段被拒，
// 而那会以明确的 API 错误暴露出来，比在解析处静默失败更容易定位。
func extractReasoningContent(rawJSON string) string {
	if strings.TrimSpace(rawJSON) == "" {
		return ""
	}
	var envelope struct {
		Choices []struct {
			Message struct {
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(rawJSON), &envelope); err != nil {
		return ""
	}
	if len(envelope.Choices) == 0 {
		return ""
	}
	return envelope.Choices[0].Message.ReasoningContent
}
