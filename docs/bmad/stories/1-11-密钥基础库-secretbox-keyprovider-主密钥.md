---
title: 'Story 1.11：密钥基础库（secretbox / KeyProvider / 主密钥）'
type: 'feature'
created: '2026-11-20'
status: 'review'
route: 'dispatch'
review_loop_iteration: 0
baseline_commit: 'a0f4803'
context:
  - docs/bmad/epics.md（Story 1.11 全文，约 1069–1091 行）
  - docs/architecture/adr/ADR-011-secrets-and-kms.md（信封加密、KeyProvider、轮换、进程内保护）
  - docs/architecture/tools-design.md（§13 master.key 由 Tools 步骤 18 生成、§15.1 LimitCORE=0、§17 备份 kek_ids_required）
  - docs/bmad/decisions-adopted.md、docs/database/README.md §2.2 与各 *_enc 列
  - docs/bmad/stories/1-4-…、1-5-…（Secret/redact/logx 约定）、1-10-…（格式与门禁约定）
---

## Intent

**Problem:** 证书私钥、Provider 凭据、TOTP 密钥等敏感值即将入库；若各模块自行加密，格式、AAD、轮换、主密钥保管会各不相同，数据库转储就可能泄露凭据，且无法轮换。

**Approach:** 在 `pkg/secretbox`（仅标准库，Server/Tools 共用）实现统一的信封加密：每值独立随机 DEK（AES-256-GCM）+ KEK 包装，AAD=`表名|列名|记录 uid` 同时绑定值与 DEK 包装，密文固定 `v1:<kek_id>:<wrapped_dek>:<nonce>:<ct+tag>`；`KeyProvider` 抽象主密钥，`FileKeyProvider` 以权限/属主校验读取主密钥文件（fail-closed）；`secrets.rewrap` 原语只重包装 DEK，并提供旧 KEK 移除前的核对；用 `[]byte` 持有密钥并清零；增加 G-5 日志脱敏扫描测试并接入 `make security`。

## Boundaries & Constraints

**Always:** 密文 5 段格式不变；AES-256-GCM；每值独立随机 DEK；AAD=`表名|列名|记录 uid`；缺主密钥 fail-closed；主密钥不入库/日志/错误文本；密钥材料 `[]byte` 且用后清零；旧 KEK 全部重包装前不可移除；加解密真实运行，不 Mock。

**Never:** 把主密钥放环境变量；启动/改动本机常驻 mysqld/redis；Docker；改冻结契约（contract-v1/migrations-m1）；push；改 git config；改其他产品目录。

## Code Map

- `pkg/secretbox/` -- `doc.go`；`secretbox.go`（`Box`：`Seal/Open/Rewrap`、`RecordContext`/AAD、`KeyID`、格式解析）；`keyprovider.go`（`KeyProvider` 接口、哨兵错误、`ValidKEKID`）；`fileprovider.go`（`FileKeyProvider`、主密钥文件格式、`ResolveMasterKeyPath`、`Close` 清零、格式化恒为占位）；`file_unix.go`/`file_other.go`（`O_NOFOLLOW`、同 fd 上校验 0400/0440 与属主）；`rewrap.go`（`RecordStore`、`RewrapAll`、`CheckKEKRemovable`）；`zero.go`。
- `pkg/secretbox/*_test.go` -- 单元、AAD 搬迁、轮换端到端、文件权限/属主/符号链接/格式、缺主密钥负向、DEK/KEK 清零、并发 `-race`、模糊测试。
- `internal/platform/config/` -- 新增 `secrets.master_key_file`（绝对路径校验 + 单测）；`configs/server.example.yaml` 同步。
- `tests/security/logscan_test.go` -- G-5 日志脱敏扫描（含对扫描自身的负向自测）；`Makefile` 的 `security` 目标接入。
- `docs/operations/secrets.md` -- 格式、主密钥文件、轮换流程、记入 Tools 渲染清单（1.32）的要求、等待环境验证项。

## Tasks & Acceptance

