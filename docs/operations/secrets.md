# 密钥基础库运维说明（Story 1.11；ADR-011、tools-design §13/§15/§17）

实现：`pkg/secretbox`（信封加密、`KeyProvider`、`FileKeyProvider`、`secrets.rewrap` 原语）；配置键 `secrets.master_key_file`。

## 1. 密文格式

`v1:<kek_id>:<b64url(wrapped_dek)>:<b64url(nonce)>:<b64url(ct+tag)>`（base64url 无填充，共 5 段）。

- 每个值一个独立随机 DEK（32B）；值用 AES-256-GCM 加密（nonce 12B、tag 16B）。
- DEK 由 KEK 以 AES-256-GCM 包装：`wrapped_dek = nonce(12) || GCM(KEK, DEK)`，32B DEK 包装后 60B。
- AAD（值与 DEK 包装同用）= `表名|列名|记录 uid`。含 `|` 或为空的片段被拒绝，避免分隔符混淆。
- `kek_id`：`[a-z0-9][a-z0-9_-]{0,31}`（对应 `enc_key_id VARCHAR(32)`，且不含 `:`）。

## 2. 主密钥文件（`master.key`）

由 Tools 安装步骤 18 用 `crypto/rand` 生成（Web 向导不接触），文本格式：

```
# 注释与空行忽略
@current k2              # 可选；缺省取最后一条
k1:<base64url 无填充，32 字节随机值>
k2:<base64url 无填充，32 字节随机值>
```

读取路径：配置 `secrets.master_key_file`（绝对路径）优先，否则 `$CREDENTIALS_DIRECTORY/master.key`（systemd `LoadCredential=master.key:/etc/lingedge/server/master.key`）。

启动校验（任一失败即拒绝启动，fail-closed）：不得是符号链接（`O_NOFOLLOW`）；必须是普通文件；权限恰为 `0400` 或 `0440` 且无 setuid/setgid/sticky；属主为 root 或运行用户的 uid；大小 ≤64KiB；每个密钥解码后恰 32B 且非全零；kek_id 合法且不重复。缺失主密钥返回 `ErrMasterKeyMissing`（明确提示）；解密遇到文件里没有的 `kek_id` 返回 `ErrKEKNotFound` 并指出缺哪个 `kek_id`（恢复旧备份时需带对应主密钥，F-7）。

主密钥不入库、不入日志（格式化输出恒为占位文本；错误只含行号与固定说明）、不入备份产物。

## 3. 轮换

1. 在 `master.key` 追加新 `kek_id`（并 `@current` 指向它）并重载；新写入使用新 KEK，旧数据仍可解密。
2. 运行 `secrets.rewrap`（`Box.RewrapAll(from, to)`）：只重包装 DEK，`nonce` 与 `ct+tag` 原样保留；可重入；单条失败记入报告而不中止；并发被改写的记录计入 `Skipped`，下一轮再处理。
3. 移除旧 KEK 前必须通过 `CheckKEKRemovable`：非当前 KEK；`CountByKEK==0`（库内无 `enc_key_id` 引用）；不在保留备份清单的 `kek_ids_required` 内，除非显式 `--drop-backup-compat`。任一不满足即拒绝。

### 3.1 RecordStore 真实实现（`internal/platform/secrets/repository`）

`secretbox.RecordStore` 的真实 MySQL 实现：`repository.New(appPool)` 得到仓储（方法 `*AcrossTenants(ctx, scope, …)`，含 SQL），再经 `secrets.NewRecordStore(repo, tenantscope.PlatformForJob(actor))` 绑定平台 Scope 适配为接口（`internal/platform/secrets`，不含 SQL；这样满足 repo-scope 门禁：Repository 方法首参必须是 Scope）。

