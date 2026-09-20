// Command helpdesk-agent 是一期命令行入口。
//
// 一期只提供评测与自检命令：派单器不依赖 LLM，因此这套评测
// 可以在没有任何模型配置的情况下完整复现。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mac/helpdesk-agent/internal/assign"
	"github.com/mac/helpdesk-agent/internal/eval"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "eval-assign":
		if err := runEvalAssign(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "错误: %v\n", err)
			os.Exit(1)
		}
	case "eval-retrieval":
		if err := runEvalRetrieval(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "错误: %v\n", err)
			os.Exit(1)
		}
	case "eval-intent":
		if err := runEvalIntent(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "错误: %v\n", err)
			os.Exit(1)
		}
	case "eval-trajectory":
		if err := runEvalTrajectory(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "错误: %v\n", err)
			os.Exit(1)
		}
	case "eval-realtickets":
		if err := runEvalRealTickets(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "错误: %v\n", err)
			os.Exit(1)
		}
	case "serve":
		if err := runServe(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "错误: %v\n", err)
			os.Exit(1)
		}
	case "demo":
		if err := runDemo(); err != nil {
			fmt.Fprintf(os.Stderr, "错误: %v\n", err)
			os.Exit(1)
		}
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `helpdesk-agent —— 一期命令

用法:
  helpdesk-agent serve [选项]         启动 HTTP 服务
  helpdesk-agent demo                 跑一遍完整工单链路（建单/派单/去重/升级/完成）
  helpdesk-agent eval-assign [选项]   运行指派评测
  helpdesk-agent eval-retrieval [选项]  运行检索召回评测
  helpdesk-agent eval-intent [选项]   运行意图分类评测
  helpdesk-agent eval-trajectory [选项]  运行轨迹评测（工具选择/轮次/成本/延迟）
  helpdesk-agent eval-realtickets [选项] 真实工单观测（无金标，只记录行为分布）

eval-retrieval 选项:
  -dataset string   检索评测集路径（默认 eval/datasets/retrieval.json）
  -json string      把机读报告写入该路径
  -k int            召回截断位置（默认 5）
  -threshold float  覆盖检索器的分数阈值，用于做敏感性分析

eval-intent 选项:
  -dataset string   意图评测集路径（默认 eval/datasets/intent.json）
  -json string      把机读报告写入该路径
  -offline          用离线关键词基线（零成本对照，无需 API Key）

eval-trajectory 选项:
  -dataset string   轨迹评测集路径（默认 eval/datasets/trajectory.json）
  -json string      把机读报告写入该路径
  -sabotage string  人为注入缺陷以验证评测区分力（仅脚本模式）:
                    skip_rag | always_write | extra_rounds | unknown_tool
  -api-key string   提供后用真实模型驱动评测（或用 LLM_API_KEY），
                    与 -sabotage 互斥；测量真实工具选择能力
  -base-url string  模型服务地址（或用 LLM_BASE_URL，默认 DeepSeek 官方）
  -model string     模型名称（或用 LLM_MODEL，默认 deepseek-flash）

serve 选项:
  -addr string        监听地址（默认 :8080）
  -offline            使用离线脚本模型（无需 API Key，仅验证链路）
  -base-url string    模型服务地址（或用 LLM_BASE_URL）
  -api-key string     模型 API Key（或用 LLM_API_KEY）
  -model string       模型名称（或用 LLM_MODEL，默认 gpt-4o-mini）
  -embed-base-url / -embed-api-key / -embed-model   向量模型（可选）

eval-assign 选项:
  -dataset string   评测集路径，可重复指定（默认同时运行基础集与对抗集）
  -json string      把机读报告写入该路径
  -quiet            只输出汇总
`)
}

func runEvalAssign(args []string) error {
	fs := flag.NewFlagSet("eval-assign", flag.ContinueOnError)
	var datasets multiFlag
	fs.Var(&datasets, "dataset", "评测集路径，可重复指定（默认同时运行基础集与对抗集）")
	jsonPath := fs.String("json", "", "机读报告输出路径")
	quiet := fs.Bool("quiet", false, "只输出汇总")
	if err := fs.Parse(args); err != nil {
		return err
	}

	paths := datasets
	if len(paths) == 0 {
		paths = defaultDatasetPaths()
	}

	assigner := assign.New(assign.DefaultWeights())
	suite := eval.SuiteReport{}

	for _, path := range paths {
		dataset, err := eval.LoadAssignmentDataset(path)
		if err != nil {
			return err
		}
		report := eval.RunAssignmentEval(dataset, assigner)
		suite.Datasets = append(suite.Datasets, eval.DatasetReport{
			Name:   filepath.Base(path),
			Path:   path,
			Report: report,
		})
	}

	suite.Summarize()
	if *quiet {
		for i := range suite.Datasets {
			suite.Datasets[i].Report.Failures = nil
		}
	}
	fmt.Print(suite.Text())

	if *jsonPath != "" {
		if err := writeJSON(*jsonPath, suite); err != nil {
			return err
		}
		fmt.Printf("\n机读报告已写入 %s\n", *jsonPath)
	}
	return nil
}

// multiFlag 支持重复出现的字符串参数。
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(value string) error {
	*m = append(*m, value)
	return nil
}

func writeJSON(path string, payload any) error {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("创建输出目录: %w", err)
		}
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化报告: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("写入报告: %w", err)
	}
	return nil
}

// defaultDatasetPaths 相对仓库根定位评测集，
// 使命令在任意工作目录下执行都能找到数据。
// 两份数据集都会返回（存在才列出）：
//   - assignment.json       基础集，覆盖各匹配场景
//   - assignment_hard.json  对抗集，用于证明评测具备区分能力
func defaultDatasetPaths() []string {
	names := []string{"assignment.json", "assignment_hard.json"}
	dirs := []string{
		"eval/datasets",
		"../eval/datasets",
		"../../eval/datasets",
	}
	var ret []string
	for _, name := range names {
		for _, dir := range dirs {
			path := filepath.Join(dir, name)
			if _, err := os.Stat(path); err == nil {
				ret = append(ret, path)
				break
			}
		}
	}
	if len(ret) == 0 {
		ret = append(ret, filepath.Join("eval", "datasets", "assignment.json"))
	}
	return ret
}
