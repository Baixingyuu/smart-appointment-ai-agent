SHELL := /bin/bash

# 依赖全局 Go 工具链（brew install go）。
# 若本机 Go 在其他位置，用 make GO=/path/to/go 覆盖。
GO ?= go

# 评测报告输出目录
REPORT_DIR := eval/reports

.PHONY: help
help:
	@echo "agentdesk —— 一期命令"
	@echo ""
	@echo "  make test        运行全部单元测试"
	@echo "  make serve       启动 HTTP 服务（需 LLM_API_KEY）"
	@echo "  make serve-offline  启动 HTTP 服务（离线脚本模型，无需密钥）"
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
	$(GO) run ./cmd/agentdesk serve $(ARGS)

.PHONY: serve-offline
serve-offline:
	$(GO) run ./cmd/agentdesk serve -offline $(ARGS)

.PHONY: eval
eval:
	$(GO) run ./cmd/agentdesk eval-assign

# 意图路由评测：准确率/Macro-F1/解析率 + 关键词基线对照
#   make eval-intent ARGS="-offline"   零成本基线
.PHONY: eval-intent
eval-intent:
	$(GO) run ./cmd/agentdesk eval-intent $(ARGS)

# 检索召回评测：Recall@K / MRR / 假命中率
.PHONY: eval-retrieval
eval-retrieval:
	$(GO) run ./cmd/agentdesk eval-retrieval $(ARGS)

# 轨迹评测：工具选择/顺序/轮次/越界/成本/延迟
# 可传 ARGS 注入缺陷以验证评测区分力，例如：
#   make eval-trajectory ARGS="-sabotage=always_write"
.PHONY: eval-trajectory
eval-trajectory:
	$(GO) run ./cmd/agentdesk eval-trajectory $(ARGS)

.PHONY: eval-report
eval-report:
	@mkdir -p $(REPORT_DIR)
	$(GO) run ./cmd/agentdesk eval-assign -quiet -json $(REPORT_DIR)/assign.json

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
