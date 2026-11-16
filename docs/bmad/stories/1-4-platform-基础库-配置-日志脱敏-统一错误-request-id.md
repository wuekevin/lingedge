---
title: 'Story 1.4：platform 基础库（配置 / 日志脱敏 / 统一错误 / Request ID）'
type: 'feature'
created: '2026-11-17'
status: 'in-review'
route: 'dispatch'
review_loop_iteration: 0
baseline_commit: '82af1b3944f7cdbd436a67de4ea78089797a7f7a'
context:
  - docs/bmad/epics.md（Story 1.4 全文，约 893–913 行）
  - docs/bmad/decisions-adopted.md（UD-27、UD-29、AA-7）
  - docs/architecture/adr/ADR-011-secrets-and-kms.md（§7 日志与错误脱敏）
  - docs/architecture/adr/ADR-017-api-conventions.md（§3 统一错误体、§6 Request ID）
  - docs/architecture/adr/ADR-025-redis-boundary.md（键空间 <env>）
  - docs/architecture/architecture.md（§5.2 目录、§9.1 标识/时间、§9.2 API 约定）
  - docs/api/openapi.yaml（Error / ErrorDetail / ErrorResponse、XRequestID）、docs/api/README.md（§5 错误码索引）
  - docs/bmad/stories/1-1-go-monorepo-脚手架与目录骨架.md、1-2-…、1-3-…
---

<frozen-after-approval reason="human-owned intent — do not modify unless human renegotiates">

## Intent

**Problem:** Server 内所有模块在写第一行业务代码之前，缺少统一的配置加载、会被机器验证的日志脱敏、稳定的错误码与错误体、可贯穿日志/审计/Task/gRPC 的 Request ID，以及可注入的时间与 ULID 来源；各 Story 若各自发明，行为必然漂移，且日志泄漏凭据（NFR-12、G-5）无法被统一拦截。

**Approach:** 在依赖图最底层落地七个小包，全部有表驱动单测与「泄漏即失败」断言：`pkg/redact`（仅标准库的脱敏引擎，供 Server/Tools/Node 共用）、`internal/platform/{config,logx,apierr,requestid,clock,uid}`；新增 lint 规则禁止 `internal/` 业务代码直接读 `time.Now`；交付不含任何默认口令的 `configs/server.example.yaml`。

## Boundaries & Constraints

**Always:** 新增第三方依赖版本固定且 `make vuln` 无漏洞；脱敏对「键名 / 值格式 / 错误链 / panic 值 / 嵌套结构」逐类有断言，且对良性内容有「不得误伤」断言；错误体与 `docs/api/openapi.yaml` 的 `Error`/`ErrorDetail`、`docs/api/README.md` §5 错误码索引逐字对齐（有机器核对测试）；内部错误（500）响应不得携带原因文本；不虚构数值默认值（UD-27：待基线的参数不进 schema）。

**Never:** 改动已冻结契约（OpenAPI/proto/迁移）；Docker；Mock 冒充生产（时钟/随机源只经接口注入，测试替身只在 `_test.go`）；浮点金额；push；改 `git config`；不得改动 `SmartCDN`（隔离，NFR-29）；在生产代码路径中加入「检测到测试环境」的分支。

</frozen-after-approval>

## Code Map

