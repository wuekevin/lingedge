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

# 前端 workspace（Story 1.36、ADR-014）：pnpm 版本由 frontend/package.json 的 packageManager 固定；全部检查入口见 frontend/README.md。
PNPM     ?= pnpm
FRONTEND := frontend

define not_implemented
	@echo "NOT_IMPLEMENTED(story $(1))" >&2; exit 1
endef

.PHONY: help build verify-build test race lint fmt vet check-scaffold clean \
        gen-lint arch-check golangci shell-check repo-check gate-selftest vuln verify-gates \
        release-keys-check contract-bin contract-selftest openapi-breaking migrations-check migrations-seal freeze-check perms-check tenantscope-check audit-check contract-check \
        frontend-install frontend-audit frontend-lint frontend-test frontend-types-check frontend-gate-selftest frontend-check setup-web setup-web-check admin-e2e-build \
        print-ldflags migrate migrate-test proto proto-check openapi-check frontend e2e edge-test security release release-finalize release-verify release-sign release-sign-test release-test secrets-check

help:
	@echo "目标: build verify-build test race lint migrate migrate-test proto proto-check openapi-check frontend e2e edge-test security release"
	@echo "辅助: fmt vet check-scaffold clean gen-lint arch-check golangci shell-check gate-selftest vuln verify-gates"
	@echo "契约(Story 1.3): contract-check（= contract-selftest openapi-check proto-check migrations-check freeze-check perms-check[Story 1.18] tenantscope-check[Story 1.20] audit-check[Story 1.21]）migrations-seal"
	@echo "迁移(Story 1.6): migrate（MIGRATE_ARGS=plan|up|down|status|recover|lint ...）migrate-test"
	@echo "安装向导(Story 1.37): setup-web（构建并同步嵌入产物）setup-web-check（漂移/缺失检查）e2e（Playwright 浏览器端到端，需 ENV-BROWSER）"
	@echo "前端(Story 1.36): frontend-check（= 依赖锁定/ESLint/架构门禁/令牌与对比度/i18n 缺键/类型漂移/vue-tsc/vitest/构建/隔离与 mock 检查）；已并入 lint（frontend-lint）、test（frontend-test）、gate-selftest（frontend-gate-selftest）、contract-check（frontend-types-check）、verify-build（frontend）"
	@echo "发行(Story 1.34): release → release-finalize → release-verify（两轮离线签名，见 docs/operations/release.md）release-sign（构建离线签名工具）release-sign-test release-test secrets-check"
	@echo "未实现的目标会输出 NOT_IMPLEMENTED(story X.Y) 并以非零退出。"

# 内置发行公钥守卫（Story 1.27 AC5、NFR-1）：internal/tools/manifest/builtin/keys.json 由发行流水线在构建前注入（不入库）。
# 含测试公钥/测试 key_id/私钥字段/杂散文件 → 任何构建失败；缺失 → 开发构建放行（lingedge 运行期对任何清单失败关闭），
# 发行构建（make build RELEASE=1；make release 经同一守卫，Story 1.34）失败。门禁自测见 scripts/gates/releasekeys_test.go、tests/gates、tests/security/releasekeys。
release-keys-check:
	$(GATES) release-keys $(if $(RELEASE),-require,)

# 生产构建不得带 lingedge_testdouble 标签（此处从不传入 -tags）。
build:
	@$(MAKE) --no-print-directory release-keys-check
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

# Go 构建产物校验 + 前端生产构建后的隔离与 mock 检查（Story 1.36：Console 产物不含 platform 端点/Admin 路由，生产构建不含 mock 入口）。
verify-build:
	@bash scripts/verify-build.sh $(DIST) "$(CMDS)"
	@$(MAKE) --no-print-directory frontend

# Go 测试 + 前端 vitest（shared 组件/格式化/i18n；门禁脚本自测在 gate-selftest）。
test:
	$(GO) test ./...
	@$(MAKE) --no-print-directory frontend-test

# 竞态检测下集成测试明显变慢（tests/integration/auth 单包约 10 分钟，已逼近 go test 默认的 10 分钟包超时），故显式放宽包超时。
race:
	CGO_ENABLED=1 $(GO) test -race -timeout 30m ./...

