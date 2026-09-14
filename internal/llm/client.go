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
		Model: m.config.Model,
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
	return response, nil
}

func (m *OpenAIModel) applySampling(params *openai.ChatCompletionNewParams) {
	if m.config.MaxTokens > 0 {
		params.MaxCompletionTokens = openai.Int(m.config.MaxTokens)
	}
	if m.config.Temperature > 0 {
		params.Temperature = openai.Float(m.config.Temperature)
	}
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