- `pkg/redact/` -- 脱敏引擎（仅标准库）：键名判定、自由文本按值格式清洗（`le_<前缀>_…`、`lenr1.…`、PEM 私钥块、DSN 口令、Bearer、`k=v`/JSON 片段、Cookie）、嵌套结构、错误链包装、panic 值。
- `internal/platform/logx/` -- Zap 集成：`Wrap(core)` 过滤消息/栈/全部字段类型（含 error、Stringer、Reflect、ObjectMarshaler、ArrayMarshaler、Namespace 之后的键）；`New(Options)`（JSON、UTC 毫秒时间戳、可注入时钟）；`Redact(key)`；`PanicFields(r)`；`WithContext(ctx, l)` 附 `request_id`。
- `internal/platform/apierr/` -- 错误模型：`Code` 常量与 HTTP 状态映射表（与 README §5 机器核对）、`Error{Code,Message,MessageKey,Details}`、`ToBody`（任何 error → 状态码 + `{"error":{…}}`，500 不泄漏原因）、`Wrap`。
- `internal/platform/requestid/` -- 入口读取/生成 ULID（非法入站值丢弃重生成）、context 存取、响应头/gRPC `x-request-id` 元数据帮助函数（入站读取、出站追加、两个一元拦截器）。
- `internal/platform/clock/` -- `Clock` 接口、`System`（UTC、毫秒截断，仅此包可调用 `time.Now`）、`Func` 适配器。
- `internal/platform/uid/` -- ULID 生成器（单调、时钟回拨保护、可注入时钟与熵）、`Valid`/`Normalize`/`Time`。
- `internal/platform/config/` -- YAML 加载：未知键拒绝、环境变量覆盖（`LINGEDGE_<节>__<键>`，未知覆盖拒绝）、`password_file`/`*_file` 密钥通道、`Secret` 类型（任何格式化/序列化均为 `***`）、schema 校验（一次报出全部问题且错误文本不含任何值）。
- `configs/server.example.yaml` -- 示例配置（无口令/密钥，有测试断言）。
- `tests/fixtures/redaction/corpus.json` -- 脱敏回归语料（标注「仅测试」，G-6 唯一豁免位置）。
- `scripts/gates/genlint.go`、`.golangci.yml`、`tests/gates/gates_selftest_test.go` -- `forbidigo` 禁 `time.Now/Since/Until`（仅 `internal/` 且 `internal/platform/clock` 豁免）及其正反自测。
- 复用：`.golangci.yml` 的 depguard（platform 不得依赖任何业务模块；`pkg` 不得依赖 `internal`）、1.2 的口令/测试替身/品牌门禁。不改动：`docs/api/openapi.yaml`、`proto/`、`migrations/`。

## Tasks & Acceptance

**Execution:**
- [x] `pkg/redact` 引擎 + 表驱动/语料/随机口令测试（AC2）
- [x] `internal/platform/logx`（AC2、AC4）
- [x] `internal/platform/apierr`（AC3）
- [x] `internal/platform/requestid`、`clock`、`uid`（AC4、AC5）
- [x] `internal/platform/config` + `configs/server.example.yaml`（AC1）
- [x] `forbidigo` 禁直接读时钟的 lint 规则、`make gen-lint`、门禁自测（AC5）
- [x] 文档同步（dev-environment 依赖表、README 目录说明）、`sprint-status.yaml`
- [x] 全部门禁 + 独立评审 + 本地提交

**Acceptance Criteria:** 见 `docs/bmad/epics.md` Story 1.4 的 AC1–AC5（原文为准）。

## Implementation Notes

见文末「开发记录」。

## Spec Change Log

## Review Triage Log

（见「开发记录 · 三路评审」。）

## Verification

**Commands:**
- `make lint`、`make security`、`make gate-selftest`、`make test`、`make race`、`make vuln`、`make contract-check`、`make build && make verify-build`

## 开发记录

**开发会话**：2026-11-17，bmad-build（非交互、完全自主）。环境：macOS（Darwin 25.6.0 arm64）、go1.27.4。

### 范围决策与偏差

