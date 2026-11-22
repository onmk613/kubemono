# kubemono 构建入口：本机调试、linux 发布二进制、离线物料与质量门禁。
# 目标平台只有 linux/amd64、linux/arm64；macOS 开发机可跑 fmt/vet/test。

SHELL   := /bin/bash
BIN_DIR := bin
BINARY  := kubemono
GO_SRC  := cmd internal

# 归档与造包参数：ARCHS=amd64、arm64 或 amd64,arm64；IMAGES=1 生成含镜像的完整包。
OUT_DIR        ?= dist
ARCHS          ?= amd64
IMAGES         ?= 0
CLUSTER_CONFIG ?=
PACKAGE_ARGS   ?=

# 质量门禁工具：固定版本，CI 用 `make tools` 安装到本仓库 bin/。
DEADCODE_VERSION   ?= v0.51.0
GOLANGCI_VERSION   ?= v2.15.0
TOOL_BIN           := $(CURDIR)/$(BIN_DIR)

COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X kubemono/internal/version.Commit=$(COMMIT) -X kubemono/internal/version.BuildDate=$(DATE)
RELEASE_LDFLAGS := $(LDFLAGS) -s -w

.DEFAULT_GOAL := build
.PHONY: build build-linux dist fmt vet test test-race lint check ci e2e-local clean help tools

build: ## 本机调试二进制（当前 OS，不能直接用于 Linux 节点）
	@mkdir -p $(BIN_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) ./cmd/kubemono

build-linux: ## 发布二进制：bin/kubemono-linux-amd64、bin/kubemono-linux-arm64
	@mkdir -p $(BIN_DIR)
	@for arch in amd64 arm64; do \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath \
			-ldflags "$(RELEASE_LDFLAGS)" -o $(BIN_DIR)/$(BINARY)-linux-$$arch ./cmd/kubemono || exit 1; \
	done
	@if [ "$$(uname -s)/$$(uname -m)" = "Linux/x86_64" ]; then \
		echo "smoke: $$($(BIN_DIR)/$(BINARY)-linux-amd64 version --short)"; \
	fi

PACK_FLAGS := --out-dir "$(OUT_DIR)" \
	$(if $(filter 1,$(IMAGES)),--images --fetch-images) \
	$(if $(CLUSTER_CONFIG),-f "$(CLUSTER_CONFIG)") $(PACKAGE_ARGS)

dist: build-linux ## 物料归档（默认轻量 amd64；ARCHS=... IMAGES=1 可调）
	@mkdir -p "$(OUT_DIR)"
	@for arch in $$(echo "$(ARCHS)" | tr ',' ' '); do \
		echo "==> pack linux/$$arch"; \
		go run ./cmd/kubemono bundle pack --arch "$$arch" $(PACK_FLAGS) || exit 1; \
	done

fmt: ## gofmt 检查（只报告，不修改）
	@out="$$(gofmt -l $(GO_SRC))" || exit 1; \
	if [ -n "$$out" ]; then echo "需要 gofmt："; echo "$$out"; exit 1; fi

vet: ## go vet（GOOS=linux）
	GOOS=linux go vet ./...

test: ## 单元测试
	go test ./... -count=1

test-race: ## 单元测试 + 竞态检测
	go test -race ./... -count=1

tools: ## 安装固定版本的质量工具到 bin/（CI 前置）
	@mkdir -p $(TOOL_BIN)
	GOBIN=$(TOOL_BIN) go install golang.org/x/tools/cmd/deadcode@$(DEADCODE_VERSION)
	GOBIN=$(TOOL_BIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)

lint: ## golangci-lint（GOOS=linux；固定版本见 Makefile，需先 make tools）
	@# 版本固定在 Makefile：golangci-lint 自带的 x/tools 必须能解码当前 Go 的导出数据，
	@# 否则会报 "export data version N > M"——这时升级 GOLANGCI_VERSION，而不是只重建。
	@lint_bin="$$(command -v golangci-lint || true)"; \
	if [ -x "$(TOOL_BIN)/golangci-lint" ]; then lint_bin="$(TOOL_BIN)/golangci-lint"; fi; \
	if [ -z "$$lint_bin" ]; then \
		echo "缺少 golangci-lint：先在 CI 执行 make tools（或 go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)）"; exit 1; \
	fi; \
	lint_go=$$("$$lint_bin" version | grep -oE 'built with go[0-9.]+' | grep -oE 'go[0-9.]+' | tail -1 | sed 's/^go//'); \
	cur_go=$$(go env GOVERSION | sed 's/^go//'); \
	if [ -z "$$lint_go" ]; then echo "无法解析 golangci-lint 的构建 Go 版本"; exit 1; fi; \
	if [ "$$(printf '%s\n%s\n' "$$lint_go" "$$cur_go" | sort -V | head -1)" != "$$cur_go" ]; then \
		echo "golangci-lint 由旧 Go 构建（$${lint_go} < $${cur_go}）：请重新构建 make tools；若仍报 export data，升级 GOLANGCI_VERSION。"; \
		exit 1; \
	fi; \
	GOOS=linux "$$lint_bin" run --timeout 10m

e2e-local: ## 本地 CLI 链路：bundle load/verify/list + pack 集成（不需要真实集群）
	go test ./internal/cli/bundle/ ./internal/pack/ -count=1

check: fmt vet test ## 提交前检查（不需要额外工具）

# CI 门禁：GitHub Actions（.github/workflows/ci.yml）执行的就是 `make tools` + `make ci`。
ci: fmt vet test-race build-linux ## 提交门禁：格式/vet/竞态测试/发布构建/死代码/边界/文档链接/lint
	@# 先执行 make tools；这里把缺工具从“跳过”改成硬失败，避免门禁空转。
	KUBEMONO_REQUIRE_TOOLS=1 ./hack/deadcode-report.sh
	./hack/check-boundaries.sh
	./hack/check-docs.sh
	@$(MAKE) --no-print-directory lint

clean: ## 清理 bin/ 与 dist/
	rm -rf $(BIN_DIR) $(OUT_DIR)

help: ## 列出目标
	@grep -E '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