fmt:
	gofmt -l -w .

vet:
	$(GO) vet ./...
	$(GO) vet -tags lingedge_testdouble ./...

# make lint：gofmt + go vet + 脚手架结构测试 + 依赖图/结构检查 + golangci-lint（depguard 依赖方向与 sqlx 边界、forbidigo 禁 exec/直接读时钟/SQL 拼接/受限池与 Scope 构造）+ shell 拼接检查 + Repository Scope 首参检查
# + 前端静态检查（frontend-lint，Story 1.36）。
lint: vet
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt 未格式化:" >&2; echo "$$out" >&2; exit 1; fi
	$(GO) test -short -count=1 ./tests/scaffold/...
	$(MAKE) --no-print-directory arch-check golangci shell-check repo-check
	$(MAKE) --no-print-directory frontend-lint

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
	$(MAKE) --no-print-directory release-sign-test
	$(MAKE) --no-print-directory frontend-gate-selftest

# 依赖漏洞扫描（G-7；需联网，不在 make security 内，完整供应链门禁属 Story 1.34）：
#   Go：govulncheck（访问 vuln.go.dev）；前端：pnpm audit（Story 1.40，见 frontend-audit）。两部分都失败即失败，无豁免清单。
vuln:
	$(GOVULNCHECK) ./...
	@$(MAKE) --no-print-directory frontend-audit

# 前端依赖漏洞（Story 1.40，Dependabot #2 的后续）：对 frontend/pnpm-lock.yaml 固定的**全部**依赖（含 devDependencies：minimatch/glob 的告警就在开发依赖链上）
# 运行 pnpm audit，moderate 及以上即失败。注意：
#   · 需要访问 npm registry 的 audit 端点（与 govulncheck 访问 vuln.go.dev 同类的联网依赖）；联网失败按失败处理（fail closed），不会静默通过；
#   · 结果取决于当天的公告库（与 govulncheck 一样可能随时间出现新告警）——可确定、可复现的部分是：锁文件固定、overrides 精确版本由 check:deps 校验、
#     frontend-check 每次都在冻结锁文件上重跑全部检查；修复方式见 frontend/pnpm-workspace.yaml 顶部的 overrides 注释（先升级依赖，必要时用 overrides 钉到已修复的精确版本）。
FRONTEND_AUDIT_LEVEL ?= moderate
frontend-audit: frontend-install
	$(PNPM) -C $(FRONTEND) audit --audit-level $(FRONTEND_AUDIT_LEVEL)

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

# 前端类型漂移（Story 1.36 AC2）：frontend/shared/src/api-types/openapi.gen.ts 由 docs/api/openapi.yaml 生成；重新生成并逐字节比对，
# openapi.yaml 变动而生成物未同步 → 失败（改 OpenAPI 后运行 `pnpm -C frontend run gen:types` 并提交）。
frontend-types-check: frontend-install
	$(PNPM) -C $(FRONTEND) run check:types

contract-check: contract-selftest openapi-check proto-check migrations-check freeze-check perms-check tenantscope-check audit-check frontend-types-check

# --- 前端 workspace（Story 1.36、ADR-014、docs/architecture/dev-environment.md §4）------------------------------------------
# 依赖必须经 --frozen-lockfile 安装（锁文件入库；精确版本由 check:deps 校验；生命周期脚本关闭）。
frontend-install:
	$(PNPM) -C $(FRONTEND) install --frozen-lockfile

# 静态检查：依赖锁定 + 架构门禁（全仓禁 v-html/硬编码色值/硬编码文案；shared 禁路由表/菜单/权限/会话/API 客户端）+ ESLint
# + 令牌与 DESIGN.md 一致/无漂移 + 文字对比度 ≥ 4.5:1 + i18n 缺键 + vue-tsc。
frontend-lint: frontend-install
	$(PNPM) -C $(FRONTEND) run lint
	$(PNPM) -C $(FRONTEND) run check:tokens
	$(PNPM) -C $(FRONTEND) run check:contrast
	$(PNPM) -C $(FRONTEND) run check:arco-contrast
	$(PNPM) -C $(FRONTEND) run check:i18n
	$(PNPM) -C $(FRONTEND) run check:setup-codes
	$(PNPM) -C $(FRONTEND) run typecheck

