package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mac/helpdesk-agent/internal/agent"
	"github.com/mac/helpdesk-agent/internal/classify"
	"github.com/mac/helpdesk-agent/internal/evalrun"
	"github.com/mac/helpdesk-agent/internal/llm"
	"github.com/mac/helpdesk-agent/internal/rag"
	"github.com/mac/helpdesk-agent/internal/seed"
	"github.com/mac/helpdesk-agent/internal/store"
)

// 真实工单观测：把真实 IT 工单文本喂给 Agent，记录它做了什么。
//
// 为什么单独做成命令而不是复用 eval-trajectory：
// 真实工单**没有金标**，不能算通过率。硬塞进轨迹评测会让报告出现一个
// 没有意义的 100%（空断言必然通过），反而误导。这里的产出是**行为分布**：
// 意图判成什么、有没有走短路、要不要建单、犯没犯错、花了多少。
//
// 它回答的问题与评测不同：评测问「做得对不对」，观测问「在分布外输入上
// 系统会怎么表现」。前者需要金标，后者不需要——后者恰恰是发现金标盲区的手段。

// RealTicketCase 一条真实工单样本。
type RealTicketCase struct {
	ID         string `json:"id"`
	TopicGroup string `json:"topicGroup"`
	Text       string `json:"text"`
}

// RealTicketDataset 真实工单观测集。
type RealTicketDataset struct {
	Version     int              `json:"version"`
	Description string           `json:"description"`
	Source      string           `json:"source"`
	Cases       []RealTicketCase `json:"cases"`
}

// RealTicketObservation 单条工单的观测结果。
type RealTicketObservation struct {
	ID            string   `json:"id"`
	TopicGroup    string   `json:"topicGroup"`
	Intent        string   `json:"intent,omitempty"`
	ShortCircuit  bool     `json:"shortCircuited"`
	Interrupted   bool     `json:"interrupted"`
	TicketCreated bool     `json:"ticketCreated"`
	Tools         []string `json:"tools"`
	Rounds        int      `json:"rounds"`
	PromptTokens  int      `json:"promptTokens"`
	TotalTokens   int      `json:"totalTokens"`
	DurationMS    int      `json:"durationMs"`
	Error         string   `json:"error,omitempty"`
}

// RealTicketReport 真实工单观测报告。
type RealTicketReport struct {
	Mode  string `json:"mode"`
	Model string `json:"model"`

	Total  int `json:"total"`
	Errors int `json:"errors"`

	// 行为分布。分母均为「成功执行的样本数」，错误样本不计入——
	// 把服务故障混进行为统计会把故障读成模型行为。
	ShortCircuitRate  float64 `json:"shortCircuitRate"`
	TicketCreatedRate float64 `json:"ticketCreatedRate"`
	InterruptRate     float64 `json:"interruptRate"`
	AvgRounds         float64 `json:"avgRounds"`
	AvgTools          float64 `json:"avgTools"`

	LatencyP50MS int `json:"latencyP50Ms"`
	LatencyP95MS int `json:"latencyP95Ms"`
	LatencyMaxMS int `json:"latencyMaxMs"`

	PromptTokens int `json:"promptTokens"`
	TotalTokens  int `json:"totalTokens"`

	IntentDistribution map[string]int       `json:"intentDistribution"`
	ToolDistribution   map[string]int       `json:"toolDistribution"`
	ByTopic            map[string]TopicStat `json:"byTopic"`

	Observations []RealTicketObservation `json:"observations"`
}

// TopicStat 按真实工单类别分解的观测统计。
type TopicStat struct {
	Total         int     `json:"total"`
	Errors        int     `json:"errors"`
	ShortCircuit  int     `json:"shortCircuit"`
	TicketCreated int     `json:"ticketCreated"`
	AvgRounds     float64 `json:"avgRounds"`
}

