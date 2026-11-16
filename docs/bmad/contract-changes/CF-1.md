# CF-1 契约冻结评审记录（`contract-v1`）

- **日期**：2026-11-17
- **对应 Story**：1.3「契约工具链与契约冻结点 CF-1」（`docs/bmad/stories/1-3-契约工具链与契约冻结点-cf-1-openapi-proto-migrations.md`）
- **签收方式**：UD-25（`docs/bmad/decisions-adopted.md`）——「冻结前复核全绿（1.3 AC7）+ 架构评审代理出具评审记录」。**本记录由执行 1.3 的开发代理（bmad-build）依据机器复核结果出具，未经人工签收**；用户可事后推翻（推翻则 `contract-v1` 作废，走 ADR-017 §10 变更说明，并行泳道按新契约返工）。
- **前置检查**：AA-6 已由架构修订关闭，依据 [`docs/bmad/architecture-revision-log.md`](../architecture-revision-log.md)（两份评审 67 项：已修 66、延后 1〔MIN-09，UD-28〕；BLK-01…07 全部关闭）。本次**复核而非重做修订**。
- **结论**：**通过**。全部复核项成立，允许打标签 `contract-v1`（annotated tag，仅本地创建；标签指向的提交以 `git rev-parse contract-v1^{commit}` 为准——本记录随该提交入库，故不在文内写死哈希）。

## 1. 冻结对象

| 契约 | 文件 | 规模（实测） |
|---|---|---|
| REST（OpenAPI 3.1） | `docs/api/openapi.yaml`、`docs/api/README.md`（权限串/错误码索引） | 307 路径、486 operation；`x-phase`：M1=200、P2=128、P3=63、P4=38、P5=56、P6=1 |
| Node 协议（gRPC/Protobuf） | `proto/lingedge/node/v1/*.proto`（6 个）、`proto/buf.yaml`、`proto/buf.gen.yaml` | 5 个 Service（`NodeControlService`、`RevisionService`、`TaskService`、`TelemetryService`、`EnrollService`） |
| ERD / 迁移结构设计 | `docs/database/*.md`、`docs/database/verification/m1-fk-skeleton.sql` | 13 个迁移文件、61 张 M1 表（骨架另含 6 张后续阶段样例，共 67 表） |
| 迁移哈希清单 | `migrations/CHECKSUMS` | 当前 0 条（真实迁移 000001–000013 由 Story 1.7–1.9 创建，CF-2 登记） |

> 冻结的是**契约**，实现可迭代；M1 之外阶段的 operation 在其阶段开工前补全并重新冻结（`contract-vN`）。

## 2. 复核项（Story 1.3 AC7）与实测结果

| 项 | 要求 | 结果 | 证据（命令 / 实测） |
|---|---|---|---|
| (a) | 校验器通过；M1 operation = 200；路径含冒号 0；R-T1 泄漏扫描 0 | **通过** | `make openapi-check`：kin-openapi v0.153.0 校验 OpenAPI 3.1.0 通过；`make freeze-check`：M1=200、冒号 0、R-T1 0 处；另对 AC1/AC8 全部静态规则 0 违规 |
| (b) | 6 个 `.proto` 经 protoc/buf 编译与 lint 通过，含 `NodeRollback`、`held_seq`、`GetCaBundle`、`lineage` | **通过** | `make proto-check`：buf v1.75.0 `lint` 通过、`buf breaking` 对基线通过、生成两次逐字节一致，并在临时 module（protobuf v1.36.12、grpc v1.85.0）真实编译 + `go vet` 通过；`freeze-check` 断言四个符号均存在 |
| (c) | `docs/database/README.md` §8.1 迁移顺序与 `verification/m1-fk-skeleton.sql` 一致；骨架空库 0 错误、67 表；`run_ddl_rules.py` 26/26 | **通过** | `freeze-check` 机器比对 §8.1 的 13 行与骨架 12 个分节（000004 为纯种子无 DDL）的顺序、命名与每个迁移所含表（61 张 M1 表）；**本次另在独立临时 mysqld 8.0.46 上重跑**：骨架 0 错误、67 表，`run_ddl_rules.py` 26 个用例失败 0（临时实例用后已关闭删除，未触碰本机常驻 MySQL） |
| (d) | 修订日志 §4 的 5 项「待环境验证」已登记到对应 Story 的『等待环境验证条件』 | **通过** | `freeze-check` 逐项核对 epics.md：①MySQL 8.0.16 → Story 1.6/1.9；②PoC-5 → 3.24；③PoC-3 → 3.23；④Agent 单元内 `openresty -t` → 1.32/3.22；⑤PoC-1 → 3.23。五项仍**未验证**（需 Linux/OpenResty/MySQL 8.0.16），不阻塞冻结 |
| (e) | 评审记录引用修订日志 | **通过** | 本文件（`freeze-check` 断言其引用 `architecture-revision-log.md`） |
| AC9（AA-8） | `TaskType` 与 `partition.maintain` 对齐 | **通过** | 方案二，见 §3 |

## 3. AA-8 决策：`TaskType` 口径

