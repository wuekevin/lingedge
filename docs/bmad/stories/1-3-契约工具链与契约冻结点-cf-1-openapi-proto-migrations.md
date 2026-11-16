---
title: 'Story 1.3：契约工具链与契约冻结点 CF-1（OpenAPI / proto / migrations）'
type: 'feature'
created: '2026-11-17'
status: 'in-review'
route: 'dispatch'
review_loop_iteration: 0
baseline_commit: '1ef0f8c（main 首个提交；本 Story 的改动在其之上形成第二个提交，contract-v1 指向该提交）'
context:
  - docs/bmad/epics.md（Story 1.3 全文，约 862–892 行）
  - docs/bmad/decisions-adopted.md（UD-25、AA-6、AA-8）
  - docs/bmad/architecture-revision-log.md（§1、§3、§4、§5 IMP-12/IMP-21）
  - docs/api/openapi.yaml、docs/api/README.md
  - proto/lingedge/node/v1/*.proto
  - docs/database/README.md（§8.1）、docs/database/verification/
  - docs/bmad/stories/1-1-go-monorepo-脚手架与目录骨架.md、1-2-静态质量门禁集-….md
---

<frozen-after-approval reason="human-owned intent — do not modify unless human renegotiates">

## Intent

**Problem:** 契约（OpenAPI、Node proto、迁移/ERD）只有文档，没有机器可检查的冻结与破坏性变更检测：`make openapi-check`/`make proto`/`make proto-check` 仍是 `NOT_IMPLEMENTED(story 1.3)`，官网与基础平台无法在稳定契约上并行。

**Approach:** 以「独立 Go module 的契约检查程序（kin-openapi）+ 固定版本的外部工具（oasdiff、buf，均 `go run <pkg>@<版本>`）」落地：OpenAPI 3.1 校验与静态规则（元数据、路径无冒号、R-T1、`x-reauth` 固定集合、分页、无内部 id）、对冻结标签的破坏性变更检测（oasdiff + operationId/`x-` 扩展 + 错误码索引，破坏性变更须变更说明与 ADR）、buf lint/breaking 与可再现的代码生成、迁移哈希清单；再做 CF-1 冻结前置复核（AC7/AC9），出具评审记录并打本地 annotated tag `contract-v1`。

## Boundaries & Constraints

**Always:** 外部工具版本固定且已在本机实际运行；契约检查的第三方依赖不进根 `go.mod`；每条检查有「正向通过 + 负向必败」自测；缺基线标签时检查失败而不是静默跳过；未验证项如实标「等待环境验证」。

**Never:** Docker/Compose/K8s 任何文件；push 任何内容（含标签）；修改 `git config`；改动 `SmartCDN`；为让门禁变绿而降低标准或隐藏发现；虚构验证结果。

</frozen-after-approval>

## Code Map

- `scripts/contract/`（独立 module，kin-openapi v0.153.0）
  - `openapi.go` -- 加载/校验 OpenAPI 3.1；元数据、无冒号路径、R-T1 泄漏扫描、`x-reauth` 固定集、list 分页、无内部整型 `id`
  - `breaking.go`、`cmd_openapi.go` -- 基线取自 git ref；调用固定版本 oasdiff；operationId/`x-permissions`/`x-tenant-scope`/`x-reauth` 比较；错误码索引比较；变更说明校验
  - `migrations.go`、`cmd_migrations.go` -- `migrations/CHECKSUMS` 校验/追加登记；`lingedge:class` 钩子；清单相对基线只追加
  - `freeze.go`、`cmd_freeze.go` -- AC7（a–e）与 AC9 冻结前置复核
  - `data/x-reauth.txt`、`data/oasdiff-severity-levels.txt` -- 固定集合与严重级别覆盖
  - `*_test.go` -- 40 个测试（含真实运行 oasdiff、buf、git、`proto-check-gen.sh` 的负向用例）
- `proto/buf.yaml`、`proto/buf.gen.yaml`、`scripts/proto-check-gen.sh` -- buf lint/breaking、可再现且可编译的代码生成
- `Makefile` -- `openapi-check`、`openapi-breaking`、`proto`、`proto-check`、`migrations-check`、`migrations-seal`、`freeze-check`、`contract-selftest`、`contract-check`
- `migrations/CHECKSUMS`、`docs/bmad/contract-changes/{_template.md,CF-1.md}`
- 文档：`docs/api/README.md`、`docs/database/{README,platform-identity}.md`（AA-8 回写）、`docs/architecture/dev-environment.md`、`README.md`、`.github/workflows/ci.yml`、`tests/scaffold/scaffold_test.go`

## Tasks & Acceptance

**Execution:**
- [x] `scripts/contract` 的 OpenAPI 校验与静态规则（AC1、AC8）
- [x] 破坏性变更检测：oasdiff + 扩展/错误码比较 + 变更说明放行（AC2）
- [x] `proto/buf.yaml`、`buf.gen.yaml`、`scripts/proto-check-gen.sh`、`make proto`/`proto-check`（AC3）
- [x] `migrations/CHECKSUMS` 与 `migrations-check`/`migrations-seal`（AC4）
- [x] `docs/bmad/contract-changes/_template.md`（AC6）、`CF-1.md`（AC5、AC7e）
- [x] `freeze-check`：AC7(a–e)（AC7）与 AC9（AA-8，方案二）
- [x] Makefile/CI/README/dev-environment/api README 同步；`tests/scaffold` 更新目标语义与接线断言
- [x] `docs/bmad/sprint-status.yaml` 回写
- [x] 提交（本地）与 annotated tag `contract-v1`（AC5、AC10）

**Acceptance Criteria:** 见 `docs/bmad/epics.md` Story 1.3 的 AC1–AC10（原文为准）。

## Implementation Notes

见文末「开发记录」。

## Spec Change Log

## Review Triage Log

（见「开发记录 · 独立评审」。）

## Verification

**Commands:**
- `make contract-check`（= `contract-selftest openapi-check proto-check migrations-check freeze-check`）
- `make lint`、`make security`、`make gate-selftest`、`make test`、`make race`、`make build && make verify-build`

## 开发记录

**开发会话**：2026-11-17，bmad-build（非交互、完全自主）。执行环境：macOS（Darwin 25.6.0 arm64）、go1.27.1。

### 决策与偏差

- **工具与版本（均已在本机实测，非虚构）**：buf **v1.75.0**（`go run github.com/bufbuild/buf/cmd/buf@v1.75.0 --version` → `1.75.0`；lint/build/breaking/generate 均实跑）；oasdiff **v1.38.0**（`go run github.com/oasdiff/oasdiff@v1.38.0`；其 `--version` 经 `go run` 只显示 `main`，版本以 `@v1.38.0` 为准；实测支持 OpenAPI 3.1，JSON 输出）；kin-openapi **v0.153.0**（独立 module `scripts/contract`，加载并 `Validate` 了 307 路径的 3.1 契约）；protoc-gen-go **v1.36.12**、protoc-gen-go-grpc **v1.6.2**（临时 module 内与 protobuf v1.36.12、grpc v1.85.0 真实编译、vet 通过）。根 `go.mod`/`go.sum` 未改动。
- **契约检查程序为独立 module**（`scripts/contract/go.mod`）：kin-openapi 的依赖不进根 `go.mod`（沿用 1.2「根模块无第三方依赖」）；Makefile 先编译到 `bin/contract` 再从仓库根运行；自测由 `make contract-selftest` 运行（根 `go test ./...` 不含它）。
- **破坏性变更检测 = oasdiff + 三项补充**：① oasdiff `breaking`，level>=warn 即破坏；严重级别覆盖两处（响应枚举新增值 error→info，与 README §3/AC9 一致；响应可选字段删除 info→err，满足 AC2「删除字段」）；② operationId 删除/改名、路径模板/方法变更（仅改路径参数名不算）、`x-permissions` 新增必需项、`x-tenant-scope` 改变、`x-reauth` 增删、`x-audit` true→false、`x-phase` 变更；③ `docs/api/README.md` §5 错误码索引的 code→HTTP（删除/改名/改状态）。放行方式：`CONTRACT_NOTE=docs/bmad/contract-changes/<说明>.md`，说明必须位于该目录（非模板/CF-1）、基线中不存在、声明「是否破坏：是」+ 关联 `ADR-NNN` + 影响范围、无占位符，且**逐项写出本次检出的全部变更类型 ID**。门禁策略文件（oasdiff 严重级别、`proto/buf.yaml`）相对基线被改动同样判为破坏。基线缺失（标签不存在、README/OpenAPI 在基线中不存在、基线错误码解析为 0）一律失败，不静默跳过。
- **AA-8**：选方案二（内部周期作业不入 `TaskType`），回写 `docs/api/README.md`、`docs/database/README.md`（2 处）、`platform-identity.md`，并由 `freeze-check` 机械守护；理由见 CF-1.md §3。
- **proto**：`proto/buf.yaml`（STANDARD lint + FILE breaking）；3 类命名偏差登记为 `ignore_only` 例外（改名会破坏已冻结的线上名），见 CF-1.md §4。
- **生成的 Go 代码暂不入库（重要发现）**：入库需要根 `go.mod` 引入 grpc/protobuf；实测引入后 `make vuln`（govulncheck v1.9.0）对 go1.27.1 标准库报告 net/http 的已公布漏洞（GO-2026-6859 等，修复于 go1.27.2–go1.27.4）并以非零退出；纯净 HEAD 无此问题。为不让既有门禁变红且不隐藏发现，`make proto` 输出到 gitignore 的 `dist/proto-gen/`（附 `go.mod` 使其不被根模块 `./...` 扫描），`make proto-check` 以「两次生成逐字节一致 + 临时 module 真实编译 + vet」证明可再现、可编译。**建议另行评估升级 toolchain（影响 Story 1.12 起的 HTTP 服务）**；本 Story 未改动 Story 1.1 固定的 `go1.27.1`。
- **`x-reauth` 固定集合为 11 项**（修订日志写「10 个」，以契约实测为准）。
- **迁移清单**：`migrations/CHECKSUMS` 暂为空（真实迁移在 1.7–1.9，CF-2 登记）；`lingedge:class` 仅检查标注存在；`.gitattributes` 强制 `migrations/**` 为 LF，避免 autocrlf 令哈希误报。
- **未做**：路由注册层与契约一致性（1.12）、前端类型生成（1.36）、GitHub Actions 远端运行（无运行记录；CI 的 `make contract-check` 需要推送 `contract-v1` 标签后才可能通过）。

### 实际验证（本机 macOS arm64、go1.27.1）

- `make contract-selftest`：`go vet` + 40 个测试全部通过（约 35 秒），其中**真实运行**：oasdiff（十余类变异矩阵：删除/改类型/收紧/新增必填/删 operation/删枚举值/删请求字段/删成功响应均判破坏，向后兼容新增与枚举新增均放行；工具失败不得静默通过）、buf（枚举值删除/改号、字段编号变更、消息删除均 `breaking` 失败，新增字段与新增消息通过，违规命名 `lint` 失败）、git（临时仓库 + annotated tag：与标签一致通过、operationId 改名/必填放宽/错误码删除/策略文件改动必败、有效说明放行、目录外/基线已有/漏列 ID 的说明不放行、标签不存在必败）、`scripts/proto-check-gen.sh`（真实通过；非确定性生成、不可编译生成物、`buf generate` 失败均必败）。
- 另有静态规则自测：缺 `operationId`/四个扩展逐项必败；3.0 与悬空 `$ref` 被校验器拒绝；含 `:` 路径；R-T1（`$ref` 递归、环、聚合计数允许、路径级与参数 `content`、`prefixItems`/`if-then`/`patternProperties`/`contains`/`unevaluated*`、驼峰、`cluster_region`）；`x-reauth` 集合两个方向；list 分页（页码/游标、顶层数组、`data` 集合、带 charset 的媒体类型）；内部整型 id（`type: string, format: int64`、`*_id`、`additionalProperties`/`prefixItems` 内、整型 id 路径参数）；迁移哈希（改动/删除/追加空白/子目录/大写扩展名/清单篡改/清单改写/格式错误）；冻结复核（迁移顺序与骨架互换/改名/错表/漏行、5 项待环境验证登记被抹、AA-8 污染、proto 符号被注释满足）。
- `make openapi-check`：OpenAPI 3.1.0、307 路径、486 operation、`x-reauth` 11、`M1=200 P2=128 P3=63 P4=38 P5=56 P6=1`，静态规则 0 违规；`openapi-breaking` 相对基线无破坏。`make proto-check`：buf lint 通过、`buf breaking` 通过、生成可再现（两次一致，6 个 .proto 均有生成物，5 个含 service 的另有 gRPC 桩）、临时 module 编译 + vet 通过。`make migrations-check`、`make freeze-check` 通过。（打标签之前以 `CONTRACT_BASE=HEAD` 运行；打标签后以默认 `contract-v1` 复跑，见下。）
- **AC7(c) 额外实测**：独立临时 mysqld 8.0.46（独立 datadir/端口/socket，`--log-bin` 且 `log_bin_trust_function_creators=0`，用后关闭并删除，未触碰本机常驻 MySQL）上重放 `m1-fk-skeleton.sql`：**0 错误、67 表**；`run_ddl_rules.py`：**26 个用例失败 0**。
- 既有门禁（改动后复跑）：`make lint`（vet×2、gofmt、scaffold、arch、golangci-lint `0 issues`、shell）、`make security`（4 项）、`make gate-selftest`、`make test`、`make race`、`make build && make verify-build`（15 个产物 ok）、`make vuln`（无受影响漏洞，exit 0）全部通过。

### 独立评审

一名无上下文的评审子代理对 `scripts/contract`、Makefile、`proto-check-gen.sh` 做了只读对抗评审（13 条，其中高 2、中 6、低 5），处置如下：

| 发现 | 判定 | 处置 |
|---|---|---|
| 取消 `x-reauth` 不判破坏（IMP-12 形同虚设） | high | patch：`reauth-removed` 判破坏；另加 `audit-removed`（x-audit true→false）、`phase-changed`；单测 |
| R-T1 漏检：路径级参数、参数 `content`、3.1 关键字、`cluster_*` 与文档不一致、驼峰 | high | patch：统一 `children()` 枚举全部子 schema 关键字；合并路径级参数并扫描 `content`；规则改为文档口径 `cluster_.*` 并拦驼峰；真实契约仍 0 违规；单测 |
| 变更说明不绑定本次变更、可复用旧说明、路径不受限 | medium | patch：限定 `docs/bmad/contract-changes/`、排除模板/CF-1、基线已存在的说明不得复用、须逐项列出本次检出的变更类型 ID；「是（…）」不再误拒 |
| 策略文件未与基线绑定 | medium | patch：oasdiff 严重级别与 `proto/buf.yaml` 相对基线改动即判破坏（`policy-file-changed`） |
| 错误码比对在基线 README 缺失/解析为 0 时静默跳过 | medium | patch：改为失败；`-base-file` 模式必须配 `-base-readme-file` |
| 内部 id 检查过窄；list 识别过窄 | medium | patch：`id`/`*_id` 整型或 int32/int64 格式、递归全部 3.1 关键字、整型 id 参数；list 含顶层数组、`data/records/results`、charset 媒体类型、遍历全部 2xx |
| AC7b 符号检查可被注释满足 | medium | patch：剥离注释并按定义形态匹配（`message X {`、`rpc X(`、`<type> field = N`） |
| `operation-moved` 因路径参数改名误报 | low | patch：路径模板参数名归一 |
| 迁移：子目录不递归、扩展名大小写、无 `.gitattributes` | low | patch：递归、`strings.ToLower`、新增 `.gitattributes`；基线无清单时跳过追加比对属设计（首次冻结，已文档化） |
| `_ = extra` 空转 | low | patch：删除；总数由 M1=61 表断言覆盖，另在 CF-1 记录 67 表实测 |
| `CONTRACT_BASE`/`BUF`/`OASDIFF` 可被 make 命令行覆盖而绕过 | low | defer：属 CI 配置（工作流固定使用默认值，未传覆盖）；分支保护与必选检查属托管侧 |
| 未登记（未发布）迁移可任意改删 | low | 驳回：清单即「已发布」的定义；未发布迁移允许改，发布动作是 `make migrations-seal` |

### AC 对照

| AC | 结果 |
|---|---|
| 1 `make openapi-check`：3.1 校验 + 元数据 + 路径无冒号 + R-T1 + `x-reauth` 固定集 | 通过（40 个自测含负向；kin-openapi v0.153.0） |
| 2 相对 `contract-v1` 的破坏性变更检测；无 ADR/说明失败，向后兼容新增通过 | 通过（oasdiff v1.38.0 + 补充规则；端到端 git 标签测试） |
| 3 `make proto-check`：buf lint + breaking；破坏已发布 `.proto` 失败 | 通过（buf v1.75.0；负向均必败）；生成代码可再现且可编译 |
| 4 `migrations/CHECKSUMS`：改动已发布迁移即失败；新增迁移 `lingedge:class` 钩子 | 通过（清单目前为空，工具与负向自测就绪；class 细则随 1.6） |
| 5 评审记录 `CF-1.md` + 标签 `contract-v1` | 通过：`docs/bmad/contract-changes/CF-1.md`（UD-25：由开发代理依据机器复核出具，未经人工签收，用户可事后推翻）；标签见下 |
| 6 变更模板 `_template.md` | 通过 |
| 7 冻结前置复核 (a)(b)(c)(d)(e) | 通过（`make freeze-check` + 独立 mysqld 重放骨架）；(d) 的 5 项仍**待环境验证**，不阻塞冻结 |
| 8 list 分页参数；对外 schema 无内部 int64 id | 通过（含负向自测） |
| 9 AA-8 `TaskType` 对齐 | 通过（方案二，`freeze-check` 守护） |
| 10 `contract-v1` 为指向通过复核的提交的 annotated tag，须在 1.1 首个提交之后 | 见下「提交与标签」 |

### DoD（基线 §25）

需求明确、代码完成、编译通过、单元/集成（含真实运行 buf/oasdiff/git/MySQL 骨架重放）测试通过、关键异常（负向）测试通过、文档更新、运行验证（本机）：满足。数据库迁移、API、前端功能：本 Story 不适用。**等待环境验证**：GitHub Actions 工作流在远端的实际运行（环境缺口：远端运行记录；且需先推送 `contract-v1` 标签）；修订日志 §4 的 5 项（与本 Story 完成无关，仍待 Linux/OpenResty/MySQL 8.0.16）。

### 提交与标签

- 本地提交 `ee6445a832a8a4047bcde78da333c83de2244892`（`feat: Story 1.3 契约工具链与冻结点 CF-1…`，父提交 `1ef0f8c`）；`git add` 逐路径，未含 `_bmad/`、`dist/`、`bin/`、二进制；使用 `git -c user.name=… -c user.email=…`，未改 `git config`；**未 push 任何内容**。
- `git tag -a contract-v1 ee6445a`（annotated，本地）：`git cat-file -t contract-v1` → `tag`，`git rev-parse 'contract-v1^{commit}'` → `ee6445a832a8a4047bcde78da333c83de2244892`。打标签前核对：AA-6 关闭依据 `architecture-revision-log.md` 与契约一致（`freeze-check` 的 AC7(a)(b)(c)(d)(e) + AC9 全部成立）、1.1 首个提交 `1ef0f8c` 已形成。标签指向的提交即复核通过的提交；之后仅追加本节记录的文档提交，不改契约文件。
- 打标签后以默认基线复跑 `make contract-check`（`CONTRACT_BASE=contract-v1`）：自测 40 个通过；`openapi-check`、`openapi-breaking`（相对 `contract-v1` 无破坏）、`proto-check`（`buf breaking` 相对标签通过）、`migrations-check`、`freeze-check` 全部通过。
- 标签**尚未推送**：推送时须 `git push origin main contract-v1`（由用户决定）。

### 遗留与后续

- 推送远端时须**同时推送 `contract-v1` 标签**，CI 的 `make contract-check` 才有基线；未推送前远端 CI 会因缺基线失败（这是设计：缺基线不得静默通过）。
- `proto` 生成物入库的时机：toolchain 升级（≥go1.27.4 需另行评估）与首个 gRPC 消费者（Story 3.x）。
- 1.6 落地迁移分类 lint 后，收紧 `lingedge:class` 钩子；1.9（CF-2）起把基线标签改为新标签并登记 `migrations/CHECKSUMS`。
- Story 1.1/1.2 在 sprint-status 仍为 `review`，本 Story 未改动其状态。