func runEvalRealTickets(args []string) error {
	fs := flag.NewFlagSet("eval-realtickets", flag.ContinueOnError)
	datasetPath := fs.String("dataset", defaultRealTicketDatasetPath(), "真实工单观测集路径")
	jsonPath := fs.String("json", "", "机读报告输出路径")
	limit := fs.Int("limit", 0, "只观测前 N 条（0 表示全部）")
	baseURL := fs.String("base-url", envOr("LLM_BASE_URL", "https://api.deepseek.com/v1"), "模型服务地址")
	apiKey := fs.String("api-key", envOr("LLM_API_KEY", ""), "模型 API Key（必需：真实数据必须由真实模型驱动）")
	modelName := fs.String("model", envOr("LLM_MODEL", "deepseek-flash"), "模型名称")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *apiKey == "" {
		return fmt.Errorf("需要 -api-key 或 LLM_API_KEY：真实工单观测必须由真实模型驱动" +
			"（本地模型用 make eval-realtickets-local）")
	}

	dataset, err := loadRealTicketDataset(*datasetPath)
	if err != nil {
		return err
	}
	cases := dataset.Cases
	if *limit > 0 && *limit < len(cases) {
		cases = cases[:*limit]
	}

	model, err := llm.New(llm.Config{BaseURL: *baseURL, APIKey: *apiKey, Model: *modelName})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "模式：真实模型 %s 驱动真实工单观测（%d 条，逐条独立执行）\n\n",
		*modelName, len(cases))

	report := RealTicketReport{
		Mode:               "observation",
		Model:              *modelName,
		Total:              len(cases),
		IntentDistribution: map[string]int{},
		ToolDistribution:   map[string]int{},
		ByTopic:            map[string]TopicStat{},
	}

	durations := make([]int, 0, len(cases))
	var roundsSum, toolsSum, okCount int
	var shortCircuit, created, interrupted int
	// 按类别累计轮次与可执行数，最后统一求均值——避免在循环里
	// 用增量公式重算均值（那样写容易错，且不体现「错误样本不计入」的语义）。
	topicRounds := map[string]int{}
	topicOK := map[string]int{}

	for _, item := range cases {
		observation := observeOneTicket(item, model)
		report.Observations = append(report.Observations, observation)

		stat := report.ByTopic[item.TopicGroup]
		stat.Total++
		if observation.Error != "" {
			report.Errors++
			stat.Errors++
			report.ByTopic[item.TopicGroup] = stat
			fmt.Fprintf(os.Stderr, "  %s [%s] 失败：%s\n", item.ID, item.TopicGroup, observation.Error)
			continue
		}
		okCount++
		roundsSum += observation.Rounds
		toolsSum += len(observation.Tools)
		topicRounds[item.TopicGroup] += observation.Rounds
		topicOK[item.TopicGroup]++
		report.PromptTokens += observation.PromptTokens
		report.TotalTokens += observation.TotalTokens
		durations = append(durations, observation.DurationMS)
		for _, code := range observation.Tools {
			report.ToolDistribution[code]++
		}
		if observation.Intent != "" {
			report.IntentDistribution[observation.Intent]++
		}
		if observation.ShortCircuit {
			shortCircuit++
			stat.ShortCircuit++
		}
		if observation.TicketCreated {
			created++
			stat.TicketCreated++
		}
		if observation.Interrupted {
			interrupted++
		}
		report.ByTopic[item.TopicGroup] = stat

		fmt.Fprintf(os.Stderr, "  %s [%s] 完成：意图=%s 轮次=%d 工具=%d 建单=%v\n",
			item.ID, item.TopicGroup, observation.Intent, observation.Rounds,
			len(observation.Tools), observation.TicketCreated)
	}

	for topic, count := range topicOK {
		stat := report.ByTopic[topic]
		stat.AvgRounds = float64(topicRounds[topic]) / float64(count)
		report.ByTopic[topic] = stat
	}

	if okCount > 0 {
		report.ShortCircuitRate = float64(shortCircuit) / float64(okCount)
		report.TicketCreatedRate = float64(created) / float64(okCount)
		report.InterruptRate = float64(interrupted) / float64(okCount)
		report.AvgRounds = float64(roundsSum) / float64(okCount)
		report.AvgTools = float64(toolsSum) / float64(okCount)
		report.LatencyP50MS, report.LatencyP95MS, report.LatencyMaxMS = latencyMS(durations)
	}

	fmt.Print(report.Text())
	if *jsonPath != "" {
		if err := writeJSON(*jsonPath, report); err != nil {
			return err
		}
		fmt.Printf("\n机读报告已写入 %s\n", *jsonPath)
	}
	return nil
}

// observeOneTicket 把一条真实工单文本作为用户消息跑一遍 Agent。
//
// 每条样本使用全新存储：真实工单之间互不相关，若共享存储，
// 前一条建出的工单会影响后一条的去重判定。
func observeOneTicket(item RealTicketCase, model llm.ChatModel) RealTicketObservation {
	observation := RealTicketObservation{
		ID:         item.ID,
		TopicGroup: item.TopicGroup,
		Tools:      []string{},
	}

	st := store.NewMemory()
	if err := seed.Load(st); err != nil {
		observation.Error = "加载种子数据失败: " + err.Error()
		return observation
	}
	tickets := newDispatchService(st, nil)
	retriever := seed.NewBM25Retriever(rag.DefaultOptions())

	// 与 serve 同构：注入意图分类器，观测的是「部署形态的系统」，
	// 而不是裸模型。短路与否本身就是要观测的行为之一。
	ag, err := agent.New(model, st, retriever, tickets, agent.DefaultConfig(),
		agent.WithClassifier(evalrun.NewClassifierRunner(classify.New(model))))
	if err != nil {
		observation.Error = err.Error()
		return observation
	}

	startedAt := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), liveTurnTimeout)
	defer cancel()
	result, err := ag.Run(ctx, agent.TurnInput{ConversationID: 1, UserMessage: item.Text})
	if err != nil {
		observation.Error = err.Error()
		return observation
	}

	observation.Intent = string(result.Intent)
	observation.ShortCircuit = result.ShortCircuited
	observation.Interrupted = result.Interrupted
	observation.TicketCreated = result.TicketID > 0
	observation.Rounds = result.Rounds
	observation.PromptTokens = result.Usage.PromptTokens
	observation.TotalTokens = result.Usage.Total()
	observation.DurationMS = int(time.Since(startedAt).Milliseconds())
	for _, call := range result.ToolCalls {
		observation.Tools = append(observation.Tools, call.Code)
	}
	return observation
}

