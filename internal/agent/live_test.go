package agent_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/mac/helpdesk-agent/internal/agent"
	"github.com/mac/helpdesk-agent/internal/assign"
	"github.com/mac/helpdesk-agent/internal/classify"
	"github.com/mac/helpdesk-agent/internal/llm"
	"github.com/mac/helpdesk-agent/internal/rag"
	"github.com/mac/helpdesk-agent/internal/seed"
	"github.com/mac/helpdesk-agent/internal/store"
	"github.com/mac/helpdesk-agent/internal/ticket"
)

// TestLiveAgentAgainstDeepSeek 用真实模型跑一遍完整链路。
//
// 需要 DEEPSEEK_API_KEY 环境变量，未设置时跳过，
// 因此 `make check` 与离线测试不依赖任何密钥。
//
// 运行方式：
//
//	DEEPSEEK_API_KEY=sk-xxx go test ./internal/agent/ -run TestLive -v
//
// 这个测试的价值在于暴露「离线桩模型发现不了」的问题——实践中已据此发现：
// 预算上限对真实模型的并行工具调用过紧；思维链模型必须回传 reasoning_content；
// 字符哈希向量化器的检索质量太差导致模型反复重试、成本翻倍。
func TestLiveAgentAgainstDeepSeek(t *testing.T) {
	key := os.Getenv("DEEPSEEK_API_KEY")
	if key == "" {
		t.Skip("未设置 DEEPSEEK_API_KEY，跳过真实模型测试")
	}

	model, err := llm.New(llm.Config{
		BaseURL: "https://api.deepseek.com/v1",
		APIKey:  key,
		Model:   "deepseek-flash",
	})
	if err != nil {
		t.Fatalf("构造模型失败: %v", err)
	}

	st := store.NewMemory()
	if err := seed.Load(st); err != nil {
		t.Fatalf("加载种子失败: %v", err)
	}
	tickets := ticket.New(st, assign.New(assign.DefaultWeights()))
	retriever := seed.NewBM25Retriever(rag.DefaultOptions())

	ag, err := agent.New(model, st, retriever, tickets, agent.DefaultConfig(),
		agent.WithClassifier(classifyAdapter{model: model}))
	if err != nil {
		t.Fatalf("构造 Agent 失败: %v", err)
	}

	cases := []struct {
		name           string
		conversationID int64
		messages       []string
	}{
		{"知识库能答", 1001, []string{"接口返回 401 鉴权失败怎么办"}},
		{"账号锁定", 1002, []string{"员工账号被锁定了怎么解锁"}},
		{"知识库未覆盖", 1003, []string{"我们的私有化环境磁盘满了需要处理", "确认"}},
		{"寒暄", 1004, []string{"你好"}},
	}

	var totalPrompt, totalCompletion int
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for i, message := range tc.messages {
				startedAt := time.Now()
				result, err := ag.Run(context.Background(), agent.TurnInput{
					ConversationID: tc.conversationID,
					UserMessage:    message,
				})
				elapsed := time.Since(startedAt)
				if err != nil {
					t.Fatalf("第 %d 轮失败: %v", i+1, err)
				}
				var tools []string
				for _, call := range result.ToolCalls {
					tools = append(tools, call.Code+"("+string(call.ErrorKind)+")")
				}
				t.Logf("轮次=%d 耗时=%v token=%d/%d 工具=%v 中断=%v 工单=%d 意图=%s 短路=%v",
					i+1, elapsed.Round(time.Millisecond),
					result.Usage.PromptTokens, result.Usage.CompletionTokens,
					tools, result.Interrupted, result.TicketID,
					result.Intent, result.ShortCircuited)
				for _, rr := range result.RoundRecords {
					t.Logf("  第%d轮 token=%d/%d 工具=%v",
						rr.Round, rr.PromptTokens, rr.CompletionTokens, rr.Tools)
				}
				totalPrompt += result.Usage.PromptTokens
				totalCompletion += result.Usage.CompletionTokens
				t.Logf("  回复: %s", truncate(result.Reply, 120))
			}
		})
	}
	t.Logf("合计 token: prompt=%d completion=%d total=%d",
		totalPrompt, totalCompletion, totalPrompt+totalCompletion)
}

func truncate(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "..."
}

// classifyAdapter 用同一个模型做意图分类，供 live 测试验证路由效果。
type classifyAdapter struct{ model llm.ChatModel }

func (c classifyAdapter) ClassifyIntent(text string) (agent.IntentOutcome, error) {
	classifier := classify.New(c.model)
	result, err := classifier.Classify(context.Background(), text)
	if err != nil {
		return agent.IntentOutcome{}, err
	}
	return agent.IntentOutcome{Intent: result.Intent, Parsed: result.Parsed, Usage: result.Usage}, nil
}