# shared 的 vitest（三态组件、格式化、i18n）+ 安装向导的 vitest 组件测试（Story 1.37）+ Admin 外壳的 vitest（Story 1.38：守卫、菜单裁剪、三态、标签、搜索壳、横幅、登录）。
frontend-test: frontend-install
	$(PNPM) -C $(FRONTEND) run test:shared
	$(PNPM) -C $(FRONTEND) run test:setup
	$(PNPM) -C $(FRONTEND) run test:admin

# 门禁脚本自身的正向/负向自测（lint 负向用例、依赖锁定、i18n、令牌/对比度、构建隔离、mock、真实 vite 构建 e2e、pnpm 脚本关闭实测）。
frontend-gate-selftest: frontend-install
	$(PNPM) -C $(FRONTEND) run test:selftest

# 前端生产构建（admin、console、setup 各自独立的 Vite 工程）+ 安装向导嵌入产物漂移/缺失检查（Story 1.37）+ 构建隔离检查（IMP-14，含向导不依赖 Admin）+ mock 入口检查（NFR-1）。
frontend: frontend-install
	$(PNPM) -C $(FRONTEND) run build
	$(PNPM) -C $(FRONTEND) run check:embed
	$(PNPM) -C $(FRONTEND) run check:isolation
	$(PNPM) -C $(FRONTEND) run check:mock

# 安装向导 SPA（Story 1.37）：构建 frontend/setup 并同步到 internal/platform/setup/webdist（go:embed 的来源，产物入库）。
# 改了 frontend/setup 或 frontend/shared 之后必须运行并提交 webdist；`make verify-build`（经 frontend 目标）与 `setup-web-check` 会重新构建并逐字节比对，
# 漂移或缺失即失败；Go 侧 TestEmbeddedWizardIsARealBuild 另外保证嵌入内容是真实构建（不是占位页）。选择“入库 + 漂移检查”的理由见 frontend/README。
setup-web: frontend-install
	$(PNPM) -C $(FRONTEND) run build:setup
	$(PNPM) -C $(FRONTEND) run sync:embed

setup-web-check: frontend-install
	$(PNPM) -C $(FRONTEND) run build:setup
	$(PNPM) -C $(FRONTEND) run check:embed

# 前端全部检查一次跑完（等价于上面各目标之和）。
frontend-check: frontend-install
	$(PNPM) -C $(FRONTEND) run check

# 浏览器端到端（Story 1.37，ENV-BROWSER）：Playwright 驱动真实 Chromium，被测对象是真实 lingedge-server（setup mode，内嵌向导 SPA）+ 自拉起的独立临时 mysqld/redis
# （tests/mysqltest、tests/redistest；不触碰本机常驻实例）。场景：全流程（TCP+TOTP+中文 / Unix socket+英文）、错 token（含 URL token 与限流）、非空库、中断续做（early/late）、键盘可操作、
# 对比度≥4.5:1 审计（含审计器自测）；截图落在 LINGEDGE_E2E_SHOT_DIR（缺省临时目录）。环境缺失时 go 测试会以「等待环境验证」跳过，本目标设置 REQUIRE 使缺失视为失败。
# 首次需要：pnpm -C frontend install --frozen-lockfile && pnpm -C frontend/setup exec playwright install chromium
#
# Story 1.38 起同一目标还覆盖 Admin（真实 server 正常模式 + Chromium：登录/TOTP/恢复码、无权限菜单隐藏、直访 403/404、租户账号被拒、手机宽度、亮/暗对比度）：
# 需要 Admin 的生产构建（frontend/dist/admin）与 e2e 构建（frontend/dist/admin-e2e，含夹具页面；不是发行物）——由 admin-e2e-build 先行构建。
#
# Story 1.39 起同一目标还覆盖 Admin 系统管理页（生产构建 × 真实 server 正常模式 × Chromium）：创建角色→分配→无权限用户验证、API Token 明文一次、L3 运行模式 + L4 再认证（口令 + TOTP）、
# 审计游标分页与过滤、品牌修改读到新值、版本信息与二进制自报版本/库里迁移号一致、再认证窗口外、手机宽度、亮暗对比度审计（TestBrowserAdminSystem / TestBrowserAdminSystemSettings）。
e2e: setup-web-check admin-e2e-build
	LINGEDGE_REQUIRE_MYSQL=1 LINGEDGE_REQUIRE_REDIS=1 LINGEDGE_REQUIRE_BROWSER=1 $(GO) test -count=1 -timeout 45m -v -run 'TestBrowser' ./tests/integration/setup/