- [x] secretbox 信封加密与格式、AAD 绑定（AC1）
- [x] KeyProvider 接口 + FileKeyProvider（权限/属主/缺失/格式）（AC2）
- [x] rewrap 原语、批量重包装、KEK 移除核对（AC3）
- [x] 清零与 `-race`；`LimitCORE=0` 要求记入 Tools 渲染清单文档（AC4）
- [x] G-5 日志扫描测试并接入 `make security`（AC5）
- [x] 配置键、运维文档、sprint-status、开发记录、本地提交

## Verification

`make lint security gate-selftest test race vuln contract-check build verify-build migrate-test` 全绿；`go test -race -count=3 ./pkg/secretbox/`；`go test -fuzz=FuzzOpenNeverPanics -fuzztime=8s ./pkg/secretbox/` 无崩溃。

## Spec Change Log

无（规格未变更；未触碰 contract-v1 / migrations-m1）。

## Review Triage Log

见「开发记录 · 自审」。

## 开发记录

**开发会话**：2026-11-20，bmad-build（非交互、完全自主）。环境：macOS（Darwin 25.6.0 arm64）、go1.27.4。无需外部进程（密码学与文件权限均在本机真实执行）；`make race`/`migrate-test` 自带的独立临时 mysqld/redis 用后即清，收尾核对本机常驻 mysqld（PID 576/744）不变，无临时目录残留。

### 范围与设计要点

- **格式**：`v1:<kek_id>:<b64url(wrapped_dek)>:<b64url(nonce)>:<b64url(ct+tag)>`；`wrapped_dek = nonce(12)||GCM(KEK, DEK, aad)`（60B）。解析严格：段数、版本、kek_id 字符集、各段长度（nonce 12、ct≥16、wrapped≥60）。
- **AAD**：`表名|列名|记录 uid`，同时用于值加密与 DEK 包装；片段为空或含 `|` 一律拒绝（防分隔符混淆）。`Rewrap` 的解包同样校验 AAD，因此搬迁的密文无法被「重包装洗白」。
- **FileKeyProvider**：配置路径（`secrets.master_key_file`）优先，否则 `$CREDENTIALS_DIRECTORY/master.key`。`O_NOFOLLOW` 打开，在同一 fd 上校验：普通文件、权限恰为 0400/0440 且无 setuid/setgid/sticky、属主 ∈ {运行用户 euid, root}（`FileOptions.AllowedUIDs` 可注入以便测试）。文件格式：`<kek_id>:<b64url 32B>` 多行 + 可选 `@current`；每个密钥须恰 32B 且非全零。错误文本只含行号/固定说明。状态在内部指针后，按值拷贝并 `%v/%#v` 打印也只得占位文本。
- **缺主密钥**：构造期 `ErrMasterKeyMissing`（调用方据此拒绝启动）；解密遇文件里没有的 `kek_id` → `ErrKEKNotFound` 并指出缺的 `kek_id`（恢复旧备份场景，F-7）。
- **轮换**：`Box.Rewrap`（单值，只改 DEK 包装，nonce/ct 原样，回读核对）；`RewrapAll`（按（表,列,uid）游标分批、可重入、单条失败入报告不中止、并发改写计 Skipped、乱序存储防死循环）；`CheckKEKRemovable`（非当前 + `CountByKEK==0` + 不在 `kek_ids_required`，除非 `DropBackupCompat`）。
- **清零**：DEK 在 Seal/Open/Rewrap 路径 `defer Zero`；`FileKeyProvider.Close` 清零全部 KEK 并让后续调用 `ErrClosed`；读文件缓冲区解析后清零。已知限制：`crypto/aes` 展开的轮密钥由运行时持有，无法清零，故需 `LimitCORE=0`（已写入 `docs/operations/secrets.md` §5 作为 1.32 渲染清单要求）。
- **G-5**：`tests/security/logscan_test.go` 向真实 `logx` 注入 PEM/API Token/enroll Token/Bearer/主密钥/明文/DSN 口令（消息、字段、错误链、嵌套对象、panic），并真实走一遍 secretbox 流程后断言日志与 secretbox 自身错误文本无泄露；另有「裸 logger 必被扫描发现」的负向自测。已接入 `make security`（现覆盖 G-5/G-6/G-11/G-12/品牌）。

### 实际验证