- **范围以 AC1–AC5 为准**：配置、日志脱敏、统一错误（含 HTTP 映射）、Request ID、clock/ULID。**分页助手属于 Story 1.12（AC4）、`pkg/money` 属于 Story 6.1**，本 Story 不提前实现（避免预占别的 Story 的契约）。
- **脱敏引擎放在 `pkg/redact`（仅标准库）**，Zap 接入放在 `internal/platform/logx`：原因是 NFR-12 同样约束 Tools 与 Node（二者不得 import `internal/platform`），引擎共用可避免日后重复实现；`pkg` 不得依赖 `internal`，方向仍满足 1.2 门禁。
- **新增第三方依赖（均版本固定于 go.mod）**：`go.uber.org/zap v1.28.0`（基线固定）、`github.com/oklog/ulid/v2 v2.1.2`、`go.yaml.in/yaml/v3 v3.0.6`（YAML 官方组织维护的 yaml.v3 延续版，gopkg.in/yaml.v3 已停更；`KnownFields(true)` 拒绝未知键）、`google.golang.org/grpc v1.85.0`（仅 `metadata`/拦截器与测试用 `health` 服务）。`govulncheck` 初次对间接依赖 `golang.org/x/net v0.60.0` 报 2 条「模块含漏洞但代码未调用」（GO-2026-6860/6866），已升级到 v0.62.0（连带 x/sys v0.49.0、x/text v0.43.0），`make vuln` 现为「No vulnerabilities found」。
- **配置 schema 的取舍**：只登记架构文档已明确的键（server.env/data_dir、http/grpc/metrics 监听、database、redis、log、release）；`auth.reauth_window_s`、`placement.*`、`grpc.tls.san_extra` 属 `system_settings`，不进 server.yaml；待基线的参数不进 schema（UD-27）；`server.env`、数据库/Redis 连接为**运行期必填**（setup mode 可缺，`Options.RequireRuntime`）；**口令不得写在 YAML**（写了即拒绝），只能经 `LINGEDGE_<节>__<键>` 环境变量或 `*.password_file`（口令文件不得被其他用户访问、同组不得写入，0440 的 root:lingedge 为 AA-7 建议形态）。AA-7 的最终落点仍由架构补充，本 Story 不预判。
- **lint 规则「禁止直接读系统时钟」**：`forbidigo` 增 `^time\.(Now|Since|Until)$`（含函数值别名），由 `scripts/gates/genlint.go` 生成、`make gen-lint` 重新生成 `.golangci.yml`；范围限定为 Server 业务代码（`internal/<spec.go 登记的 Server 模块>/`），`internal/platform/clock` 豁免；Tools（不能依赖 platform）、cmd、pkg、scripts、tests 不在范围。`time.After/Sleep/NewTimer` 暂未禁止（见遗留）。
- **测试口令纪律（G-6）**：1.2 的固定口令扫描会把源码里的「口令类键名 + 字面量」与 DSN 字面量判为违规，且唯一豁免是 `tests/fixtures/` 内标注「仅测试」的文件。因此回归语料放在 `tests/fixtures/redaction/corpus.json`，Go 测试中的口令类键名/DSN 一律运行时拼接或用 `%s` 模板；未放宽任何门禁。

### 交付物

`pkg/redact`、`internal/platform/{config,logx,apierr,requestid,clock,uid}`、`configs/server.example.yaml`、`tests/fixtures/redaction/corpus.json`、lint 规则与其正反自测（`tests/gates`）、`README.md`/`dev-environment.md`/`internal/platform/doc.go` 同步。统一错误：状态映射表 52 个码与 `docs/api/README.md` §5 双向机器核对（`codes_test.go`），响应体字段集合与 `openapi.yaml` 的 `Error`/`ErrorDetail`/`ErrorResponse` 机器核对。

### 实际验证（本机 macOS arm64、go1.27.4）

