---
title: 'Story 1.1：Go monorepo 脚手架与目录骨架'
type: 'feature'
created: '2026-11-16'
status: 'in-review'
route: 'dispatch'
review_loop_iteration: 0
baseline_commit: 'NO_COMMIT（仓库尚无任何提交；以工作树快照为基线）'
context:
  - docs/bmad/epics.md（Story 1.1 全文，约 812–845 行）
  - docs/architecture/dev-environment.md（§3 仓库初始化约定）
  - docs/architecture/adr/ADR-001-modular-monolith.md
---

<frozen-after-approval reason="human-owned intent — do not modify unless human renegotiates">

## Intent

**Problem:** 仓库只有文档与 proto，没有可编译的工程骨架，后续 Story 无统一的构建/测试约定。

**Approach:** 按基线 §20 建立 Go monorepo：单一 `go.mod`、7 个仅输出真实 `--version` 的 `cmd/*` 入口、全部目录骨架、含 dev-environment §3 全部目标的 Makefile（未实现目标以 `NOT_IMPLEMENTED(story X.Y)` 非零退出）、README、.gitignore，并用真实运行的测试守住 AC。

## Boundaries & Constraints

**Always:** 模块路径 `github.com/wuekevin/lingedge`（UD-1）；`go` 与 `toolchain` 行固定 1.27.1；`go.sum` 入库、无本地 `replace`；构建 `CGO_ENABLED=0`；`make build` 覆盖 linux/amd64、linux/arm64，reporter 另含 windows/amd64；无业务逻辑、无伪数据。

**Never:** Docker/Compose/K8s 任何文件；`lingedge-updater` 目标或目录；空目标假通过；引入未使用的第三方依赖；git commit（本次会话用户未授权，AC7 标待授权）；改动 `SmartCDN`。

</frozen-after-approval>

## Code Map

- `docs/prd/LINGEDGE_V2_REQUIREMENTS.md` §20 -- 目录清单（权威）
- `docs/architecture/dev-environment.md` §3 -- Makefile 目标表
- `docs/architecture/adr/ADR-001-modular-monolith.md` §3 -- 模块根包仅 `api.go`，实现在 `internal/<mod>/internal/…`（后续 Story 落地）
- `proto/lingedge/node/v1/*.proto` -- 已存在，`go_package` 已用 UD-1 路径，本 Story 不生成代码（1.3）

## Tasks & Acceptance

**Execution:**
- [x] `go.mod`、`go.sum` -- 模块与 toolchain 固定
- [x] `pkg/version/` -- 版本信息与 `CLI` 入口（ldflags 注入，真实输出）及单元测试
- [x] `cmd/*/main.go` x7 -- 调用 `version.CLI`
- [x] 基线 §20 全部目录（`doc.go` / `.gitkeep`）
- [x] `Makefile` -- 全部目标；`build` 含交叉编译矩阵；`verify-build`、`check-scaffold`
- [x] `tests/scaffold/` -- 目录、go.mod、Docker、updater、Makefile 目标存在性与 NOT_IMPLEMENTED 语义、7 入口真实 `--version` 的测试
- [x] `README.md`、`.gitignore`
- [x] `docs/bmad/sprint-status.yaml` -- 状态回写

**Acceptance Criteria:** 见 `docs/bmad/epics.md` Story 1.1 的 AC1–AC7（原文为准）。

## Implementation Notes

见文末「开发记录」。

## Spec Change Log

## Review Triage Log

三路评审（blind / edge-case / verification-gap，均为无上下文子代理）共约 30 条发现，分级如下。