- `go test -race -count=3 ./pkg/secretbox/`：通过，覆盖率 89.2%；模糊测试 `FuzzOpenNeverPanics` 8s/约 184 万次执行无崩溃。
- AC1 格式与各段长度、独立 DEK（两次加密各段全不同；DEK 互换失败）、AAD 搬迁（别的记录/别的列/别的表均 `ErrDecrypt`）、篡改任一段、换主密钥、畸形输入、未知 kek_id。
- AC2 权限表：0400/0440 接受；0600/0640/0644/0444/0404/0200/0000/setuid 拒绝；属主判定（允许集不含本进程 uid 时拒绝）；符号链接/目录拒绝；文件格式 10 种畸形输入拒绝且错误不含密钥；缺失（未配置/不存在）与缺 kek_id 负向；`$CREDENTIALS_DIRECTORY` 路径解析与真实读取；格式化不泄露。
- AC3 端到端轮换：250 条记录（两张表）在 k1 下写入 → 新增 k2 → 新写入用 k2、旧数据仍可解 → k1 因 250 条引用被拒移除 → 分批 `RewrapAll`（250 条，幂等再跑为空）→ 仅 DEK 段与 kek_id 改变 → 仅含 k2 的主密钥可解密全部数据；备份需要（`kek_ids_required`）拒绝与 `DropBackupCompat` 放行；失败/并发改写/取消场景。
- AC4 清零：Seal/Open/Rewrap 三路径 DEK 切片全零（经包装真实 provider 的探针，另断言曾非零）；`Close` 后内部 KEK 全零；并发 Seal/Open/Rewrap + Close 在 `-race` 下无竞争。
- 门禁：`make lint`（0 issues）、`security`、`gate-selftest`、`test`、`race`（exit 0，23 个包 ok）、`vuln`（No vulnerabilities found）、`contract-check`（OK，无破坏性变更，迁移 13 个哈希一致）、`build`、`verify-build`、`migrate-test` 全绿。

### 自审

| 发现 | 判定 | 处置 |
|---|---|---|
| `FileKeyProvider` 值拷贝后 fmt 反射会打印未导出的 KEK 字节 | high | patch：状态移入内部指针，值/指针格式化均占位；`TestProviderNeverPrintsKey` 覆盖 `*kp` |
| `RewrapAll` 遇到永远失败的记录会因「每轮都列出它」而死循环 | high | patch：改用（表,列,uid）游标；对未升序的存储直接中止；失败计入报告 |
| 解析失败路径上已解码的 KEK 残留 | medium | patch：解析失败时清零已解码的密钥，读文件缓冲区 `defer Zero` |
| G-5 初版用「password=」字面量触发 G-6 固定口令门禁 | low | patch：测试改用 `dsn=` 键（门禁正常工作，未豁免） |
| 轮密钥无法清零 | accept | Go 标准库限制，`LimitCORE=0` 补偿，已文档化 |
| `RecordStore` 无真实库实现；`CountByKEK/ListByKEK/Replace` 仅有内存实现的测试 | patch（遗留补齐，见下） | 已补 `internal/platform/secrets/repository` 与真实 MySQL 端到端测试；worker `secrets.rewrap` 任务接线仍属后续 Story |
| 自由文本中不带敏感键名的裸 base64 主密钥无法被 redact 识别 | patch（保守识别，见下） | `pkg/redact/bare.go`：恰 43 字符 + 三类字符 + 熵≥4.5；残余风险见 docs/operations/secrets.md §7 |

### AC 对照

| AC | 结果 |
|---|---|
| 1 AES-256-GCM、独立 DEK、5 段格式、AAD、搬迁失败 | 通过 |
| 2 FileKeyProvider：0400/0440 + 属主、缺失明确错误并拒绝、不入库不入日志 | 通过（本机可验证部分）；真实 systemd `$CREDENTIALS_DIRECTORY` 与 root 属主场景为等待环境验证 |
| 3 轮换：新增 kek_id → rewrap → 移除旧 KEK；未全部重包装不可移除 | 通过（原语 + 内存存储；库表接线待后续 Story） |
| 4 `[]byte` 清零、`-race`、`LimitCORE=0` 记入渲染清单 | 通过（清单要求记于 `docs/operations/secrets.md` §5，由 1.32 落实渲染与 golden） |
| 5 日志扫描注入私钥/Token 被脱敏（G-5） | 通过，已接入 `make security` |