- `make lint`：vet×2、gofmt、scaffold、arch、golangci-lint `0 issues`、shell 通过。`make security`（G-6/G-11/G-12/品牌）通过。
- `make gate-selftest`：通过；新增时钟规则正向/负向用例——`time.Now`、`time.Since/Until`、`var Now = time.Now` 别名、`internal/platform` 其他子包的直接读时钟均被拦截（诊断含 `[no-direct-clock]`），时钟包本身、`_test.go`、Tools、cmd、pkg 与注入式写法不被误伤。
- `make test`、`make race`（`CGO_ENABLED=1 go test -race ./...`）全部通过；其中 gRPC 用例使用真实 gRPC 服务端（回环 TCP + grpc 自带 health 服务，覆盖一元与流式、非法元数据值、生成器失败 fail-closed）。
- `make vuln`：No vulnerabilities found。`make contract-check`：契约自测 + openapi-check/breaking + proto-check + migrations-check + freeze-check 全部通过（未改动契约）。`make build && make verify-build`：15 个产物 ok。
- 突变验证：临时去掉 `logx` 对 error 字段的清洗，语料测试立即失败；临时去掉 `Stack` 清洗，对应测试失败，已还原。

### 三路评审（Blind Hunter / Edge Case Hunter / Verification Gap，均为无上下文子代理）

处置汇总（均已修复并有回归测试，除注明 defer/驳回者）：

| 发现 | 判定 | 处置 |
|---|---|---|
| `PanicFields` 的 `stacktrace` 与 zap 自带栈键重复 | medium | patch：改为 `panic_stack`；测试按原始行断言无重复键 |
| 结构体里的 `[]byte`（JSON 后为 base64）绕过值格式清洗 | high | patch：`redact.Value` 对 base64 字符串解码后复检，命中整体遮蔽；`[]byte` 直接清洗；corpus/单测 |
| 敏感键名漏项（master_key、session_id、sid、csrf、otp、hmac、signature…） | medium | patch：补子串词与「整段相等」短词（避免 side/footprint 误伤）；语料含良性对照 |
| `Chain` 对自引用 Unwrap 无深度限制（栈溢出致命）、Unwrap 自身 panic | high | patch：深度上限 32 + `recover`；测试 |
| MySQL DSN 省略协议括号（`user:pw@/db`）、URL 用户信息放令牌、命令行 `--flag value`、JSON 套 JSON 的转义引号 | medium | patch：新增规则与语料 |
| `reflectField`：MarshalJSON panic 外泄、大整数经 float64 丢精度 | medium | patch：recover + `UseNumber`；测试 |
| 请求 ID 中间件生成失败返回纯文本 500 | medium | patch：改走统一错误体；测试 |
| 流式拦截器生成器失败分支无测试；`Detail.Code`/`message_key` 清洗无断言；错误码常量 ↔ 映射表反向核对缺失 | medium | patch：补测试 |
| YAML 里的明文口令被静默接受，与文档矛盾 | high | patch：拒绝（只允许环境变量/`password_file`），测试 |
| 口令文件：检查与读取之间可被替换、同组可写被接受、无大小上限、仅去一个换行 | medium | patch：`open`+`fstat` 同一句柄，拒绝同组写/其他用户任何访问，64 KiB 上限，去除全部末尾换行；符号链接按目标检查 |
| 监听冲突不识别通配地址与具体地址同端口；`https://:443/` 通过；YAML 错误回显含反引号的值片段、按字节截断破坏 UTF-8；相对路径重复报告 | medium | patch：均修复并有测试 |
| `Retry-After` 取整在极大值时溢出 | low | patch（一行算术） |
| `Secret.Format` 中 `%#v` 分支是死代码 | low | patch：用 `f.Flag('#')` |
| `ParseLevel` 大小写与配置校验不一致 | low | patch：统一小写 |
| 示例 `server.env: prod` 会让本地开发复制后混用生产 Redis 键空间 | low | patch：示例与文档加注，开发改 `dev` |
| `uid.MustNew` 文档自相矛盾；`clock` 文档夸大 lint 范围；缺信任边界说明 | low | patch：修正文字 |
| `Inline` 展开字段顺序不确定 | low | patch：排序 |
| 4xx 消息经清洗会把 `password: …` 自由文本遮蔽 | low | 驳回为设计取舍：已在 `apierr` 包文档写明（字段名与原因应分别放入 details） |
| Redis DB 限 0–15、无 MySQL/Redis TLS 与 ACL 用户选项、无 gRPC 状态映射、5xx 的 `Retry-After`/`Cache-Control`、缺可复用的 fake clock 与计时器抽象、`System().Now()` 为墙钟、`redact.String` 无快速路径与基准、`requestid` 丢弃非 ULID 上游 ID（已在包文档写明信任边界） | low | defer：均属后续 Story 的范围或性能优化，不影响本 Story 的安全与正确性断言 |
| 弱断言：`TestStackIsScrubbed…` 等 | low | 部分改进（`containsAny` 已删除；流式断言补全），其余不改 |