- **决策**：采用 AC9 的方案二——**不改 OpenAPI**，`partition.maintain` 及同类作业改称「**内部周期作业**」并列出清单：`tasks.reap`、`idempotency.cleanup`、`sessions.cleanup`、`partition.maintain`。它们由 Scheduler 注册，**不属于 OpenAPI `TaskType`**、不对外暴露、不可经 API 入队。
- **理由**：① `TaskType`（25 项）是**对外**白名单枚举（FR-35），内部作业无需让前端/API 客户端感知；② 采用方案一（补枚举）会扩大对外契约面且无消费者；③ 方案二对已冻结契约零改动，epics 的 1.14/1.15/1.21 已按此口径书写（「内部周期作业不入 TaskType」）。
- **回写**：`docs/api/README.md` §2 增 TaskType 事实源与内部周期作业清单；`docs/database/README.md` §5.1（两处）、`docs/database/platform-identity.md` 的 `partition.maintain` 行标注「内部周期作业，非 TaskType」。
- **机械守护**：`freeze-check` 断言 `TaskType` 恰为 25 项且与清单**不相交**，并断言 `docs/{database,api,architecture}` 中凡提及清单内作业名的行都带有「内部周期作业」标注。1.14 的「注册表 ⊆ TaskType」一致性 CI 据此以 `TaskType` 为界。

## 4. 本次冻结登记的偏差与取舍（评审须知）

1. **buf lint 例外（3 类，登记在 `proto/buf.yaml`）**：`OPENRESTY_STATE_*` 枚举值前缀（OpenResty 为产品专名，按 buf 规则应为 `OPEN_RESTY_STATE_`）、`TelemetryService.UploadAccessLogs` 的请求/响应消息名（`LogBatch`/`UploadLogsSummary`，非 `UploadAccessLogsRequest/Response`）。改名会破坏已写入文档与实现约定的线上名，故登记为例外而不改 `.proto`；新增 `.proto` 不得扩大该名单。
2. **oasdiff 严重级别两处覆盖**（`scripts/contract/data/oasdiff-severity-levels.txt`）：「响应枚举新增值」由 error 降为 info（与 README §3「新增枚举值允许」及 AC9 的非破坏 expand 一致，客户端须容忍未知枚举值）；「删除响应可选字段」由 info 升为 error（AC2「删除/改名字段」为破坏）。
3. **`x-reauth` 固定集合为 11 项**（`scripts/contract/data/x-reauth.txt`：`createRole`、`createUser`、`deleteRole`、`deleteUser`、`putRunMode`、`putSsrfAllowlist`、`resetUserPassword`、`revokeNodeCertificate`、`setRolePermissions`、`setUserRoles`、`updateRole`）。修订日志 IMP-12 行写作「10 个」，**以契约实测为准**；该处计数为文档笔误，不改契约。
4. **错误码纳入破坏检测**：OpenAPI 本身不枚举错误码，故以 `docs/api/README.md` §5「错误码索引」的 code→HTTP 状态为错误码契约，删除/改名/改 HTTP 状态判为破坏。
5. **迁移清单尚空**：真实迁移在 1.7–1.9 创建；`migrations/CHECKSUMS` 工具链已就绪并有负向自测，CF-2（1.9）起登记。`lingedge:class` 目前只做「标注存在」检查，取值与语法细则随 1.6。
6. **生成的 Go 代码暂不入库**：入库需要根 `go.mod` 引入 grpc/protobuf，而 grpc 把 `net/http` 引入可达调用图后，`govulncheck`（v1.9.0）对 go1.27.1 标准库报告 `net/http` 的已公布漏洞（GO-2026-6859 等，修复于 go1.27.2–go1.27.4），`make vuln` 会由绿变红。故 `make proto` 输出到 gitignore 的 `dist/proto-gen/`，`make proto-check` 以「生成两次逐字节一致 + 临时 module 真实编译」证明可再现与可编译；入库随 toolchain 升级与首个 gRPC 消费者（Story 3.x）落地。**此漏洞对任何 net/http 使用者（含 Story 1.12 的 HTTP 服务）同样成立，建议另行评估升级 toolchain。**
7. **工具链为本机实测**：buf v1.75.0、oasdiff v1.38.0、kin-openapi v0.153.0、protoc-gen-go v1.36.12、protoc-gen-go-grpc v1.6.2，均在本机（macOS arm64、go1.27.1）实际运行通过；GitHub Actions 上的运行**未验证**（尚无远端运行记录，且 `contract-v1` 标签需随推送一并推到远端，否则 CI 的 `make contract-check` 因缺基线标签而失败）。

## 5. 工具链自检与独立评审

工具链自测 40 个（含真实运行 oasdiff、buf、git、MySQL 骨架重放）全部通过；另经一名无上下文评审子代理的只读对抗评审（13 条，高 2、中 6、低 5），高/中全部修复（如：取消 `x-reauth` 判为破坏、R-T1 扫描补全 3.1 关键字与路径级参数、变更说明与检出变更绑定、策略文件与基线绑定、错误码基线缺失即失败），处置明细见 Story 1.3 开发记录。该评审针对**工具链**而非契约内容本身；契约内容的复核依据是 §2 的机器检查与修订日志。

## 6. 未覆盖 / 仍开放

- 修订日志 §4 的 5 项待环境验证（见 §2(d)）。
- 缺少独立人工评审（UD-25 默认接受）。
- 路由注册层与 OpenAPI 的一致性检查（NFR-4/22 的「每条已注册路由在契约中存在」）依赖路由注册代码，属 Story 1.12。
- 前端 API 类型生成（ADR-014）属 Story 1.36。