### DoD 与遗留

DoD（基线 §25）：除下述等待环境验证项外满足；前端/迁移/API 可调用不适用。**等待环境验证**：① 真实 systemd `LoadCredential` / `$CREDENTIALS_DIRECTORY` 下的端到端启动；② `root:root 0400` 原文件与以 root 身份运行时的属主接受路径（本机非 root，仅验证判定逻辑）；③ `LimitCORE=0` 在 Linux 单元中的生效（Story 1.32）。**后续接入提示**：各 `*_enc` 列的仓储用 `Box.Seal/Open`（AAD 用表名/列名/uid）；`enc_key_id` 写 `Box.CurrentKeyID()`；`RecordStore` 与 `kek_ids_required` 在 worker `secrets.rewrap` 任务与 Tools `backup` 接线；解密热路径默认不缓存明文。

### 遗留项补齐（同日第二次会话）

1. **真实 RecordStore**：`internal/platform/secrets/repository`（符合 `internal/**/repository` 路径约定；方法 `*AcrossTenants(ctx, scope, …)` 以满足 repo-scope 门禁——首版方法无 Scope 首参被该门禁拦下，据此重构）+ `internal/platform/secrets.NewRecordStore`（无 SQL 的适配层，绑定平台 Scope）。白名单四个目标，以 `migrations/` 实际 `*_enc` 列为准：`certificates.private_key_enc`、`signing_keys.private_key_enc`（有 `enc_key_id`）、`system_settings.value_enc`、`users.totp_secret_enc`（无 `enc_key_id` 列 → 以密文第 2 段 kek_id 判定；未改冻结迁移）。SQL 全为 `db.Q` 常量，无标识符拼接；keyset 游标；CAS 用 `BINARY` 比较（变异测试证明必要：去掉后大小写变体命中）；四表 app 均有 UPDATE，故仅用 app 池、不走 maint 约定（`repository.New` 拒绝 maint/migrate 池，仓储方法与 `NewRecordStore` 拒绝非平台 Scope）。`provider_credentials` 随 P2，未提前声明；`TestTargetsMatchMigrations` 防遗漏。
2. **真实库端到端测试** `tests/integration/secrets`（独立临时 mysqld + 全部 13 个内嵌迁移 + 真实 `lingedge_app` + 真实 `FileKeyProvider`，无 Mock；仅用 root 连接造数/旁路篡改/核对）：轮换端到端（52 条/4 表、批 5）含拒绝/放行/仅 k2 可解全部、nonce/ct 不变；CAS 并发冲突；CAS 大小写敏感；AAD 搬迁（Failures 记录且 k1 仍被计数拒绝移除）；中断（第 10 次写回前取消）后续跑；NULL 与软删除；kek_id 非通配符；账号授权核对。
3. **`CheckKEKRemovable` 接真实引用计数**（上述测试内）；备份 `kek_ids_required` 数据源属 Story 1.41，**未伪造**，接口保持，文档标注（secrets.md §3.2）。
4. **sprint-status**：1.11 `waiting_env_verification: true`，补 `environment_verification.1-11-…`（systemd `$CREDENTIALS_DIRECTORY`、root 属主文件、`LimitCORE=0`、`kek_ids_required` 数据源 4 项待验）。
5. **裸 base64 主密钥脱敏**：评估后做保守版（风险可控）：恰 43 字符的最大连续 base64 串 + 大写/小写/数字齐全 + 熵≥4.5；正例 1000+ 随机样本、反例（ULID/UUID/hex 散列含大写/SHA-1/路径/URL/主机名/驼峰/kebab/PEM 行/JWT 头载荷）原样；G-5 加入裸密钥注入。不做十六进制（与指纹不可分）；32 字节散列的 base64 与 JWT HS256 签名段会被过遮。残余风险写入 secrets.md §7。

**变更文件**：`internal/platform/secrets/{recordstore.go,repository/{doc,store}.go}`、`tests/integration/secrets/*`、`pkg/redact/{bare,redact}.go`、`pkg/redact/bare_test.go`、`tests/security/logscan_test.go`、`docs/operations/secrets.md`、`docs/bmad/sprint-status.yaml`、本文件。
