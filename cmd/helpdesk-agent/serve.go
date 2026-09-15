package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mac/helpdesk-agent/internal/agent"
	"github.com/mac/helpdesk-agent/internal/api"
	"github.com/mac/helpdesk-agent/internal/assign"
	"github.com/mac/helpdesk-agent/internal/classify"
	"github.com/mac/helpdesk-agent/internal/conversation"
	"github.com/mac/helpdesk-agent/internal/evalrun"
	"github.com/mac/helpdesk-agent/internal/llm"
	"github.com/mac/helpdesk-agent/internal/rag"
	"github.com/mac/helpdesk-agent/internal/seed"
	"github.com/mac/helpdesk-agent/internal/store"
	"github.com/mac/helpdesk-agent/internal/ticket"
)

// runServe 启动 HTTP 服务。
//
// 两种模式：
//   - 真实模型：提供 -api-key（或设置 LLM_API_KEY 等环境变量）
//   - 离线脚本：加 -offline，无需任何密钥
//
// 模式会写入 /api/health 并在启动日志中明确标注。
// 这样做的原因：离线脚本模型是「照着预设回复演」的，
// 若不标注，使用者会把演示效果误当作真实模型能力。
func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", ":8080", "监听地址")
	offline := fs.Bool("offline", false, "使用离线脚本模型（无需 API Key，仅用于链路演示）")

	baseURL := fs.String("base-url", envOr("LLM_BASE_URL", ""), "模型服务地址（OpenAI-compatible）")
	apiKey := fs.String("api-key", envOr("LLM_API_KEY", ""), "模型 API Key")
	modelName := fs.String("model", envOr("LLM_MODEL", "gpt-4o-mini"), "模型名称")

	embedBaseURL := fs.String("embed-base-url", envOr("EMBED_BASE_URL", ""), "向量模型服务地址")
	embedAPIKey := fs.String("embed-api-key", envOr("EMBED_API_KEY", ""), "向量模型 API Key")
	embedModel := fs.String("embed-model", envOr("EMBED_MODEL", ""), "向量模型名称")

	if err := fs.Parse(args); err != nil {
		return err
	}

	st := store.NewMemory()
	if err := seed.Load(st); err != nil {
		return err
	}
	tickets := ticket.New(st, assign.New(assign.DefaultWeights()))

	chatModel, embedder, mode, err := buildModel(*offline, llmConfig{
		baseURL: *baseURL, apiKey: *apiKey, model: *modelName,
		embedBaseURL: *embedBaseURL, embedAPIKey: *embedAPIKey, embedModel: *embedModel,
	})
	if err != nil {
		return err
	}

	// 未配置向量模型时使用 BM25 检索（离线可用、无需密钥）；
	// 配置了向量模型则用稠密向量。
	retriever := seed.NewBM25Retriever(rag.DefaultOptions())
	if embedder != nil {
		retriever = rag.New(seed.KnowledgeChunks(), embedder, rag.DefaultOptions())
	}

	// 注入意图分类器以启用路由短路：寒暄与无关请求无需走完整工具链路。
	classifier := classify.New(chatModel)
	ag, err := agent.New(chatModel, st, retriever, tickets, agent.DefaultConfig(),
		agent.WithClassifier(evalrun.NewClassifierRunner(classifier)))
	if err != nil {
		return err
	}

	// 会话编排把「客户消息 → Agent 回合 → 回复落库」串起来。
	turnExecutor := conversation.TurnExecutorFunc(func(conversationID int64, message string) (conversation.TurnOutcome, error) {
		result, err := ag.Run(context.Background(), agent.TurnInput{
			ConversationID: conversationID,
			UserMessage:    message,
		})
		if err != nil {
			return conversation.TurnOutcome{}, err
		}
		return conversation.TurnOutcome{
			Reply:       result.Reply,
			Interrupted: result.Interrupted,
			TicketID:    result.TicketID,
		}, nil
	})

	conversations := conversation.New(st, turnExecutor)
	server := api.New(st, conversations, tickets, mode)

	httpServer := &http.Server{
		Addr:    *addr,
		Handler: server.Handler(),
		// 超时是必须的：真实模型调用可能长时间挂起，
		// 没有超时的服务会被少量慢请求耗尽连接。
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      180 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	fmt.Printf("helpdesk-agent 已启动\n")
	fmt.Printf("  监听地址  http://localhost%s\n", *addr)
	fmt.Printf("  模型模式  %s\n", mode)
	fmt.Printf("  端点\n")
	fmt.Printf("    GET  /api/health\n")
	fmt.Printf("    POST /api/conversations\n")
	fmt.Printf("    GET  /api/conversations\n")
	fmt.Printf("    GET  /api/conversations/{id}\n")
	fmt.Printf("    POST /api/conversations/{id}/messages\n")
	fmt.Printf("    POST /api/conversations/{id}/close\n")
	fmt.Printf("    GET  /api/tickets  |  GET /api/tickets/{id}\n")
	fmt.Printf("    GET  /api/skills   |  GET /api/employees\n")
	if *offline {
		fmt.Printf("\n注意：当前为离线脚本模型模式，回复来自预设脚本，\n")
		fmt.Printf("不代表真实模型的对话与工具调用能力。接入真实模型请提供 -api-key。\n")
	}

	// 优雅关闭：等待在途请求完成，避免截断正在执行的 Agent 回合。
	errCh := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return fmt.Errorf("服务启动失败: %w", err)
	case <-stop:
		fmt.Println("\n正在关闭...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpServer.Shutdown(ctx)
	}
}

