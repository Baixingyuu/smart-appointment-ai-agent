// Command helpdesk-agent 是一期命令行入口。
//
// 评测与自检命令默认离线：派单走三段流水线的 Stage 1/3（纯确定性，不依赖 LLM），
// 因此这套评测可以在没有任何模型配置的情况下完整复现。
// 只有 serve / chat，以及显式关闭 -offline-stage2 的派单评测需要模型配置。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "eval-assign-v2":
		if err := runEvalAssignV2(os.Args[2:]); err != nil {
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
	case "chat":
		if err := runChat(os.Args[2:]); err != nil {
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
  helpdesk-agent chat [选项]          人工测试对话窗口（每回合轨迹落 JSONL 样本）
  helpdesk-agent demo                 跑一遍完整工单链路（建单/派单/去重/升级/完成）
  helpdesk-agent eval-assign-v2 [选项] 运行三段流水线派单评测（抽取/判弱/排序三轴）
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

chat 选项:
  -offline            使用离线脚本模型（无需 API Key）
  -base-url / -api-key / -model   同 serve
  -log string         样本 JSONL 路径（默认 eval/samples/chat-<时间>.jsonl）

  本地模型示例（8k/16k 变体会撑爆显存，用 qwen3:8b）：
    helpdesk-agent chat -api-key=ollama -base-url=http://localhost:11434/v1 \
      -model=qwen3:8b
    或直接 make chat-local

eval-assign-v2 选项:
  -dataset string   v2 评测集路径（默认 eval/datasets/assignment_v2.json）
  -json string      把机读报告写入该路径
  -offline-stage2   不注入 Stage 2 chooser（默认），判弱样本直落 Stage 3；
                    关掉此标志需要同时提供 LLM 配置：
  -base-url / -api-key / -model   Stage 2 用的 LLM 服务
`)
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