func loadRealTicketDataset(path string) (*RealTicketDataset, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取观测集 %s: %w（先用 eval/gen_realtickets.py 生成）", path, err)
	}
	var dataset RealTicketDataset
	if err := json.Unmarshal(data, &dataset); err != nil {
		return nil, fmt.Errorf("解析观测集 %s: %w", path, err)
	}
	if len(dataset.Cases) == 0 {
		return nil, fmt.Errorf("观测集 %s 不含任何样本", path)
	}
	return &dataset, nil
}

func latencyMS(durations []int) (int, int, int) {
	if len(durations) == 0 {
		return 0, 0, 0
	}
	sorted := append([]int(nil), durations...)
	sort.Ints(sorted)
	pick := func(p int) int {
		index := (len(sorted)*p+99)/100 - 1
		if index < 0 {
			index = 0
		}
		if index >= len(sorted) {
			index = len(sorted) - 1
		}
		return sorted[index]
	}
	return pick(50), pick(95), sorted[len(sorted)-1]
}

// Text 渲染人读报告。
func (r RealTicketReport) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "真实工单观测报告（不是评测：真实数据无金标，不计算通过率）\n")
	fmt.Fprintf(&b, "%s\n", strings.Repeat("=", 66))
	fmt.Fprintf(&b, "模型              %s\n", r.Model)
	fmt.Fprintf(&b, "样本总数          %d（执行失败 %d 条，不计入行为统计）\n", r.Total, r.Errors)
	fmt.Fprintf(&b, "可执行样本        %d\n", r.Total-r.Errors)
	fmt.Fprintf(&b, "短路率            %.1f%%（意图判为寒暄/无关/转人工）\n", r.ShortCircuitRate*100)
	fmt.Fprintf(&b, "发起建单确认率    %.1f%%（单条消息通常停在等待用户确认）\n", r.InterruptRate*100)
	fmt.Fprintf(&b, "实际建单率        %.1f%%（需用户确认后才建单，本观测预期接近 0）\n", r.TicketCreatedRate*100)
	fmt.Fprintf(&b, "平均轮次          %.2f\n", r.AvgRounds)
	fmt.Fprintf(&b, "平均工具调用      %.2f\n", r.AvgTools)
	fmt.Fprintf(&b, "延迟 P50/P95/Max  %d / %d / %d ms\n", r.LatencyP50MS, r.LatencyP95MS, r.LatencyMaxMS)
	fmt.Fprintf(&b, "prompt token 合计 %d\n", r.PromptTokens)
	fmt.Fprintf(&b, "token 合计        %d\n", r.TotalTokens)

	if len(r.IntentDistribution) > 0 {
		fmt.Fprintf(&b, "\n意图分布\n%s\n", strings.Repeat("-", 66))
		for _, name := range sortedKeys(r.IntentDistribution) {
			fmt.Fprintf(&b, "%-28s %d\n", name, r.IntentDistribution[name])
		}
	}
	if len(r.ToolDistribution) > 0 {
		fmt.Fprintf(&b, "\n工具调用分布\n%s\n", strings.Repeat("-", 66))
		for _, name := range sortedKeys(r.ToolDistribution) {
			fmt.Fprintf(&b, "%-28s %d\n", name, r.ToolDistribution[name])
		}
	}
	if len(r.ByTopic) > 0 {
		fmt.Fprintf(&b, "\n按真实工单类别\n%s\n", strings.Repeat("-", 66))
		fmt.Fprintf(&b, "%-24s %5s %6s %6s %8s\n", "类别", "条数", "短路", "建单", "均轮次")
		for _, name := range sortedKeys(r.ByTopic) {
			stat := r.ByTopic[name]
			fmt.Fprintf(&b, "%-24s %5d %6d %6d %8.2f\n",
				name, stat.Total, stat.ShortCircuit, stat.TicketCreated, stat.AvgRounds)
		}
	}

	if r.Errors > 0 {
		fmt.Fprintf(&b, "\n执行失败样本\n%s\n", strings.Repeat("-", 66))
		for _, item := range r.Observations {
			if item.Error != "" {
				fmt.Fprintf(&b, "%s [%s] %s\n", item.ID, item.TopicGroup, item.Error)
			}
		}
	}
	return b.String()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func defaultRealTicketDatasetPath() string {
	candidates := []string{
		"eval/datasets/realtickets.json",
		"../eval/datasets/realtickets.json",
		"../../eval/datasets/realtickets.json",
	}
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return filepath.Join("eval", "datasets", "realtickets.json")
}
