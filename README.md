# LingEdge（凌界云）

LingEdge（凌界云）是一套从零开发的商业级 CDN 平台：官方网站、管理员后台、用户控制台、中央主控 Server、边缘节点 Node、智能 DNS、调度、区域监控探针 Reporter、后台任务 Worker、统一安装/升级/运维 CLI（LingEdge Tools），以及帮助文档与状态页。需求基线见 [`docs/prd/LINGEDGE_V2_REQUIREMENTS.md`](docs/prd/LINGEDGE_V2_REQUIREMENTS.md)，架构见 [`docs/architecture/architecture.md`](docs/architecture/architecture.md)。

**不使用 Docker。** 本项目不依赖 Docker、Compose 或 Kubernetes，任何部署与开发流程都不得将其作为必选组件（NFR-7）。运行形态为 systemd 服务（含宝塔面板场景）。

## 当前状态

仓库处于 Epic 1 起步阶段：已交付的只有工程骨架（Story 1.1）、静态质量门禁（Story 1.2）、契约工具链及冻结点 CF-1（Story 1.3）与 platform 基础库（Story 1.4：配置、日志脱敏、统一错误、Request ID、时钟与 ULID），均不含业务功能。下列二进制目前**仅支持 `--version`**，不含任何业务逻辑；表中「骨架」表示目录已建立、功能**未交付**。

| 组件 | 入口 / 目录 | 状态 |
|---|---|---|
| Tools CLI | `cmd/lingedge`、`internal/tools/*` | 骨架（未交付） |
| Server | `cmd/lingedge-server` | 骨架（未交付） |
| Node | `cmd/lingedge-node` | 骨架（未交付） |
| DNS | `cmd/lingedge-dns` | 骨架（未交付） |
| Scheduler | `cmd/lingedge-scheduler` | 骨架（未交付） |
| Reporter | `cmd/lingedge-reporter`（含 windows/amd64 构建） | 骨架（未交付） |
| Worker | `cmd/lingedge-worker` | 骨架（未交付） |
| 业务模块 | `internal/{auth,tenant,website,…}` | 骨架（未交付） |
| 平台基础库 | `internal/platform/{config,logx,apierr,requestid,clock,uid}`、`pkg/redact`（脱敏引擎，Server/Tools/Node 共用） | 已交付（Story 1.4；无业务功能，尚未被任何二进制装配） |
| 前端 / 官网 / 文档站 / 状态页 | `frontend/*`、`docs-site`、`status-site` | 骨架（未交付） |

## 开发

前置：Go 1.27.4 工具链（`go.mod` 固定 `go 1.27.0` 与 `toolchain go1.27.4`，修复 go1.27.1 的标准库已公布漏洞）。其余工具版本见 [`docs/architecture/dev-environment.md`](docs/architecture/dev-environment.md)。

```sh
make build          # 构建全部 cmd：linux/amd64、linux/arm64，reporter 另含 windows/amd64，产物在 dist/
make verify-build   # 校验交叉编译矩阵产物
make test           # go test ./...（含 MySQL 集成测试：自动起停独立临时 mysqld；无 mysqld 8.0 时跳过并标「等待环境验证」，见 tests/mysqltest）
make lint           # gofmt + go vet + 脚手架测试 + 依赖方向/结构检查 + golangci-lint（depguard、forbidigo）+ shell 拼接检查 + Repository Scope 首参检查（repo-check）
make security       # 固定口令（G-6）、测试替身隔离（G-11）、命令注入（G-12）、品牌残留（NFR-29）
make gate-selftest  # 门禁自测：在仓库副本里写入故意违规的样例，断言被拦截、合法写法不误伤
make gen-lint       # 据依赖图（scripts/gates/spec.go ← ADR-001）重新生成 .golangci.yml；改依赖图后必须运行
make vuln           # govulncheck（版本固定，需联网）

# 契约工具链（Story 1.3；buf、oasdiff 同样 go run <pkg>@<固定版本>，kin-openapi 在独立 module scripts/contract 内）
make contract-check   # = contract-selftest + openapi-check + proto-check + migrations-check + freeze-check
make openapi-check    # OpenAPI 3.1 校验 + 元数据/路径无冒号/R-T1/x-reauth/分页/无内部 id 静态规则，再对 contract-v1 做破坏性变更检测
make proto            # buf generate：生成 gRPC/Protobuf Go 代码到 dist/proto-gen/（暂不入库，见 scripts/proto-check-gen.sh）
make proto-check      # buf lint + 对 contract-v1 的 buf breaking + 代码生成可再现（生成两次比对）且可编译（临时 module 真实编译）
make migrations-check # migrations/CHECKSUMS：已发布迁移被改动/删除即失败
make freeze-check     # CF-1 冻结前置复核（AC7/AC9）
make contract-selftest # 契约检查程序自测（含负向用例）
```

外部工具（golangci-lint、govulncheck、buf、oasdiff）经 `go run <pkg>@<固定版本>` 调用，无需预装、不进 `go.mod`；版本见 Makefile。`make security` 目前只覆盖上述四类门禁，威胁模型 §9 的其余门禁随对应 Story 接入，通过不代表整套安全门禁通过。

Makefile 中尚未实现的目标（`migrate`、`migrate-test`、`frontend`、`e2e`、`edge-test`、`release`）执行时输出 `NOT_IMPLEMENTED(story X.Y)` 并以非零退出，不会假通过。

## 目录

以基线第 20 章为准：`cmd/`、`internal/`、`pkg/`、`proto/`、`frontend/`、`docs-site/`、`status-site/`、`edge/`、`migrations/`、`manifests/`、`configs/`、`deploy/`、`scripts/`、`tests/`、`docs/`。尚无代码的目录仅含 `doc.go` 或 `.gitkeep`。

## 约定

- 品牌统一为 LingEdge（凌界云）；不使用 Docker；生产构建不带 `lingedge_testdouble` 标签。
- 仓库内不含任何默认口令；本地凭据经环境变量或未入库的 `*.local.yaml`。
- 契约（`docs/api/openapi.yaml`、`proto/`、`migrations/`）已于 CF-1 冻结为 git 标签 `contract-v1`（本地标签；推送前尚未同步远端）：此后破坏性变更须 ADR 与变更说明（`docs/bmad/contract-changes/_template.md`，ADR-017 §10），否则 `make openapi-check` / `make proto-check` / `make migrations-check` 失败。