| 发现 | 判定 | 处置 |
|---|---|---|
| Blind：diff 不含全部 cmd/目录/.gitkeep（脚手架不完整） | false | 评审用 diff 仅抽样了部分文件；全量文件均存在，`TestRequiredDirsExist`/`TestCmdEntriesAreVersionOnly` 通过 |
| `make build`/`verify-build.sh` 从未被测试真实执行；ldflags 注入无验证；校验脚本无负向用例 | medium | patch：新增 `TestBuildMatrixAndVerifier`（真实 `make build`+`verify-build`，缺失/架构不符/目录缺失三个负向，Makefile `print-ldflags` 编出宿主二进制断言注入值） |
| `walkSource` 仅按根相对路径跳过 node_modules 等；内容扫描会命中 go.sum/lock 中的 docker 依赖路径 | medium | patch：按目录名任意深度跳过；内容扫描跳过 go.mod/go.sum/pnpm-lock/package.json |
| `verify-build.sh` 在 dist 缺失、非 Go 产物时直接中断；未检查 `-tags` | medium | patch：补目录检查、`fail=1; continue`、禁止 `-tags`；`GOBUILD` 置 `GOFLAGS=` 防环境注入 |
| `repoRoot` 依赖 `runtime.Caller`，`-trimpath` 下失效 | low | patch：改为 `os.Getwd` 向上查找 |
| go/toolchain 行测试仅匹配 1.27.x，未真正固定 | low | patch：精确匹配 `go 1.27.0` / `toolchain go1.27.1` |
| 无参数退出码仅断言非零 | low | patch：断言恰为 2 |
| `.gitignore` 误伤 `.env.example`；漏 docs-site/status-site 产物、go.work | low | patch |
| `make lint` 重复跑全量编译型测试 | low | patch：lint 使用 `-short` |
| Docker/k8s 词汇扫描可能误报安全测试载荷 | maybe-false | defer：NFR-7 要求不依赖容器；当前无此类 fixture，出现时用目录白名单（1.2 门禁一并收敛） |
| `.gitattributes`/CRLF、CI workflow 缺失、README 前置依赖说明、`--help` 退出 0、VERSION 含引号校验、race 无 C 编译器提示、Windows `.exe`/超时 | low | 驳回/不做：属增量复杂度或超出 1.1 范围（CI 配置随 1.2 门禁；Windows 宿主非开发平台） |
| 构建矩阵在 Makefile/脚本/测试三处各写一次 | low | 驳回：脚本由 Makefile 传入 cmd 列表，矩阵固定于 AC1，漂移由 `TestBuildMatrixAndVerifier` 捕获 |
| `-tags` 仅正则校验 | low | 已被 `verify-build.sh` 对产物 `go version -m` 的 `-tags` 检查覆盖 |

## Verification

**Commands:**
- `go build ./...`、`go vet ./...`、`go test ./...` -- 全部通过
- `make build && make verify-build` -- 矩阵产物齐全且 GOOS/GOARCH 正确
- `make check-scaffold` -- 通过

## 开发记录

**开发会话**：2026-11-16，bmad-build（非交互、完全自主）。执行环境：macOS（Darwin 25.6.0 arm64）、go1.27.1。

### 决策与偏差

