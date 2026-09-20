SHELL := /bin/bash

# 依赖全局 Go 工具链（brew install go）。
# 若本机 Go 在其他位置，用 make GO=/path/to/go 覆盖。
GO ?= go

# 评测报告输出目录
REPORT_DIR := eval/reports

.PHONY: help
help:
	@echo "helpdesk-agent —— 一期命令"
	@echo ""
	@echo "  make test        运行全部单元测试"
	@echo "  make serve       启动 HTTP 服务（需 LLM_API_KEY）"
	@echo "  make serve-offline  启动 HTTP 服务（离线脚本模型，无需密钥）"
	@echo "  make chat          人工测试对话窗口（需 LLM_API_KEY，样本落 eval/samples）"
	@echo "  make chat-local    人工测试对话窗口（本地 qwen3:8b，零成本）"
	@echo "  make eval        运行指派评测（基础集 + 对抗集）"
	@echo "  make eval-intent 运行意图路由评测"
	@echo "  make eval-retrieval 运行检索召回评测"
	@echo "  make eval-trajectory 运行轨迹评测（可传 ARGS=\"-sabotage=...\"）"
	@echo "  make eval-report 运行评测并写出 JSON 报告"
	@echo "  make verify-data 独立校验评测集金标（精确有理数运算）"
	@echo "  make fmt         格式化 Go 代码"
	@echo "  make vet         静态检查"
	@echo "  make check       fmt + vet + test + verify-data"
	@echo "  make doctor      环境自检"

.PHONY: fmt
fmt:
	$(GO) fmt ./...

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: test
test:
	$(GO) test ./...

# 启动 HTTP 服务。默认需提供模型密钥，例如：
#   LLM_API_KEY=sk-xxx make serve
# 仅验证链路时用离线脚本模型：
#   make serve-offline
.PHONY: serve
serve:
	$(GO) run ./cmd/helpdesk-agent serve $(ARGS)

.PHONY: serve-offline
serve-offline:
	$(GO) run ./cmd/helpdesk-agent serve -offline $(ARGS)

.PHONY: eval
eval:
	$(GO) run ./cmd/helpdesk-agent eval-assign

# 意图路由评测：准确率/Macro-F1/解析率 + 关键词基线对照
#   make eval-intent ARGS="-offline"   零成本基线
.PHONY: eval-intent
eval-intent:
	$(GO) run ./cmd/helpdesk-agent eval-intent $(ARGS)

# 检索召回评测：Recall@K / MRR / 假命中率
.PHONY: eval-retrieval
eval-retrieval:
	$(GO) run ./cmd/helpdesk-agent eval-retrieval $(ARGS)

# 轨迹评测：工具选择/顺序/轮次/越界/成本/延迟
# 默认离线脚本模式（链路自检）；提供密钥则用真实模型测量能力：
#   make eval-trajectory ARGS="-api-key=sk-xxx"
# 可传 ARGS 注入缺陷以验证评测区分力（仅脚本模式）：
#   make eval-trajectory ARGS="-sabotage=always_write"
.PHONY: eval-trajectory
eval-trajectory:
	$(GO) run ./cmd/helpdesk-agent eval-trajectory $(ARGS)

# 本地 Qwen3-8B（ollama）驱动的 live 轨迹评测——默认测试模型。
# 一次性准备：ollama pull qwen3:8b（约 5.2GB）
# 用默认 4k 上下文，不要盲目调大：实测把上下文提到 8k/16k 会让 KV cache 撑爆
# GPU 显存（日志：kIOGPUCommandBufferCallbackErrorOutOfMemory，Metal backend
# 进入粘滞错误态后整轮评测全失败），而本项目单请求峰值约 4.6k token，
# 4k 默认值在多轮用例上实测可完整跑完。显存紧张时重启 ollama 服务即可恢复。
# temp=0 下结果逐项一致（方差=0），可直接作为回归基线。
.PHONY: eval-trajectory-local
eval-trajectory-local:
	$(GO) run ./cmd/helpdesk-agent eval-trajectory \
		-base-url http://localhost:11434/v1 -api-key ollama -model qwen3:8b $(ARGS)

.PHONY: chat
chat:
	$(GO) run ./cmd/helpdesk-agent chat $(ARGS)

# 人工测试对话窗口（本地 qwen3:8b）。每回合轨迹落 eval/samples/*.jsonl，
# 含意图/轮次/逐轮 token/工具序列/被拒原因/是否建单，供后续评测与缺陷归因复核。
# 与 eval-trajectory-local 同一模型口径：4k 默认上下文，勿改用 8k/16k 变体（会撑爆显存）。
.PHONY: chat-local
chat-local:
	$(GO) run ./cmd/helpdesk-agent chat \
		-base-url http://localhost:11434/v1 -api-key ollama -model qwen3:8b $(ARGS)

# 真实工单观测（非评测：真实数据无金标，不算通过率）。
# 数据集由真实 IT 工单语料抽样生成：
#   python3 eval/gen_realtickets.py /path/to/all_tickets_processed_improved_v3.csv
# 数据来源见 eval/gen_realtickets.py 顶部说明；CSV 不入库。
.PHONY: eval-realtickets-local
eval-realtickets-local:
	$(GO) run ./cmd/helpdesk-agent eval-realtickets \
		-base-url http://localhost:11434/v1 -api-key ollama -model qwen3:8b $(ARGS)

.PHONY: eval-report
eval-report:
	@mkdir -p $(REPORT_DIR)
	$(GO) run ./cmd/helpdesk-agent eval-assign -quiet -json $(REPORT_DIR)/assign.json

# 独立校验：与生成脚本刻意分离，用精确有理数重算金标，
# 避免「生成即验证」的循环论证。
.PHONY: verify-data
verify-data:
	python3 eval/verify_datasets.py

.PHONY: check
check: fmt vet test verify-data
	@echo ""
	@echo "✓ 全部检查通过"

.PHONY: doctor
doctor:
	@echo "go        $$($(GO) version 2>/dev/null || echo '✗ 未安装，请执行 brew install go')"
	@echo "GOROOT    $$($(GO) env GOROOT 2>/dev/null)"
	@echo "python3   $$(python3 --version 2>&1 || echo '未安装')"
	@echo "数据集:"
	@ls -1 eval/datasets/*.json 2>/dev/null | sed 's/^/  /' || echo "  ✗ 未找到评测集"