- **扫描范围（受控白名单，以 `migrations/` 实际存在的 `*_enc` 列为准）**：`certificates.private_key_enc`、`signing_keys.private_key_enc`（二者有 `enc_key_id` 列）；`system_settings.value_enc`、`users.totp_secret_enc`（迁移里**没有** `enc_key_id` 列，不改已冻结迁移，改以密文第 2 段 `kek_id` 判定）。`provider_credentials.credential_enc` 随 P2 建表，届时在 `targets` 追加；`tests/integration/secrets` 的 `TestTargetsMatchMigrations` 会在迁移新增 `*_enc` 列而未登记时失败。
- **SQL 安全**：每个目标的 list/replace/count 都是编译期常量（`db.Q`），不拼接任何标识符；外部只传值（kek_id、uid、密文），全部 `?` 绑定。判定「以某 KEK 包装」用 `BINARY LEFT(col, n) = 'v1:<kek>:'`（前缀作参数，`kek_id` 中的 `_` 不会成为 LIKE 通配符）；有 `enc_key_id` 的表同时核对该列（OR，只会更保守）。
- **游标与顺序**：按（表,列,uid）升序、`uid > ?` keyset 分批；uid 为规范 ULID（大写），库默认排序规则下与字节序一致。
- **CAS 写回**：`UPDATE … SET <col>=?, enc_key_id=? WHERE uid=? AND BINARY <col>=?`（旧密文逐字节相等才写；`BINARY` 是必须的，库默认 `utf8mb4_0900_ai_ci` 不区分大小写，而 base64url 区分）。返回受影响行数==1；0 行表示并发改动/删除，计入 `Skipped`，下一轮再处理。
- **引用计数**：`CountByKEK` 统计四个目标全部行（含软删除行：行与备份中仍在）。`CheckKEKRemovable` 已接到它（`tests/integration/secrets.TestRotationEndToEnd`）。
- **账号与权限边界**：四张目标表在各自迁移里都已 `GRANT SELECT, INSERT, UPDATE, DELETE … TO lingedge_app`，因此重包装只用 **app 池**（`repository.New` 拒绝 maint/migrate 池；`lingedge_maint` 对这些表无任何授权，测试核对）。无需走 maint 约定，也未改动冻结迁移（contract-v1 / migrations-m1）。
- **租户范围**：`certificates` 是租户表而重包装是跨租户批处理，仓储每个方法与 `NewRecordStore` 都要求平台范围 Scope（`PlatformForJob`；租户范围/零值被拒绝，不能用平台表豁免）。`PlatformForJob` 调用点受 `[tenantscope-mint]` 白名单约束，须登记于 Story 1.14/1.15 的作业清单。
- **真实环境验证**：`tests/integration/secrets`（独立临时 mysqld、全部 13 个内嵌迁移、真实 `lingedge_app` 账号、真实 `FileKeyProvider`）：端到端轮换（52 条/4 表，小批量游标）、移除 KEK 的拒绝/放行、仅含新 KEK 的主密钥解全部、nonce/ct 段不变、CAS 并发冲突（真实并发写入者）、CAS 大小写敏感（变异验证）、AAD 搬迁、中断后续跑、NULL/软删除、kek_id 非通配符。

### 3.2 备份 `kek_ids_required`（依赖后续 Story，未伪造）

`RemovalCheck.BackupRequired` 保持为接口输入：保留备份清单（`BACKUP.json` 的 `kek_ids_required`，tools-design §17）由 **Story 1.41（backup / restore / uninstall）** 生成，`lingedge … backup list` 读取后传入。当前仓库没有真实的备份清单表/文件，因此**本 Story 不提供也不模拟其数据源**；在 1.41 落地前，`CheckKEKRemovable` 的备份维度只由调用方显式传入的清单决定（测试中以显式输入验证拒绝与 `--drop-backup-compat` 放行语义）。**运维上在 1.41 完成前，移除 KEK 前须人工确认没有仍需该 KEK 的保留备份。**

## 4. 进程内保护

密钥材料一律 `[]byte`，使用后 `Zero`；`FileKeyProvider.Close` 清零全部 KEK。已知限制：`crypto/aes` 展开的轮密钥由 Go 运行时持有，无法清零；因此 systemd 单元必须 `LimitCORE=0`。

## 5. 记入 Tools 渲染清单（Story 1.32）的要求

- `lingedge-server.service` 必须含 `LimitCORE=0`（ADR-011 §9；tools-design §15.1 已有，1.32 的 golden 与加固项检查必须保留）。
- `LoadCredential=master.key:/etc/lingedge/server/master.key`；原文件 `root:root 0400`（Tools 步骤 18 生成）。
- 不得用环境变量传递主密钥。

## 6. 等待环境验证

登记于 `docs/bmad/sprint-status.yaml` 的 `environment_verification.1-11-…`（`waiting_env_verification: true`），需 Linux/systemd 验收机（ENV-LNX）逐项验证：

- 真实 systemd 下的 `$CREDENTIALS_DIRECTORY`（凭据文件的实际属主/权限由 systemd 设定）与 `root:root 0400` 原文件经 `LoadCredential` 的端到端启动；本机（macOS、无 systemd、非 root）仅验证了权限、属主判定逻辑（以允许 uid 集合注入）与路径解析。
- 以 root 身份运行时 `root:root 0400` 的接受路径（本机以非 root 运行，只能用「属主=运行用户」覆盖同一判定逻辑）。
- `LimitCORE=0` 在 Linux 单元中的实际生效（属 Story 1.32 渲染与验收）。

## 7. 日志脱敏与裸 base64 主密钥（残余风险）

`pkg/redact` 原先只按键名与已知格式识别，自由文本里「不带键名」的裸 base64 主密钥识别不到。现增加保守识别（`pkg/redact/bare.go`）：仅当一段最大连续 base64 串**恰为 43 字符**（32 字节密钥；std 变体可带 1 个 `=`）、同时含大写/小写/数字、且香农熵 ≥ 4.5 bit/字符时遮蔽。反例测试断言 ULID、UUID、十六进制 SHA-1/SHA-256（含大写）、路径、URL、主机名、驼峰标识、kebab 文本、PEM 行、JWT 头与载荷等保持原样；正例测试用随机密钥（1000+ 样本，4 种 base64 变体、9 种上下文）断言必遮、且幂等。G-5 日志扫描已加入裸密钥注入用例。