### AC 对照

| AC | 结果 |
|---|---|
| 1 YAML：schema 校验、环境变量覆盖、未知键拒绝；示例无口令/密钥 | 通过（表驱动校验规则 30+ 条、未知键/重复键/多文档、错误不回显值；示例有「可加载 + 敏感键无值 + 运行期缺口令被拒」测试） |
| 2 日志脱敏：六类键名、`le_…`/`lenr1.…`/PEM/DSN 口令；错误链与 panic 值 | 通过（语料 49 条泄漏用例 + 26 条良性对照 + 50 个敏感键名 × 27 种载体 + 随机口令 + 突变验证；良性语料逐字不变） |
| 3 统一错误：稳定 code、message_key、details、request_id、HTTP 映射表 | 通过（与 README §5/OpenAPI 机器核对；5xx 永不泄漏原因） |
| 4 Request ID：ULID 读取/生成、context、Zap/审计/Task/gRPC `x-request-id` 帮助函数 | 通过（HTTP 中间件、真实 gRPC 一元/流式往返） |
| 5 `clock`/`ulid` 封装、禁止直接 `time.Now()`（lint）、可注入 | 通过（forbidigo 规则 + 正反自测；单调 ULID、时钟回拨保护、熵故障/溢出返回错误） |

### DoD（基线 §25）

需求明确、代码完成、编译通过、单元测试（含 `-race`）与关键异常测试通过、文档更新、本机运行验证：满足。数据库迁移、API 可调用、前端功能：本 Story 不适用。**等待环境验证**：无（本 Story 仅依赖本机）。

### 提交

本地提交（未 push）：哈希见 `git log`；`git add` 逐路径，未含 `_bmad/`、`dist/`、`bin/`、二进制；使用 `git -c user.name=… -c user.email=…`，未改 `git config`。

### 遗留与后续

- Story 1.5 前置满足：依赖 1.4（`config`、`logx`、`apierr`、`requestid`、`clock`、`uid`）已就绪；数据库连接配置（host/port/name/user/password 通道）已在 `config.Database`，三类账号（app/maint/migrate）的拆分由 1.5 扩展 schema。1.5 需要 MySQL：本机 `mysqld` 位于 `/opt/homebrew/opt/mysql@8.0/bin/mysqld`（8.0.46，不在 PATH），本机另有常驻 MySQL 实例不得触碰；账号/binlog/触发器类用例用独立临时 `mysqld --no-defaults --datadir=<tmp> --port=<空闲端口> --socket=<tmp> --log-bin --log_bin_trust_function_creators=0`（`mysqld --initialize-insecure` 初始化独立 datadir，用后关闭并删除），常规用例用 `lingedge_test` 库与账号。
- 1.3 遗留的「生成的 Go 代码暂不入库」原因（go1.27.1 标准库漏洞）已随 go1.27.4 消失，且根 `go.mod` 现已含 grpc/protobuf，可在后续评估把 `make proto` 的生成物入库。
- 计时器抽象（`time.After/Sleep/NewTimer`）与可复用 fake clock 待首个需要可测试定时逻辑的 Story（1.14/1.15）引入。