# Admin E2E 所需的两份构建：生产构建（dist/admin）与 e2e 模式构建（dist/admin-e2e，额外含 src/mocks 的夹具页面，只供 Playwright 使用）。
admin-e2e-build: frontend-install
	$(PNPM) --dir $(FRONTEND) --filter @lingedge/admin run build
	$(PNPM) -C $(FRONTEND) run build:admin-e2e

edge-test:
	$(call not_implemented,3.22)

# 本目标当前只覆盖 Story 1.2 的静态门禁：G-6 固定口令、G-11 测试替身隔离、G-12 命令注入（Story 1.14 追加任务类型白名单 tasktype）、NFR-29 品牌。
# Story 1.11 追加 G-5（日志脱敏扫描：向真实 logx 注入私钥/Token/主密钥/明文，断言输出无泄露，含负向自测）。
# 威胁模型 §9 的其余门禁（G-1、G-3、G-7…G-10、G-13；Story 1.26 起 G-4 已接入：gates extract + tests/security 路径穿越向量集）尚未接入，由对应 Story 逐项追加到本目标；
# Story 1.27 追加 release-keys（内置发行公钥守卫，含 -require 的发行模式）。Story 1.34 追加 secrets（仓库密钥扫描）。
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
	$(GATES) release-keys
	$(GATES) secrets
	$(GO) test -count=1 ./tests/security/...
	@echo "make security: 已覆盖 G-2（租户隔离矩阵）/G-4（Tools 解压：extract 门禁 + tests/security 路径穿越向量集）/G-5/G-6/G-11/G-12（含任务类型白名单 tasktype、Tools 白名单执行入口 safeexec 的元字符注入向量）/三个信任域（trustdomain）/发行公钥构建守卫（release-keys，Story 1.27 AC5；发行构建 RELEASE=1 缺失即失败）/仓库密钥扫描（secrets，G-7 的密钥扫描部分：仓库与 CI 中不存在发行私钥，Story 1.34 AC2）/CLI 旧标志名（flags，MIN-04）/品牌；G-7 其余部分：依赖漏洞见 make vuln，SBOM 随 make release 产出，许可证扫描尚未接入；G-8 篡改测试由 tests/release 覆盖（make test）；未接入（随对应 Story 追加）: G-1 G-3 G-9 G-10 G-13" >&2

# --- 发行流水线与离线签名流程（Story 1.34、ADR-012 §1.5/§1.10、tools-design §9、docs/operations/release.md）----------------------
# 在线流水线（scripts/release）只产出待签物与组装，没有任何签名代码、不接触私钥；签名只在离线机用 scripts/release-sign（独立 module）完成。
# 两轮签名（包内 SHA256SUMS.sig 在 .tar.gz 里面，包的外层 sha256 写进清单，清单再被签名）：
#   make release            构建（RELEASE=1 守卫 + 可复现性双构建）+ stage + 第 1 轮待签清单 to-sign-1.json   [在线]
#   release-sign sign ...   离线机签第 1 轮                                                                   [离线]
#   make release-finalize   验第 1 轮签名 → 最终 .tar.gz → 待签 manifest.json + to-sign-2.json              [在线]
#   release-sign sign ...   离线机签第 2 轮（清单）                                                          [离线]
#   make release-verify     用 Tools 同一条校验链验证 → publish/ 发行目录                                    [在线]
# 变量：RELEASE_VERSION（必填，semver）RELEASE_KEYS（真实发行公钥集 keys.json，UD-3；注入内置目录后经 RELEASE=1 守卫）
#       RELEASE_CHANNEL（stable|beta）RELEASE_SEQUENCE（必填，单调递增）RELEASE_BASE_URL（必填，发行源 https 地址，T-3）
#       RELEASE_SIGS（第 1 轮 .sig 目录，缺省 <out>/signatures）RELEASE_FLAGS（透传，如 -platforms/-allow-dirty/-openresty-tree）RELEASE_FINALIZE_FLAGS（透传，如 -key-rotation/-revoke）
# 发行构建总经 `release-keys-check RELEASE=1`（与 make build RELEASE=1 同一守卫：缺失/含测试公钥即失败）；流水线 build 内部再检一次并扫描产物。
RELEASE_VERSION  ?=
RELEASE_CHANNEL  ?= stable
RELEASE_SEQUENCE ?=
RELEASE_BASE_URL ?=
RELEASE_KEYS     ?=
RELEASE_SIGS     ?=
RELEASE_FLAGS    ?=
RELEASE_FINALIZE_FLAGS ?=
RELEASE_OUT      ?= bin/release/$(RELEASE_VERSION)
RELEASE_PUBKEYS  ?= internal/tools/manifest/builtin/keys.json
RELEASE_REQUIRE_RUBY ?= 1