**残余风险（接受并记录）**：
1. 十六进制形态的主密钥（64 个十六进制字符）与证书/文件指纹的十六进制 SHA-256 不可区分，**不做识别**；
2. 32 字节散列的 base64 形态（SHA-256 的 base64、JWT 的 HS256 签名段）与密钥不可区分，会被**一并遮蔽**（过遮，不泄漏；当前代码库没有日志输出此类值）；
3. 其他长度（16/24/64 字节密钥）、被换行/空格折断或再编码（如二次 base64）的密钥不识别；约 0.1% 的随机 32 字节密钥缺某一类字符而不满足条件；
4. 因此纪律仍是：主密钥永不进入任何日志调用——`FileKeyProvider` 自身从不格式化输出密钥，裸串识别只是兜底。

## 8. 数据库凭据（AA-7 已解决；ADR-026、tools-design §13.1，UD-31 默认采纳、可推翻）

> 本节是运维视角摘要；设计与理由见 ADR-026。**已由 Story 1.24 实现**（`internal/platform/{dbcred,setup,migrate}`、`lingedge-server setup|migrate credential|db credential rotate`）；环境变量/`*_file` 通道保留为最高优先级的显式覆盖。真实 systemd 下 `lingedge` 用户写 `/var/lib/lingedge/server/secrets`、root 写 `/etc/lingedge/server/migrate.yaml` 的属主/权限路径仍为「等待环境验证」（ENV-LNX，本机只验证权限位与属主判定逻辑）。

| 凭据 | 位置 | 属主/权限 | 读取方 | 轮换 |
|---|---|---|---|---|
| `lingedge_app` / `lingedge_maint` / Redis 口令、DB 连接目标 | `/var/lib/lingedge/server/secrets/runtime-credentials.json`（口令为 `pkg/secretbox` 密文，AAD=`runtime_credentials\|<路径>\|<账号>`；目录 0700） | `lingedge:lingedge` 0600 | Server 各角色启动时（`lingedge`） | root 执行 `lingedge-server db credential rotate --role app\|maint --admin-user U`（`RETAIN CURRENT PASSWORD` → 重写文件 → 重启 → `DISCARD OLD PASSWORD`）；KEK 轮换时随 `secrets.rewrap` 重包装 |
| `lingedge_migrate` 口令（运行/升级期） | `/etc/lingedge/server/migrate.yaml` | **root:root 0400** | root 运行的 `lingedge-server migrate`（Tools 升级、手工） | `lingedge-server migrate credential reset --admin-user U`；丢失无「找回」，只能重置 |
| 临时 DB 管理员、`lingedge_migrate`（向导期） | 仅内存 | — | setup mode 的 Server | 无；用后清零，续做需重新提供 |
| `setup --from-file` YAML | 调用者属主 0600 | 0600 | `lingedge-server setup` | 首次成功解析后自动清零并删除 |

操作要点：
1. 向导完成页若显示「迁移账号口令（仅一次）」，立即在服务器上执行页面给出的 `sudo lingedge-server migrate credential init --host … --port … --database …` 并键入口令保存；错过则用 `credential reset`。
2. 向导结束后自行删除临时管理员账号；不要把管理员口令保存到任何文件（`--from-file` 例外且会被自动删除，请确认 `from_file_residual` 为 false）。
3. **备份**：`runtime-credentials.json` 随 `/var/lib/lingedge/server/` 备份（密文，须有对应 `master.key`；备份清单 `kek_ids_required` 含其 `kek_id`）；`migrate.yaml` **默认不进备份**（`--include-migrate-credential` 且 `--encrypt` 才含）；恢复后需重新 `credential init`，并视口令是否已轮换执行 `credential rotate/reset` 对齐。
4. **主密钥轮换**：`runtime-credentials.json` 不在数据库，须作为 `secrets.rewrap` 的额外目标（ADR-011 修订 2026-11-21）；移除旧 KEK 前也要核对它。
5. 日志里不应出现任何口令；若怀疑泄露，先 `rotate/reset`，再排查。
6. **setup token 与向导**：token 由 Tools 生成并只显示一次，哈希在 `/etc/lingedge/server/setup-token.hash`；向导 `complete` 之后哈希文件被删除（若 `lingedge` 无权删除 root 属主目录里的文件，则清空其内容使 token 永久失效，并提示手工删除）。失败节流默认同一来源 5 分钟内 10 次（Redis 可用时全局精确，否则进程内计数）。
7. **`--from-file` 清理的局限**：进程对文件内容 0 覆盖后 `unlink`；日志型/COW 文件系统、快照与备份上可能残留旧块，故文件只应放在受控的 tmpfs/加密卷，用后检查结果里的 `from_file_residual`。
8. 宝塔 MySQL：推荐让向导用宝塔 MySQL 管理员口令（内存中一次）建账号；用面板预建账号常缺 `GRANT OPTION`/`SET_USER_ID`，预检会阻断并给出指引（具体默认权限待 ENV-BT 验证）。
