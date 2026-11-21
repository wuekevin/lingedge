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

# 契约工具链（Story 1.3）：buf、oasdiff 同样 go run <pkg>@<固定版本>；契约检查程序 scripts/contract 是独立 module
# （kin-openapi 不进根 go.mod），先编译到 bin/contract 再从仓库根运行。
BUF_VERSION     := v1.75.0
OASDIFF_VERSION := v1.38.0
BUF             ?= $(GO) run github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)
OASDIFF         ?= $(GO) run github.com/oasdiff/oasdiff@$(OASDIFF_VERSION)
CONTRACT_BIN    := bin/contract
CONTRACT        ?= $(CONTRACT_BIN)
# 冻结基线：与哪个 git ref 比较破坏性变更（CF-1 之后恒为 contract-v1；CF-2 起改为新标签）。
CONTRACT_BASE   ?= contract-v1
# 破坏性变更放行：make openapi-check CONTRACT_NOTE=docs/bmad/contract-changes/<变更说明>.md（须含 ADR，见 _template.md）。
CONTRACT_NOTE   ?=

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
COMMIT  ?= $(shell git rev-parse --verify --quiet HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w -X $(MODULE)/pkg/version.Version=$(VERSION) -X $(MODULE)/pkg/version.Commit=$(COMMIT)

# 仓库无提交时 go build 的 VCS 戳会失败，版本信息统一经 ldflags 注入。
GOBUILD := GOFLAGS= CGO_ENABLED=0 $(GO) build -trimpath -buildvcs=false -ldflags '$(LDFLAGS)'

define not_implemented
	@echo "NOT_IMPLEMENTED(story $(1))" >&2; exit 1
endef

.PHONY: help build verify-build test race lint fmt vet check-scaffold clean \
        gen-lint arch-check golangci shell-check repo-check gate-selftest vuln verify-gates \
        contract-bin contract-selftest openapi-breaking migrations-check migrations-seal freeze-check perms-check tenantscope-check audit-check contract-check \
        print-ldflags migrate migrate-test proto proto-check openapi-check frontend e2e edge-test security release

help:
	@echo "目标: build verify-build test race lint migrate migrate-test proto proto-check openapi-check frontend e2e edge-test security release"
	@echo "辅助: fmt vet check-scaffold clean gen-lint arch-check golangci shell-check gate-selftest vuln verify-gates"
	@echo "契约(Story 1.3): contract-check（= contract-selftest openapi-check proto-check migrations-check freeze-check perms-check[Story 1.18] tenantscope-check[Story 1.20] audit-check[Story 1.21]）migrations-seal"
	@echo "迁移(Story 1.6): migrate（MIGRATE_ARGS=plan|up|down|status|recover|lint ...）migrate-test"
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

# 竞态检测下集成测试明显变慢（tests/integration/auth 单包约 10 分钟，已逼近 go test 默认的 10 分钟包超时），故显式放宽包超时。
race:
	CGO_ENABLED=1 $(GO) test -race -timeout 30m ./...

fmt:
	gofmt -l -w .

vet:
	$(GO) vet ./...
	$(GO) vet -tags lingedge_testdouble ./...

# make lint：gofmt + go vet + 脚手架结构测试 + 依赖图/结构检查 + golangci-lint（depguard 依赖方向与 sqlx 边界、forbidigo 禁 exec/直接读时钟/SQL 拼接/受限池与 Scope 构造）+ shell 拼接检查 + Repository Scope 首参检查。
lint: vet
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt 未格式化:" >&2; echo "$$out" >&2; exit 1; fi
	$(GO) test -short -count=1 ./tests/scaffold/...
	$(MAKE) --no-print-directory arch-check golangci shell-check repo-check

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

# Repository 的 Scope 首参（Story 1.5 AC5、ADR-009 §1）：internal/**/repository 包内导出方法的首个业务参数必须是 scope.Scope；
# 平台级表需在文档注释登记 lingedge:platform-table=<表>（清单 scripts/gates/data/platform-tables.txt）。
repo-check:
	$(GATES) repo

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

# --- 迁移框架（Story 1.6）----------------------------------------------------------------------------
# `make migrate MIGRATE_ARGS="status --host 127.0.0.1 --database lingedge"`：构建 lingedge-server 并运行其 migrate 子命令
# （迁移账号口令经环境变量 LINGEDGE_MIGRATE_DB_PASSWORD 或 LINGEDGE_MIGRATE_DB_PASSWORD_FILE，不经命令行；其后依次是
# root 侧 migrate.yaml 与交互输入，见 docs/operations/secrets.md §8、ADR-026 §4）。
MIGRATE_ARGS ?= help
migrate:
	@mkdir -p bin
	@$(GOBUILD) -o bin/lingedge-server ./cmd/lingedge-server
	@bin/lingedge-server migrate $(MIGRATE_ARGS)

# 迁移测试台（AC5/AC6）：① 内嵌迁移的静态 lint（分类标注/命名/幂等/逐表授权/DDL 预检）；② 迁移包单元测试；
# ③ tests/migrations：真实 MySQL 上的 DDL 规则 26 例、M1 骨架 61+6 表、13 个迁移经框架重放、升降级/校验和/dirty/崩溃/互斥/预检、lint 负向夹具。
# 用独立临时 mysqld 实例（tests/mysqltest；不触碰本机常驻实例，用后删除）。MySQL 缺失时缺省视为失败：
# MIGRATE_TEST_REQUIRE_MYSQL=0 可降级为「跳过并标等待环境验证」（跳过不代表通过）。8.0.16 最低版本需在 CI/验收机另行验证。
MIGRATE_TEST_REQUIRE_MYSQL ?= 1
migrate-test:
	$(GO) run ./cmd/lingedge-server migrate lint
	$(GO) test -count=1 ./internal/platform/migrate/...
	LINGEDGE_REQUIRE_MYSQL=$(MIGRATE_TEST_REQUIRE_MYSQL) $(GO) test -count=1 -v ./tests/migrations/...

# --- 契约工具链（Story 1.3）---------------------------------------------------------------------------

contract-bin:
	@$(GO) build -C scripts/contract -o ../../$(CONTRACT_BIN) .

# 契约检查程序自身的单元自测（含缺元数据 / R-T1 泄漏 / 破坏性变更 / 改动已发布迁移 / buf 破坏性检测的负向用例必须失败）。
contract-selftest:
	cd scripts/contract && $(GO) vet ./... && BUF='$(BUF)' OASDIFF='$(OASDIFF)' $(GO) test -count=1 ./...

# 生成 gRPC/Protobuf Go 代码到 $(PROTO_OUT)（buf generate；插件版本固定在 proto/buf.gen.yaml）。
# 生成物暂不入库、不进根 go.mod，原因见 scripts/proto-check-gen.sh 头注释（go1.27.1 标准库 net/http 的已公布漏洞）。
PROTO_OUT ?= dist/proto-gen
proto:
	rm -rf $(PROTO_OUT)
	cd proto && $(BUF) generate --output ../$(PROTO_OUT)
	printf 'module github.com/wuekevin/lingedge/proto\n\ngo 1.27.0\n' > $(PROTO_OUT)/go.mod

# buf lint + 对 $(CONTRACT_BASE) 的 breaking 检测 + 代码生成可再现且可编译（生成两次比对 + 临时 module 真实编译）。
proto-check:
	cd proto && $(BUF) lint
	$(BUF) breaking proto --against '.git#ref=$(CONTRACT_BASE),subdir=proto'
	BUF='$(BUF)' bash scripts/proto-check-gen.sh

# OpenAPI 3.1 校验 + 元数据/路径无冒号/同位置路径参数名一致/R-T1/x-reauth/分页/无内部 id 静态规则（Story 1.3 AC1、AC8），
# 再校验运行时路由清单（Story 1.12 AC2/AC7：lingedge-server routes 导出的真实注册路由 ⊆ openapi.yaml 且元数据与 x-* 一致、路径不含 ':'），
# 并输出「M1 未实现 operation 清单」（阶段出口用 `make openapi-check M1_EXIT=1`：清单非空即失败，不以 501 假实现充数），
# 最后相对 $(CONTRACT_BASE) 做破坏性变更检测（Story 1.3 AC2）。
ROUTES_JSON ?= bin/routes.json
M1_EXIT     ?=
openapi-check: contract-bin
	@mkdir -p bin
	$(GO) run ./cmd/lingedge-server routes -o $(ROUTES_JSON)
	$(CONTRACT) openapi-check -routes $(ROUTES_JSON) $(if $(M1_EXIT),-require-m1-complete)
	$(CONTRACT) openapi-breaking -base-ref '$(CONTRACT_BASE)' -oasdiff '$(OASDIFF)' $(if $(CONTRACT_NOTE),-note '$(CONTRACT_NOTE)')

openapi-breaking: contract-bin
	$(CONTRACT) openapi-breaking -base-ref '$(CONTRACT_BASE)' -oasdiff '$(OASDIFF)' $(if $(CONTRACT_NOTE),-note '$(CONTRACT_NOTE)')

# migrations/CHECKSUMS：已发布迁移被改动/删除即失败；新增迁移须带合法的 lingedge:class 首行标注（完整 lint 见 make migrate-test）。
migrations-check: contract-bin
	$(CONTRACT) migrations-check -base-ref '$(CONTRACT_BASE)'

# 把未登记的迁移追加登记为「已发布」（只追加，不改写已有行；由评审后的发布动作调用）。
migrations-seal: contract-bin
	$(CONTRACT) migrations-seal

# CF-1 冻结前置复核（AC7/AC9）：M1=200 operation、proto 符号、迁移顺序与骨架一致、待环境验证登记、评审记录、TaskType 口径。
freeze-check: contract-bin
	$(CONTRACT) freeze-check

# 权限一致性（Story 1.18 AC1、ADR-018 §8）：代码权限注册表 ⇄ 种子迁移 ⇄ openapi.yaml 的 x-permissions 三方一致，且权限 scope 与路由的 x-tenant-scope 相容；
# 门禁自身的负向样本在 scripts/gates/perms_test.go（随 make gate-selftest 运行）。
perms-check:
	$(GATES) perms

# 租户 Scope 覆盖（Story 1.20 AC4、ADR-009 §7）：真实路由集合里 x-tenant-scope 为 tenant|either 的路由必须经 tenantscope（Scope 钩子），否则失败；
# 检查器自身的负向样本（旁路路由/自带宽松钩子/空路由集）在 internal/platform/httpx/contracttest（随 make race 与此目标一同运行）。
tenantscope-check:
	$(GO) test -count=1 -run 'TestCheckScopeCoverage' ./internal/platform/httpx/contracttest/
	$(GO) test -count=1 -run 'TestTenantScopeCoverage' ./cmd/lingedge-server/

# 审计覆盖（Story 1.21 AC1、FR-23）：「声明 Audit 但未记录」与「写 operation 缺 Audit 声明」都被发现——
# ① `gates audit`：openapi 的写 operation 须声明 x-audit（例外清单 scripts/gates/data/audit-exempt.txt，带理由）、audit.Catalog ⇄ openapi 一致、每个声明的动作都有 .Record(ctx, audit.ActionX, …) 调用；
# ② 真实路由集合的同类检查（cmd/lingedge-server 的 TestAuditCoverage）；检查器自身的负向样本在 scripts/gates/audit_test.go（make gate-selftest）与 internal/platform/httpx/contracttest。
audit-check:
	$(GATES) audit
	$(GO) test -count=1 -run 'TestCheckAuditCoverageNegativeSamples' ./internal/platform/httpx/contracttest/
	$(GO) test -count=1 -run 'TestAuditCoverage' ./cmd/lingedge-server/

contract-check: contract-selftest openapi-check proto-check migrations-check freeze-check perms-check tenantscope-check audit-check

frontend:
	$(call not_implemented,1.36)

e2e:
	$(call not_implemented,1.37)

edge-test:
	$(call not_implemented,3.22)

# 本目标当前只覆盖 Story 1.2 的静态门禁：G-6 固定口令、G-11 测试替身隔离、G-12 命令注入（Story 1.14 追加任务类型白名单 tasktype）、NFR-29 品牌。
# Story 1.11 追加 G-5（日志脱敏扫描：向真实 logx 注入私钥/Token/主密钥/明文，断言输出无泄露，含负向自测）。
# 威胁模型 §9 的其余门禁（G-1、G-3、G-7…G-10、G-13；Story 1.26 起 G-4 已接入：gates extract + tests/security 路径穿越向量集）尚未接入，由对应 Story 逐项追加到本目标；
# Story 1.23 追加三个信任域门禁（gates trustdomain：Server 代码路径不得存在加载发行清单私钥的逻辑；tests/security/trustdomain 对真实二进制符号表做同样检查，含负向样本）与 G-5 签名密钥材料日志脱敏用例。
# Story 1.20 追加 G-2（多租户隔离矩阵 tests/security/tenant_isolation：真实临时 MySQL/Redis 上的真实进程内装配；缺 MySQL/Redis 时测试以「等待环境验证」跳过——
# 跳过不代表通过，LINGEDGE_REQUIRE_MYSQL=1 / LINGEDGE_REQUIRE_REDIS=1 可把缺失视为失败）。
# 因此本目标通过只表示上述已列项通过，不代表整套安全门禁通过。
security:
	$(GATES) testdouble
	$(GATES) passwords
	$(GATES) brand
	$(GATES) shell
	$(GATES) tasktype
	$(GATES) trustdomain
	$(GATES) flags
	$(GATES) extract
	$(GO) test -count=1 ./tests/security/...
	@echo "make security: 已覆盖 G-2（租户隔离矩阵）/G-4（Tools 解压：extract 门禁 + tests/security 路径穿越向量集）/G-5/G-6/G-11/G-12（含任务类型白名单 tasktype、Tools 白名单执行入口 safeexec 的元字符注入向量）/三个信任域（trustdomain）/CLI 旧标志名（flags，MIN-04）/品牌；未接入（随对应 Story 追加）: G-1 G-3 G-7 G-8 G-9 G-10 G-13" >&2

release:
	$(call not_implemented,1.34)

clean:
	rm -rf $(DIST) bin
