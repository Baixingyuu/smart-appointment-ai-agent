package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mac/helpdesk-agent/internal/assign"
	"github.com/mac/helpdesk-agent/internal/eval"
	"github.com/mac/helpdesk-agent/internal/llm"
	"github.com/mac/helpdesk-agent/internal/seed"
)

// runEvalAssignV2 跑三段流水线的 v2 数据集，输出三轴独立指标 + 漏斗。
//
// 默认 --offline-stage2：不注入 LLM chooser，判弱样本直落 Stage 3。
// 这是最省心的评测姿态 —— 三段的期望都在 (Stage 1 ∪ Stage 3) 的路径上；
// Stage 2 想单独评测请配 -base-url/-api-key/-model 或改用 ScriptedChooser 走单测。
//
// 与 v1 eval-assign 的分工：v1 只测排序段（旧 Assigner + 120+30 数据集），
// v2 拆三段各打一次分。两者并行保留、互不替代；见 docs/DISPATCH_PIPELINE.md §5.1。
func runEvalAssignV2(args []string) error {
	fs := flag.NewFlagSet("eval-assign-v2", flag.ContinueOnError)
	datasetPath := fs.String("dataset", "", "v2 评测集路径（默认 eval/datasets/assignment_v2.json）")
	jsonPath := fs.String("json", "", "机读报告输出路径")
	offlineStage2 := fs.Bool("offline-stage2", true, "不注入 Stage 2 chooser；判弱样本直落 Stage 3")
	baseURL := fs.String("base-url", envOr("LLM_BASE_URL", ""), "Stage 2 模型服务地址（关闭 -offline-stage2 时必填）")
	apiKey := fs.String("api-key", envOr("LLM_API_KEY", ""), "Stage 2 模型 API Key")
	modelName := fs.String("model", envOr("LLM_MODEL", ""), "Stage 2 模型名称")
	if err := fs.Parse(args); err != nil {
		return err
	}

	path := *datasetPath
	if path == "" {
		path = locateV2Dataset()
	}
	ds, err := eval.LoadPipelineV2Dataset(path)
	if err != nil {
		return err
	}

	var chooser assign.LLMChooser
	if !*offlineStage2 {
		if *baseURL == "" || *apiKey == "" || *modelName == "" {
			return fmt.Errorf("关闭 -offline-stage2 需要同时提供 -base-url / -api-key / -model")
		}
		cm, err := llm.New(llm.Config{BaseURL: *baseURL, APIKey: *apiKey, Model: *modelName})
		if err != nil {
			return fmt.Errorf("Stage 2 模型初始化: %w", err)
		}
		chooser = assign.NewOpenAIChooser(cm, 5)
	}
	pipeline := assign.NewPipeline(
		assign.NewBM25ServiceResolver(3),
		assign.NoopSimilarIndex{},
		chooser,
	)
	rep := eval.RunPipelineV2Eval(ds, pipeline, seedDirectory())
	rep.Dataset = filepath.Base(path)
	fmt.Print(rep.Text())
	if *jsonPath != "" {
		if err := writeJSONFile(*jsonPath, rep); err != nil {
			return err
		}
		fmt.Printf("\n机读报告已写入 %s\n", *jsonPath)
	}
	// 三段任一低于 100% 都以非零退出码返回，方便挂进 CI。
	if len(rep.Failures) > 0 {
		os.Exit(1)
	}
	return nil
}

// seedDirectory 用 seed 快照构造一份当前配置下的 Directory。
//
// 员工负载全部归零 —— 评测的每次跑都必须从同一状态出发；否则同一条 case
// 两次跑因负载漂移落到不同员工，指标就不可复现。
// 生产路径的实时负载由 ticket.Service.candidates 计算，不走这里。
func seedDirectory() assign.Directory {
	emps := seed.Employees()
	for i := range emps {
		emps[i].CurrentLoad = 0
	}
	return assign.Directory{
		Employees:  emps,
		Extensions: seed.ExtensionsByEmployeeID(),
		Services:   seed.ServicesByID(),
	}
}

// locateV2Dataset 相对仓库根定位 v2 数据集，使命令在任意目录都能找到。
func locateV2Dataset() string {
	candidates := []string{
		"eval/datasets/assignment_v2.json",
		"../eval/datasets/assignment_v2.json",
		"../../eval/datasets/assignment_v2.json",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return candidates[0]
}

// writeJSONFile 与主 main.go 里的 writeJSON 同语义，独立函数避免包内符号冲突。
func writeJSONFile(path string, payload any) error {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("创建输出目录: %w", err)
		}
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化报告: %w", err)
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
