SHELL := /bin/bash

# 本项目使用的 Go 工具链安装在仓库同级目录，不污染系统路径。
# 若本机已有 go 且版本 ≥1.24，可覆盖 GO 变量使用系统版本。
GO ?= $(shell command -v go 2>/dev/null || echo $(CURDIR)/../go/bin/go)
GOPATH ?= $(CURDIR)/../gopath
GOCACHE ?= $(GOPATH)/build-cache
export GOPATH
export GOCACHE

REPORT_DIR := eval/reports

.PHONY: help
help:
	@echo "agentdesk —— 一期命令"
	@echo ""
	@echo "  make test        运行全部单元测试"
	@echo "  make eval        运行指派评测（基础集 + 对抗集）"
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

.PHONY: eval
eval:
	$(GO) run ./cmd/agentdesk eval-assign

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
	@echo "GO        $(GO)"
	@$(GO) version || echo "  ✗ 未找到 Go 工具链"
	@echo "GOPATH    $(GOPATH)"
	@echo "GOCACHE   $(GOCACHE)"
	@echo "python3   $$(python3 --version 2>&1 || echo '未安装')"
	@echo "数据集:"
	@ls -1 eval/datasets/*.json 2>/dev/null | sed 's/^/  /' || echo "  ✗ 未找到评测集"