release:
	@test -n "$(RELEASE_VERSION)" || { echo "release: 需要 RELEASE_VERSION=<semver>" >&2; exit 2; }
	@if [ -n "$(RELEASE_KEYS)" ]; then install -m 0644 "$(RELEASE_KEYS)" $(RELEASE_PUBKEYS); fi
	@$(MAKE) --no-print-directory release-keys-check RELEASE=1
	$(GO) run ./scripts/release build -src . -out $(RELEASE_OUT) -version $(RELEASE_VERSION) $(RELEASE_FLAGS)

release-finalize:
	@test -n "$(RELEASE_VERSION)" && test -n "$(RELEASE_SEQUENCE)" && test -n "$(RELEASE_BASE_URL)" || { echo "release-finalize: 需要 RELEASE_VERSION RELEASE_SEQUENCE RELEASE_BASE_URL" >&2; exit 2; }
	$(GO) run ./scripts/release finalize -out $(RELEASE_OUT) -keys $(RELEASE_PUBKEYS) -channel $(RELEASE_CHANNEL) -sequence $(RELEASE_SEQUENCE) -base-url $(RELEASE_BASE_URL) $(if $(RELEASE_SIGS),-sigs $(RELEASE_SIGS),) $(RELEASE_FINALIZE_FLAGS)

release-verify:
	@test -n "$(RELEASE_VERSION)" || { echo "release-verify: 需要 RELEASE_VERSION" >&2; exit 2; }
	$(GO) run ./scripts/release verify -out $(RELEASE_OUT) -keys $(RELEASE_PUBKEYS)

# 构建离线签名工具（在离线机上执行；只需 Go toolchain，无第三方依赖）。
release-sign:
	@mkdir -p bin
	$(GO) build -C scripts/release-sign -o ../../bin/release-sign .
	@echo "已构建 bin/release-sign：拷到离线机运行，私钥只存在于离线介质"

# 离线签名工具自测（独立 module）：单元 + §9.6 向量复算 + Ruby OpenSSL 与 Go ed25519 双实现交叉验证。
# 缺少支持 Ed25519 的 Ruby 时缺省视为失败（RELEASE_REQUIRE_RUBY=0 可降级为「跳过并标等待环境验证」，跳过不代表通过）。
release-sign-test:
	cd scripts/release-sign && $(GO) vet ./... && LINGEDGE_REQUIRE_RUBY=$(RELEASE_REQUIRE_RUBY) $(GO) test -count=1 ./...

# 发行流水线测试：scripts/release 单测（符号链接消除、SBOM、迁移预告解析、确定性打包、无签名代码）+ tests/release 端到端
# （真实构建 → 离线签名（运行时随机密钥）→ Tools 验签通过/篡改拒绝、可复现性、公钥轮换、OpenResty 链接消除、Ruby 交叉验证）。
release-test: release-sign-test
	$(GO) test -count=1 ./scripts/release/... ./tests/release/...

secrets-check:
	$(GATES) secrets

clean:
	rm -rf $(DIST) bin