- **BMad 初始化**：项目根无 `_bmad`，按技能指引运行 `bmad setup`（无待答问题）生成 `_bmad/`；`.gitignore` 忽略可再生的 `_bmad/render/` 与 `_bmad-output/`，其余 `_bmad/` 未处理。
- **Story 文件位置**：沿用 `sprint-status.yaml` 的 `story_location: docs/bmad/stories`，而非工作流默认的 `_bmad-output/`。
- **go.mod**：写 `go 1.27.0` + `toolchain go1.27.1`。原因：`go 1.27.1` 与 `toolchain go1.27.1` 相同时，Go 会判定 toolchain 行冗余并在 `go mod tidy` 时删除（`go build` 报 "updates to go.mod needed"），无法固定；改为语言基线 1.27.0、toolchain 固定 1.27.1，经 `go build`/`go mod tidy` 验证稳定。
- **go.sum**：无任何第三方依赖，`go.sum` 为空文件入库（AC3 要求存在）。依赖随后续 Story 引入。
- **版本注入**：`pkg/version` 经 ldflags 注入 Version/Commit；仓库尚无提交时 `git rev-parse` 失败，回落 `0.0.0-dev`/`unknown`（真实的「未知」，非伪造）。`make build` 带 `-buildvcs=false` 以免无提交仓库的 VCS 戳报错。
- **模块根包**：`internal/<mod>` 仅放 `doc.go`；ADR-001 §3 的 `api.go` 与 `internal/{handler,service,repository,domain}` 子包随各模块 Story 创建，`platform/ports` 属 Story 1.2。
- **`internal/tools/updater`**：基线 §20 明确列出的 Tools 内部包，保留；AC4 禁止的是 `lingedge-updater` 构建目标/目录，由 `TestNoUpdaterTarget` 扫描全仓（docs 除外）。
- **未实现目标与 Story 映射**：migrate/migrate-test→1.6；proto/proto-check/openapi-check→1.3；frontend→1.36；e2e→1.37（首个 Playwright E2E）；edge-test→3.22；security→1.2；release→1.34。`lint` 当前为 gofmt + go vet + 脚手架结构测试，golangci-lint/depguard/forbidigo 属 1.2，已在 Makefile 与 README 注明。`test` 仅 Go 部分（前端单测随 1.36）。
- **AC7（首个提交）未完成**：本次调用方明确要求「不要 git commit」，而 AC7 规定提交须在用户授权范围内由执行会话完成；此处未获授权，故**不提交**。1.3 的 `contract-v1` 标签依赖此提交，须先由用户授权（或由用户自行）提交 `docs/`、`proto/` 与脚手架，再开工 1.3。

### 实际验证（本机 macOS arm64、go1.27.1，未经任何伪造）

- `go build ./...` → 通过；`go vet ./...` → 通过；`gofmt -l .` → 无输出。
- `go test -count=1 ./...` → `ok pkg/version`、`ok tests/scaffold`（其余包 no test files）。
- `make build` → 15 个产物（linux/amd64×7、linux/arm64×7、windows/amd64 reporter.exe）；`make verify-build` → 15 个 `ok`，GOOS/GOARCH 与产物数量校验通过。
- `make lint` → 通过；`make race`（`-race`）→ 两个包通过。
- 10 个未实现目标逐一执行：均输出 `NOT_IMPLEMENTED(story X.Y)` 且非零退出。
- 负向自测（手动，已还原）：临时加入 `Dockerfile`、`cmd/lingedge-updater/`、含 `docker run` 的脚本，`go test ./tests/scaffold` 对应三类检查均失败；删除/替换产物，`verify-build` 失败。
- 7 个入口真实 `--version`：宿主（darwin/arm64）逐个编译并运行，输出形如 `lingedge 9.9.9-test (commit deadbeef, go1.27.1, darwin/arm64)`；无参数退出码 2。linux/windows 产物因宿主非同平台未执行，仅做 `go version -m` 元数据校验。

### AC 对照

| AC | 结果 |
|---|---|
| 1 构建与交叉编译矩阵 | 通过（`make build`/`verify-build` 与测试） |
| 2 §20 目录齐全且不入「已交付」清单 | 通过（`TestRequiredDirsExist`；README 状态表标「骨架（未交付）」）；`docs/api/openapi.yaml` 为既有文档，未改动 |
| 3 go.mod | 通过（路径 UD-1、go/toolchain 固定、go.sum 入库、无本地 replace；与 `go 1.27.1` 的偏差见上） |
| 4 无 lingedge-updater | 通过（`TestNoUpdaterTarget`） |
| 5 Makefile 全部目标，未实现者 NOT_IMPLEMENTED 非零 | 通过（`TestMakefileTargets` 与手工逐个执行） |
| 6 README 与禁 Docker | 通过（`TestNoDockerArtifacts`、`TestReadmeBranding`） |
| 7 首个提交 | **未完成：等待用户授权**（见上） |

### DoD（基线 §25）

需求明确、代码完成、编译通过、单元测试通过、关键异常（负向）测试通过、文档更新、运行验证（本机）：满足。数据库迁移、API、前端功能：本 Story 不适用。集成测试：无外部依赖，等价于上述 `tests/scaffold`。因 AC7 未满足，状态标 `review` 而非 `done`。
