SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

GO      ?= go
MODULE  := github.com/wuekevin/lingedge
CMDS    := lingedge lingedge-server lingedge-node lingedge-dns lingedge-scheduler lingedge-reporter lingedge-worker
ARCHS   := amd64 arm64
DIST    := dist

# 静态质量门禁（Story 1.2）：外部工具一律 go run <pkg>@<固定版本>，无需预装、不进 go.mod。
GOLANGCI_LINT_VERSION := v2.15.0
GOVULNCHECK_VERSION   := v1.9.0
GOLANGCI_LINT ?= $(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
GOVULNCHECK   ?= $(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
GATES         ?= $(GO) run ./scripts/gates

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
COMMIT  ?= $(shell git rev-parse --verify --quiet HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w -X $(MODULE)/pkg/version.Version=$(VERSION) -X $(MODULE)/pkg/version.Commit=$(COMMIT)

# 仓库无提交时 go build 的 VCS 戳会失败，版本信息统一经 ldflags 注入。
GOBUILD := GOFLAGS= CGO_ENABLED=0 $(GO) build -trimpath -buildvcs=false -ldflags '$(LDFLAGS)'

define not_implemented
	@echo "NOT_IMPLEMENTED(story $(1))" >&2; exit 1
endef

.PHONY: help build verify-build test race lint fmt vet check-scaffold clean \
        gen-lint arch-check golangci shell-check gate-selftest vuln verify-gates \
        print-ldflags migrate migrate-test proto proto-check openapi-check frontend e2e edge-test security release

help:
	@echo "目标: build verify-build test race lint migrate migrate-test proto proto-check openapi-check frontend e2e edge-test security release"
	@echo "辅助: fmt vet check-scaffold clean gen-lint arch-check golangci shell-check gate-selftest vuln verify-gates"
	@echo "未实现的目标会输出 NOT_IMPLEMENTED(story X.Y) 并以非零退出。"

# 生产构建不得带 lingedge_testdouble 标签（此处从不传入 -tags）。
build:
	@rm -rf $(DIST)
	@for os_arch in $(foreach a,$(ARCHS),linux/$(a)) windows/amd64; do \
	  os=$${os_arch%/*}; arch=$${os_arch#*/}; \
	  for c in $(CMDS); do \
	    if [ "$$os" = windows ] && [ "$$c" != lingedge-reporter ]; then continue; fi; \
	    ext=; [ "$$os" = windows ] && ext=.exe; \
	    out=$(DIST)/$$os-$$arch/$$c$$ext; \
	    echo "build $$out"; \
	    GOOS=$$os GOARCH=$$arch $(GOBUILD) -o $$out ./cmd/$$c; \
	  done; \
	done

print-ldflags:
	@echo '$(LDFLAGS)'

verify-build:
	@bash scripts/verify-build.sh $(DIST) "$(CMDS)"

# 前端单元测试随 Story 1.36 接入；此处仅 Go 部分。
test:
	$(GO) test ./...

race:
	CGO_ENABLED=1 $(GO) test -race ./...

fmt:
	gofmt -l -w .

vet:
	$(GO) vet ./...
	$(GO) vet -tags lingedge_testdouble ./...

# make lint：gofmt + go vet + 脚手架结构测试 + 依赖图/结构检查 + golangci-lint（depguard 依赖方向、forbidigo 禁 exec）+ shell 拼接检查。
lint: vet
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt 未格式化:" >&2; echo "$$out" >&2; exit 1; fi
	$(GO) test -short -count=1 ./tests/scaffold/...
	$(MAKE) --no-print-directory arch-check golangci shell-check

# 据 ADR-001 依赖图（scripts/gates/spec.go）重新生成 .golangci.yml；改依赖图后必须运行并提交。
gen-lint:
	$(GATES) gen-lint

# .golangci.yml 与依赖图一致、模块根包仅 api.go、实现仅在 internal/<mod>/internal/{handler,service,repository,domain}、
# platform/ports 仅接口与 DTO（AC1、AC6）。
arch-check:
	$(GATES) arch

golangci:
	$(GOLANGCI_LINT) run ./...

# 命令注入：禁止经 shell 解释器执行命令串、禁止字符串拼接的 exec.Command（G-12、NFR-9）；白名单入口 internal/tools/security 同样不得绕过。
shell-check:
	$(GATES) shell

# 门禁自身的正向/负向自测：在仓库副本里写入故意违规的样例，断言被拦截、合法写法不误伤，副本用后删除。
gate-selftest:
	$(GO) test -count=1 ./scripts/gates/...
	GOLANGCI_LINT='$(GOLANGCI_LINT)' $(GO) test -count=1 -v ./tests/gates/...

# 依赖漏洞扫描（G-7 的 Go 部分；需访问 vuln.go.dev，不在 make security 内，完整供应链门禁属 Story 1.34）。
vuln:
	$(GOVULNCHECK) ./...

# 一次性运行 Story 1.2 的全部门禁与自测。
verify-gates: lint security gate-selftest

check-scaffold:
	$(GO) test -count=1 -v ./tests/scaffold/...

migrate:
	$(call not_implemented,1.6)

migrate-test:
	$(call not_implemented,1.6)

proto:
	$(call not_implemented,1.3)

proto-check:
	$(call not_implemented,1.3)

openapi-check:
	$(call not_implemented,1.3)

frontend:
	$(call not_implemented,1.36)

e2e:
	$(call not_implemented,1.37)

edge-test:
	$(call not_implemented,3.22)

# 本目标当前只覆盖 Story 1.2 的静态门禁：G-6 固定口令、G-11 测试替身隔离、G-12 命令注入、NFR-29 品牌。
# 威胁模型 §9 的其余门禁（G-1…G-5、G-7…G-10、G-13）尚未接入，由对应 Story 逐项追加到本目标；
# 因此本目标通过只表示上述四项通过，不代表整套安全门禁通过。
security:
	$(GATES) testdouble
	$(GATES) passwords
	$(GATES) brand
	$(GATES) shell
	@echo "make security: 已覆盖 G-6/G-11/G-12/品牌；未接入（随对应 Story 追加）: G-1 G-2 G-3 G-4 G-5 G-7 G-8 G-9 G-10 G-13" >&2

release:
	$(call not_implemented,1.34)

clean:
	rm -rf $(DIST) bin