type llmConfig struct {
	baseURL      string
	apiKey       string
	model        string
	embedBaseURL string
	embedAPIKey  string
	embedModel   string
}

// buildModel 依配置构造模型与向量化实现，并返回模式说明。
func buildModel(offline bool, cfg llmConfig) (llm.ChatModel, rag.Embedder, string, error) {
	if offline {
		return &offlineModel{}, nil, "离线脚本（无真实模型调用）", nil
	}
	if cfg.apiKey == "" {
		return nil, nil, "", errors.New(
			"缺少模型 API Key。请提供 -api-key 或设置 LLM_API_KEY；" +
				"若只想验证链路，可加 -offline 使用离线脚本模型")
	}

	chatModel, err := llm.New(llm.Config{
		BaseURL: cfg.baseURL, APIKey: cfg.apiKey, Model: cfg.model,
	})
	if err != nil {
		return nil, nil, "", err
	}

	// 未单独配置向量模型时使用 BM25 检索：离线可用、无需额外密钥，
	// 对中文客服 FAQ 这类词面重叠场景效果好。
	if cfg.embedAPIKey == "" || cfg.embedModel == "" {
		return chatModel, nil, "真实模型 + BM25 检索", nil
	}

	embedder, err := rag.NewOpenAIEmbedder(rag.EmbedderConfig{
		BaseURL: cfg.embedBaseURL, APIKey: cfg.embedAPIKey, Model: cfg.embedModel,
	})
	if err != nil {
		return nil, nil, "", err
	}
	return chatModel, embedder, "真实模型", nil
}

// offlineModel 是无外部依赖的脚本化模型，仅用于链路演示。
//
// 它不会理解用户输入：只要检索到证据就复述证据，否则引导建单。
// 因此仅供验证 HTTP 与编排链路是否通畅，不可用于评估对话质量。
type offlineModel struct{}

func (m *offlineModel) Chat(_ context.Context, _, user string) (*llm.Response, error) {
	return &llm.Response{
		Content: "（离线模式）收到你的问题：" + user,
		Usage:   llm.Usage{PromptTokens: 40, CompletionTokens: 20},
	}, nil
}

func (m *offlineModel) ChatWithTools(_ context.Context, req llm.ToolRequest) (*llm.Response, error) {
	// 依据最后一条消息判断是否已拿到工具结果：拿到就收尾，没拿到就先检索。
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == llm.RoleTool {
			return &llm.Response{
				Content: "（离线模式）已根据检索结果生成回复。",
				Usage:   llm.Usage{PromptTokens: 200, CompletionTokens: 40},
			}, nil
		}
	}
	return &llm.Response{
		ToolCalls: []llm.ToolCall{{
			ID: "offline-1", Name: "rag_search",
			Arguments: `{"query":"` + lastUserContent(req) + `"}`,
		}},
		Usage: llm.Usage{PromptTokens: 200, CompletionTokens: 40},
	}, nil
}

func lastUserContent(req llm.ToolRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == llm.RoleUser {
			return sanitizeForJSON(req.Messages[i].Content)
		}
	}
	return ""
}

// jsonUnsafeReplacer 去掉会破坏 JSON 字面量的字符。
var jsonUnsafeReplacer = strings.NewReplacer(`"`, "", `\`, "", "\n", " ", "\r", " ")

// sanitizeForJSON 把用户输入变成可安全嵌入 JSON 字符串的片段。
func sanitizeForJSON(text string) string {
	out := jsonUnsafeReplacer.Replace(text)
	if runes := []rune(out); len(runes) > 40 {
		out = string(runes[:40])
	}
	return out
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
