# ADR-026 数据库凭据交接与生命周期（AA-7）

- 状态：已接受（架构代理非交互决策；**默认采纳、可推翻**，登记为 UD-31）｜日期：2026-11-21｜关联：ADR-016 §6/§7、ADR-011、ADR-003/ADR-012（信任域）、ADR-013；tools-design §13/§15.1/§17；database/README §5.1；PRD FR-11、NFR-6、NFR-12；基线 §21.1｜阶段：M1（阻塞 Story 1.24 的凭据部分、1.35 联调、1.43 升级）

## 背景

安装向导运行在 setup mode 的 Server 进程内（运行用户 `lingedge`，`ProtectSystem=strict`，仅 `/var/lib/lingedge/server` 与 `/var/log/lingedge` 可写，无权写 root 属主文件，也读不到 `/etc/lingedge/server/master.key` 原文件——只能经 `LoadCredential` 取得副本），但向导第 4 步需要 `lingedge_migrate` 凭据执行迁移；`server.yaml` 由 Tools（root）生成且「不含口令」。原 tools-design §13 把 `lingedge_migrate` 口令放在 `/etc/lingedge/server/migrate.yaml`（root:root 0400），而写它的人（向导）恰恰无权写；`lingedge_app`/`lingedge_maint` 口令（以及向导里填写的 DB/Redis 连接目标）没有任何落点。同时，向导未必能「事后」把任何东西交给 root——Server 进程没有提权路径，也不应有。

必须同时满足：① 不削弱 ADR-003/012 的信任域（DB 凭据不是第四个信任域，但不得与签名私钥/CA 私钥混放）；② `lingedge_migrate`（`ALL ON db.* WITH GRANT OPTION` + `SET_USER_ID`）不得长期驻留在 `lingedge` 用户可读之处（CC-3 已否决 Server 常驻持有 migrate 池）；③ 与 ADR-011 的 `FileKeyProvider` 权限模型、systemd 加固一致；④ 无头路径与中断续做可行；⑤ 不改冻结迁移与已冻结契约的既有语义（只加法）。

## 决策

### 1. 凭据清单（谁持有、放哪里、谁读取、多久）

| 凭据 | 载体 | 路径 / 属主 / 权限 | 写入方 | 读取方 | 生命周期 |
|---|---|---|---|---|---|
| **临时 DB 管理员口令**（可选，仅「由向导建库建账号」时） | 仅内存（`config.Secret`，`[]byte`） | 无文件 | 安装者经向导表单或 `--from-file` 提供 | setup mode 的 Server 进程（`lingedge`） | 步骤 2 的建库/建账号完成（或失败/续做放弃）后**立即清零**；不入 `system_installations.state`、日志、审计、错误信息 |
| **`lingedge_migrate` 口令（向导期）** | 仅内存 | 无文件 | 向导：安装者提供，或在「创建账号」模式下由向导 `crypto/rand` 生成（≥32 字节熵，base64url） | 同一 setup mode 进程：步骤 2 的预检（触发器探针、GRANT OPTION）与步骤 4 的迁移 | 步骤 4 与预检结束即清零；`complete` 响应写出后清零；**向导进程绝不持久化** |
| **`lingedge_migrate` 口令（运行/升级期）** | 文件 | `/etc/lingedge/server/migrate.yaml`，**root:root 0400**（取代旧设想的同名文件，内容见 §4） | **root 侧命令** `lingedge-server migrate credential init\|reset`（Tools 升级流程亦调用）；绝不由 `lingedge` 进程写 | root 运行的 `lingedge-server migrate`（Tools 升级 1.43、联调 1.35、运维手工） | 长期；随升级复用；可轮换（§8）；备份**默认排除** |
| **`lingedge_app` / `lingedge_maint` 口令**、DB 连接目标（host/port/name/tls/socket）、**Redis 地址与口令** | 文件（口令字段信封加密，其余明文 JSON） | **`<server.data_dir>/secrets/runtime-credentials.json`**，目录 `lingedge:lingedge 0700`、文件 `lingedge:lingedge 0600`（缺省即 `/var/lib/lingedge/server/secrets/…`）；键 `secrets.runtime_credentials_file` 可改 | 向导（`lingedge`，原子写：同目录临时文件 + `fsync` + `rename`）；轮换时 root 侧命令写后 `chown lingedge:lingedge` | Server（api/scheduler/worker 各角色，用户 `lingedge`）启动时；root 侧**不**据此取得连接目标（§4 的信任规则） | 长期；随口令轮换重写；随备份（密文） |
| **setup token** | SHA-256 哈希文件 | `/etc/lingedge/server/setup-token.hash`（tools-design §13，不变） | Tools | setup mode Server | `complete` 后删除（不变） |
| **`--from-file` YAML** | 文件 | 调用者属主、**0600**（不满足则拒绝） | 运维 | `lingedge-server setup` | 读入后即清理（§7） |

> **与 epics AA-7「建议默认」的差异（冲突说明）**：建议默认写「app/maint 口令经信封加密存 root:lingedge 0440 的 secrets 文件（`LoadCredential`）」。该形态**无法落地**：向导以 `lingedge` 运行，不能创建或改写 root 属主文件，也没有向 root 交接的通道；`LoadCredential` 只在服务启动时一次性拷贝，向导写完后进程要么重启要么长期持有旧副本。故改为：**文件放在 `lingedge` 可写的数据目录，靠信封加密（`pkg/secretbox`，KEK = `master.key`）而非文件属主保证机密性**。这不削弱保护目标：威胁是「数据目录备份/磁盘镜像泄露」——密文没有 `master.key`（备份默认不含，F-7）不可解；运行时同一用户本就能解密（与 `signing_keys.private_key_enc` 同模型）。其余部分（migrate 口令由 root 侧命令写入 root:root 0400、向导不持久化、升级按需交互输入）完全采纳。

### 2. 向导在 setup mode 内就地迁移，migrate 凭据只在内存

- 向导步骤 4 **进程内**调用 `internal/platform/migrate` 的同一 runner（`Plan/Up`），等价于 `lingedge-server migrate up`，不再 fork 子命令、不经环境变量/文件交接。ADR-016 决策 7 的「Tools 不直接连库跑 SQL，只调用 Server 的 `migrate` 子命令」不变（Tools 仍走 CLI；向导是 Server 自身）。
- migrate 池只在 setup mode、`install.lock` 不存在时由 setup 服务创建，步骤结束即 `Close()` 并清零口令；常规模式（含 `complete` 之后的同一进程）**没有任何代码路径**可构造 RoleMigrate 池——以 `gates` 或单测断言（setup 句柄仅在 `mode==setup` 注入；`complete` 后再调用返回 `410 setup.already_installed`）。这与 CC-3 对「Server 常驻持有 migrate 池」的否决一致：本决定只允许 setup 窗口内的**瞬时**持有。
- 凭据对象一律用 `config.Secret`/`[]byte`，使用后 `Zero`；`system_installations.state` 只记录步骤名与「账号已创建」「迁移已执行」这类布尔/版本，不含任何口令、不含口令长度或哈希。

### 3. 运行期口令文件 `runtime-credentials.json`

格式（`v:1`）：

```json
{"v":1,
 "database":{"host":"127.0.0.1","port":3306,"name":"lingedge","tls":"preferred",
   "app":  {"user":"lingedge_app",  "password_enc":"v1:<kek_id>:…"},
   "maint":{"user":"lingedge_maint","password_enc":"v1:<kek_id>:…"}},
 "redis":{"addr":"127.0.0.1:6379","db":0,"tls":false,"password_enc":"v1:…"}}
```

- `password_enc` 用 `pkg/secretbox` 五段格式；**AAD = `runtime_credentials|<JSON 路径>|<DB 用户名或 redis>`**（例 `runtime_credentials|database.app.password|lingedge_app`；各片段非空、不含 `|`），防止密文在字段间挪用。`kek_id` 取当前 KEK。非口令字段明文，便于 `doctor` 在无主密钥时只读检查。
- 读取：Server 装配配置时，**`server.yaml`/环境变量/`*.password_file` 显式给出的值优先**（运维托管或容器化场景），缺省才读本文件；最终仍进入 `config.Secret`。文件缺失且无显式口令 ⇒ 沿用现有行为（setup mode 或 fail-closed 拒绝启动）。解密只解当前角色需要的字段（api 角色不解 `maint`）。同一用户进程内这只是纵深防御，不是隔离边界；P3 角色拆分为独立单元时可改为多文件（Deferred）。
- 写入时机：步骤 2（DB）成功后**立刻**写 `database` 段，步骤 3（Redis）成功后写 `redis` 段，因此中断续做可读回（§6）；写入前先 `Decrypt` 往返自检，主密钥不可用则拒绝并不留残文件。
- 启动校验（fail-closed，同 `readSecretFile` 精神）：普通文件、非符号链接（`O_NOFOLLOW`）、属主 = 运行用户或 root、权限无 group/other 位（0600/0400）、大小 ≤64KiB、`v` 已知、`DisallowUnknownFields`。
- 与 `master.key` 的关系：`master.key` 仍只由 Tools 生成、`root:root 0400`、经 `LoadCredential` 提供（ADR-011 不变）；本文件**不**经 `LoadCredential`。

### 4. `lingedge_migrate` 的 root 侧文件 `migrate.yaml`

- 路径 `/etc/lingedge/server/migrate.yaml`，`root:root 0400`，YAML（`KnownFields`）：`host`/`port`（或 `socket`）、`database`、`user`（缺省 `lingedge_migrate`）、`password`、`tls`。口令明文（root 0400 的信任边界与 `master.key` 同级；信封加密在此无意义，因为 KEK 同在 root 之下）。
- **信任规则（防止 `lingedge` 被攻陷后借 root 套取口令）**：root 运行的 `lingedge-server migrate` 的连接目标只来自 ① 命令行/环境变量显式值 ② `migrate.yaml` ③ root 属主的 `server.yaml`（root 运行时要求配置文件属主 root 且不被 group/other 写入）；**绝不**读取 `runtime-credentials.json`（`lingedge` 可写），否则被攻陷的 `lingedge` 可把 host 指向攻击者的 MySQL，让 root 侧把 migrate 口令发过去。
- 来源优先级（`migrate` 命令）：环境变量 `LINGEDGE_MIGRATE_DB_PASSWORD` ＞ `LINGEDGE_MIGRATE_DB_PASSWORD_FILE` ＞ `migrate.yaml` ＞ **交互输入**（仅 TTY，无回显；非 TTY 退出码 3「前置条件」并说明如何提供）。Tools 向其直接子进程以**环境变量**传递刚交互取得的口令（`ps` 不可见；绝不进 systemd `Environment=`、`state.json`、journal、日志）。
- 写入口：`sudo lingedge-server migrate credential init`（交互提示口令，目标默认取自命令行参数；向导完成页给出**预填 host/port/database 的完整命令**，操作者只需键入口令）与 `… credential reset --admin-user U`（交互提示管理员口令，`ALTER USER … IDENTIFIED BY` 重置并写文件，用于口令丢失、续做重置、定期轮换）。`init` 先真实连接并跑账号预检（GRANT OPTION/`SET_USER_ID`），失败不写文件。写文件用 `O_EXCL`+`0400`+`fsync`+`rename`；已存在则不覆盖，除非 `--force`。
- 备份：`/etc/lingedge/server/` 备份默认**排除** `migrate.yaml`（同 `master.key`），仅 `--include-migrate-credential` 且 `--encrypt` 才包含；恢复后重新 `credential init`。

### 5. 向导中的凭据流

1. 步骤 2 两种模式：**(A) 建账号模式**（提供临时管理员凭据）：向导创建专用库与三类账号；`lingedge_app`/`lingedge_maint` 口令由向导生成（`crypto/rand`）；`lingedge_migrate` 口令由安装者指定或自动生成。**(B) 预建账号模式**（DBA/宝塔已建好）：安装者在表单里给出 app、migrate 口令（maint 口令可选）。两种模式下向导都做版本/触发器/GRANT OPTION 预检（用 migrate 凭据，内存），随后**立即写** `runtime-credentials.json` 的 `database` 段（app/maint）。管理员口令此时清零。
2. 步骤 4 用内存中的 migrate 口令执行迁移，结束清零。若步骤 2 与 4 之间进程重启，内存凭据丢失，见 §6。
3. `complete`：事务提交 → 写 `system_installations`、`install.lock`（最后）→ 删除 setup token 哈希。若 migrate 口令是**自动生成**的，响应在 `SetupStepResult.one_time_secrets.migrate_password` 中**仅返回一次**（`Cache-Control: no-store`；该字段 `readOnly`，被 `pkg/redact` 视为敏感；不入访问日志/审计；响应写出后服务端清零）。页面同时给出 `credential init` 的预填命令；若操作者未保存，之后一律走 `credential reset`（需管理员口令），不存在「找回」途径。(B) 模式或口令由安装者自己指定时不返回。
4. 完成后进程内无任何 migrate/admin 口令；`install.lock` 之后 `/public/setup/*` 全部 410（不变）。

### 6. 中断续做：不落盘 ⇒ 重新提供

`system_installations.state` 可续做各步（不变），但**口令不会被恢复**：
- 续做时若 `database` 段已在 `runtime-credentials.json`，app/maint 口令直接读回，不再询问。
- 续做进入需要 migrate 凭据的步骤（预检/步骤 4）而内存中没有时，向导要求：(i) 重新输入 migrate 口令（若已知），或 (ii) 重新提供临时管理员凭据，向导对 `lingedge_migrate` 执行 `ALTER USER … IDENTIFIED BY <新口令>`（该账号此前只由本向导创建、尚无 root 侧文件，重置无副作用；若 `migrate.yaml` 已存在则告警并要求操作者随后 `credential init --force`）。拒绝「从某处悄悄取回」。
- 管理员凭据同理：续做若还需建/改账号则重新输入。
- `--from-file` 路径的续做：重新提供（新的或原样保留的）YAML，见 §7。

### 7. 无头路径 `lingedge-server setup --from-file <YAML>`

- 文件须为调用者属主、权限 **0600**（`O_NOFOLLOW`+`fstat`，非 0600/非普通文件/他人属主一律拒绝），否则退出码 3。
- 可含：`database.host/port/name/tls`、`database.admin.{user,password}`（临时管理员，可选）、`database.migrate.password`（可选，缺省自动生成）、`database.app.password`/`database.maint.password`（预建账号模式）、`redis.*`、管理员账号初始口令、站点基础配置。口令字段也可写成 `*_file`（间接指向另一个 0600/0400 的文件），供与密码管理器集成。
- **不残留**：读入并解析后，进程立即对内存缓冲清零，**并用 0 覆盖后 `unlink` 该文件**（尽力而为；日志/COW 文件系统上的残留风险在文档中说明）；删除失败 → 醒目警告并列出路径、`setup` 结果 JSON 带 `from_file_residual=true`，不静默。注意文件在**首次成功解析后**即清理，因此续做必须重新提供（与 §6 一致；不保留「为续做而留存」的选项）。
- 自动生成的 migrate 口令在无头路径下**写入 stdout 的结果块一次**（`--json` 时在 `one_time_secrets`），默认不落盘；同样提示 `credential init`。`--migrate-password-out <path>`（root 0400 的新文件）**不提供**——写 root 属主文件应由 root 侧 `credential init` 完成。

### 8. 轮换

- **app / maint 口令**：root 侧命令 `lingedge-server db credential rotate --role app|maint --admin-user U`（交互管理员口令；需 root，因要读 `master.key` 与 `chown`）：生成新口令 → `ALTER USER … IDENTIFIED BY <新> RETAIN CURRENT PASSWORD`（MySQL ≥8.0.14 双口令，满足 ≥8.0.16 前置）→ 原子重写 `runtime-credentials.json`（`chown lingedge:lingedge 0600`）→ 重启/重载 Server（M1 用 `systemctl restart`，热重载列 Deferred）→ 健康检查通过 → `ALTER USER … DISCARD OLD PASSWORD`。任一步失败不执行 DISCARD，旧口令仍可用，命令可重入。
- **migrate 口令**：`credential reset --admin-user U`（§4）。可定期执行；无服务停机。
- **管理员临时凭据**：不在系统内保存，无轮换；安装者应在向导结束后自行删除该临时账号（完成页提示）。
- **主密钥（KEK）轮换**：`runtime-credentials.json` 不在数据库，**不被 `RecordStore` 扫描**；`secrets.rewrap` 须把它作为额外目标重包装（原子重写），且 `CheckKEKRemovable` 把该文件的 `kek_id` 计入「仍被引用」。ADR-011 修订记录同步。
- 备份：`runtime-credentials.json` 位于 `/var/lib/lingedge/server/` 随服务数据备份（密文），`kek_ids_required` 须包含其 `kek_id`；恢复到较旧备份后若口令已轮换，运行 `credential rotate` 或 `reset` 对齐。

### 9. 宝塔（ENV-BT）差异

- 仍只创建/使用 `lingedge_*` 专用库与账号，不触碰其他库。推荐 (A) 模式：把宝塔 MySQL 的 root（或具备 `CREATE USER`、`GRANT OPTION`、全局 `SET_USER_ID` 授权能力的账号）口令仅在向导内存里用一次。
- (B) 模式（用宝塔「数据库」面板预建）：面板建出的账号通常只有库级权限、无 `GRANT OPTION`/`SET_USER_ID`——向导的预检（ADR-016 决策 6 的 GRANT OPTION 探针与 ERROR 1419 探针）会阻断并给出授权指引；向导不自行改 MySQL 全局变量。**上述宝塔默认权限与连接方式（TCP/socket 及 `lingedge` 用户能否访问 socket）为推断，须在 ENV-BT 验证**，未验证前 1.24 的宝塔用例标「等待环境验证」。
- binlog 常开、`log_bin_trust_function_creators=0` 的处置不变（tools-design §13 步骤 2）。
- 口令落点与本机原生部署相同；宝塔的 MySQL 若不在本机，host 为远端地址，`tls` 取值同 `server.yaml`（不允许跨主机明文，沿用 `db.Config` 既有校验）。

### 10. 日志、审计、传输脱敏

- 禁止出现口令的位置：应用日志、错误信息（`ErrorWithKeyPath` 风格，只含键路径）、HTTP 访问日志与请求体、`audit_logs.change_summary`（审计只记「创建账号 lingedge_app/maint/migrate」「迁移账号口令已自动生成：是/否」「运行期凭据文件已写入」，不含口令、哈希或长度）、`system_installations.state`、`state.json`/journal、systemd 环境、命令行参数（`ps`）。
- 依据：`pkg/redact` 已按键名遮蔽 `*password*`；新增键名 `one_time_secrets`、`migrate_password`、`admin_password` 的覆盖由 `G-5` 日志扫描用例保证（注入已知口令后断言所有输出无该串）。
- 凭据在 SQL 中的出现：`CREATE USER/ALTER USER … IDENTIFIED BY '<口令>'` 的字面量经唯一的转义函数构造，语句文本不得进入任何日志/错误（含驱动错误，须先脱敏）。不依赖 MySQL 通用日志对口令的处理（`log_raw` 属运维面，`doctor` 可提示，P2）。
- **传输**：携带管理员/migrate/DB/Redis 口令的向导接口，在监听非回环地址且请求不是 TLS（直接 TLS 或来自可信代理的 `X-Forwarded-Proto: https`）时拒绝：`400 setup.insecure_transport`（回环监听不受限，沿用 SSH 隧道默认）。含 `one_time_secrets` 的响应 `Cache-Control: no-store`。
- 测试：固定口令扫描（NFR-6）、日志扫描（注入口令）、审计内容断言、`runtime-credentials.json` 无明文口令的转储扫描、权限/属主/符号链接拒绝、`--from-file` 清理断言、`complete` 后进程内存无口令（best-effort：以 `Secret` 已 `Zero` 的接口断言，不做内存取证）。

### 11. 与过渡约定的迁移路径（定稿后只改这些文件）

| 过渡形态 | 现状位置 | 定稿后 |
|---|---|---|
| `LINGEDGE_MIGRATE_DB_USER/PASSWORD/PASSWORD_FILE`（Story 1.6） | `internal/platform/migrate/cli.go` | **保留**（最高优先级的显式来源，测试与 `make migrate` 继续使用）；在其后追加 `migrate.yaml` 与交互输入两级来源，新增 `credential init/reset` 子命令；root 运行时对配置/凭据文件属主与权限校验 |
| `database.maint.*`（`user`/`password`/`password_file`，Story 1.15） | `internal/platform/config/config.go`、`internal/platform/db/config.go`、`internal/platform/server/{server,scheduler}.go`、`cmd/lingedge-server/cron.go` | **保留**为显式覆盖通道（不删键，向后兼容）；缺省口令改由 `runtime-credentials.json` 提供；`Maint.Configured()` 判定改为「合并后」判定；`config` 的「口令不得写在 YAML」规则不变 |
| 「待 AA-7」注释/文档 | 见《实现指引》 | 1.24 随改随清，改为引用 ADR-026 |

契约：`SetupDbConfig` 增加可选字段、`SetupStepResult` 增加 `one_time_secrets`、新增错误码 `setup.insecure_transport` 均为**加法**，登记为 `docs/bmad/contract-changes/CC-11-setup-credential-fields.md`。`server.yaml` 新键 `secrets.runtime_credentials_file`（可选，缺省有值）同属该 CC（配置键非 OpenAPI/proto，但 1.15 已约定定稿变更走 contract-changes）。无迁移、无 proto 改动。

## Binds / Prevents / Rule

- 约束：Story 1.24（向导后端、`setup --from-file`）、1.35（Tools 安装联调）、1.43（升级）、1.15 之后所有读取 DB 凭据的代码、运维文档。
- 防止：向导需要 root 才能写的文件；`lingedge_migrate` 口令常驻 `lingedge` 可读位置或在日志/审计/状态里残留；root 侧命令信任 `lingedge` 可写的连接目标而被套取口令；运行期口令只能靠明文环境变量/文件；续做时「悄悄」从某处取回口令；备份泄露明文口令。
- 规则（AD-26）：**migrate 口令只在内存（向导期）或 root:root 0400 文件（root 侧）；app/maint/Redis 口令只以 secretbox 信封加密落在 `lingedge` 属主 0600 的 `runtime-credentials.json`；向导进程绝不持久化 migrate/admin 口令；root 侧 migrate 的连接目标绝不取自 `lingedge` 可写文件。**

## 备选方案与权衡

- **epics 建议默认：root:lingedge 0440 secrets 文件 + `LoadCredential`**：否决（见 §1 冲突说明）——向导无权创建，需新增 root 侧「提权落盘」通道（如 `.path` 单元，类比 ADR-024 的复杂度）并破坏无头单进程流程；0440 对密文并无额外收益。
- **把 app/maint 口令放进数据库（`system_settings.value_enc`）**：否决——连库本身就需要口令（鸡生蛋）。
- **口令明文写 `lingedge` 属主的 `password_file`（现有通道）**：保留为显式覆盖，不作默认——数据目录备份会带出明文。
- **向导把 migrate 口令交给 root：完成页引导 + root 侧 `credential init`（采纳）** vs 常驻 `lingedge` 可读文件（否决，CC-3 同理）vs 每次升级都要管理员口令（否决：升级体验差，且仍需第二套凭据）。
- **向导完成后锁定 `lingedge_migrate`（`ACCOUNT LOCK`）、升级时解锁**：暂不采纳（升级流程需管理员凭据；收益是缩小窗口）；列入 Deferred，可作为加固选项。
- **migrate 口令加密存盘（secretbox）**：否决——KEK 与文件同在 root 域，无增益。

## 后果

- 新增：`runtime-credentials.json` 读写库（建议 `internal/platform/dbcred`，仅依赖 `pkg/secretbox` 与 `config`）、`migrate credential init|reset` 与 `db credential rotate`、`SetupDbConfig` 的加法字段。
- 操作者必须在向导结束后执行一条 root 命令保存 migrate 口令（或之后用 `reset`）；这是有意的权衡，文档与完成页给出预填命令。
- `secrets.rewrap` 与备份清单需覆盖该文件（ADR-011、tools-design §17.1 已回写）。
- 重启才能应用新口令（M1）；热重载 Deferred。

## Deferred

- 热重载运行期口令（SIGHUP）；P3 角色拆分后的每角色独立凭据文件；`lingedge_migrate` 升级窗口外 `ACCOUNT LOCK`；外部 KMS/Vault 提供凭据（走 ADR-011 的 KeyProvider 扩展点）；`doctor` 对 `log_raw` 的提示。

## 可被推翻处（UD-31）

默认值与既有 ADR/实现无冲突，除上文已说明的「0440 root 文件」替换。若用户推翻：
- 要求 app/maint 口令放 **root 属主文件**：须新增 root 侧交接通道（Tools 命令或 systemd `.path`），向导的 `database`/`redis` 段改为「暂存 + root 落盘」两阶段，1.24 AC3/AC7、1.35 增加提权环节与测试；`--from-file` 需由 root 运行。
- 要求 **migrate 口令不经完成页展示**（只允许安装者自定）：删 `one_time_secrets` 与自动生成分支，CC-11 相应缩减。
- 要求 **升级全程不保存 migrate 口令**（每次交互）：`migrate.yaml` 变为可选，`credential init` 降级为可选；1.43 非交互升级需 `LINGEDGE_MIGRATE_DB_PASSWORD_FILE`。
- 影响面：1.24（AC3/AC7 与新增 AC）、1.35（联调）、1.43（凭据来源）、CC-11；不影响迁移与已完成的 1.1–1.23。

## 实现指引（Story 1.24 开发触及面；2026-11-21 对代码的 grep 自检）

**现有临时处理（grep：`AA-7`、`database.maint`、`LINGEDGE_MIGRATE_DB_*`、`MaintConfigFrom`/`AppConfigFrom`）**

| 文件 | 临时处理 | 1.24 动作 |
|---|---|---|
| `internal/platform/migrate/cli.go`（`EnvUser/EnvDBPass/EnvDBPassFile`、usage 文案） | 凭据只来自环境变量/口令文件 | 追加 `migrate.yaml`、交互输入、`credential init/reset`；目标来源信任规则（§4）；改注释引用 ADR-026；更新 `cli_test.go` |
| `internal/platform/config/config.go`、`secretfile.go`（`DatabaseMaint`、`readSecretFile` 注释「AA-7 建议形态」、`Parse` 的 YAML 口令拒绝、`Validate` 的 maint 规则） | maint 暂行形态；口令文件通道 | 新增 `secrets.runtime_credentials_file`；`Maint.Configured()`/校验改为面向合并后配置；删除「暂行」措辞；保持显式通道向后兼容；`config_test.go` 增例 |
| `internal/platform/db/config.go`（`AppConfigFrom`、`MaintConfigFrom` 注释「口令落点见 AA-7」） | 仅映射 server.yaml | 改为接受合并后的配置（或新增 `…FromRuntime`）；注释更新 |
| `internal/platform/server/server.go`（装配 `db.OpenApp`）、`scheduler.go`（`d.Maint.Configured()`） | 直接读 `cfg.Database` | 装配阶段先解密 `runtime-credentials.json` 填入 `Secret`，再开池；maint 未配置仍明确跳过并告警 |
| `cmd/lingedge-server/cron.go`（`MaintConfigFrom`）、`lineagecmd.go`（`AppConfigFrom`） | 同上 | 同一合并入口，避免各子命令各自读文件 |
| `internal/platform/signingkeys/repository/repository.go` 头注释 | 「AA-7 …未决」 | 仅改注释；无行为变化（只用 app 池） |
| `Makefile` 第 133 行附近、`docs/architecture/dev-environment.md` §make migrate、`docs/database/README.md` §5.1 入口行 | 描述过渡约定 | 保持有效；追加「定稿见 ADR-026」 |
| `tests/migrations/{cli_test,faults_test}.go`、`tests/integration/server/{process_test,builtin_jobs_test}.go` | 用 `migrate.EnvDBPass`/`LINGEDGE_MIGRATE_DB_*`/`MaintConfigFrom` | 保持通过（显式通道保留）；新增真实 MySQL 的向导用例 |
| `configs/server.example.yaml`（`database.maint` 注释） | 说明口令只经环境变量 | 补充「缺省由向导写入 runtime-credentials.json」 |

**新增（建议位置）**：`internal/platform/dbcred`（文件读写/校验/AAD、原子写、清零）；setup 服务（向导）与 `setup --from-file` 解析清理；`cmd/lingedge-server` 增 `migrate credential`、`db credential rotate`；`gates`/测试：migrate 池仅 setup 窗口可构造、`runtime-credentials.json` 转储无明文、日志注入扫描；`secrets.rewrap` 目标登记（`tests/integration/secrets` 的 `TestTargetsMatchMigrations` 不受影响，因文件不在迁移里，另加文件目标测试）。

**测试要求汇总**（并入 1.24）：见 §10 末尾；另加「续做重新提供」「预建账号模式」「建账号模式」「宝塔（等待 ENV-BT）」「root 侧不读 runtime-credentials」「`credential init` 预检失败不写文件」。

## 验证

- 真实 MySQL（本机，临时 mysqld，非常驻实例）：建账号模式与预建账号模式各一遍；`RETAIN/DISCARD CURRENT PASSWORD` 轮换；`migrate credential init/reset`；§10 的安全断言。
- 真实 systemd 下 `lingedge` 用户写 `/var/lib/lingedge/server/secrets/`、root 写 `/etc/lingedge/server/migrate.yaml`：ENV-LNX（等待环境验证，汇总于 1.45）。
- 宝塔：ENV-BT。

## 实现记录（Story 1.24，2026-11-21）

按本 ADR 与「实现指引」落地；与文字描述相比的**实现细节与偏差**（均不改变决策）：

- **向导进度的落点**：`system_installations` 在冻结迁移 000002 里没有独立的 `state` 列，进度存于其 `meta_json["setup"]`（步骤名/布尔/版本号/公开配置 `base_url`/「迁移口令是否自动生成」，**不含任何口令、哈希或其长度**）；进行中行 `status='upgrading'`，完成时置 `installed` 并写 `install_locked_at`。无需新迁移（CC-12 记录）。续做时 database/redis 两步由 `runtime-credentials.json` 的对应段推导，init_db 由 `schema_migrations` 推导，其余由进度行推导。
- **口令字段在内存里是 `[]byte`**（migrate 口令，用后 `secretbox.Zero`）；请求体由 `encoding/json` 解码成的 `string` 副本无法就地清零（Go 字符串不可变），实现只做到「请求结构体字段置空、不持有引用」，并在文档中如实说明（ADR-026 §10 已声明不做内存取证）。
- **步骤审计与 audit_logs 的先后**：迁移完成之前 `audit_logs` 不存在，`checks/database/redis` 三步的审计事件在内存缓冲，于 `initialize` 完成后与进度行写入**同一事务**补写（Actor=系统组件 `setup`）；进程在补写前退出则这些事件丢失（同时有不含口令的结构化运行日志）。其后各步的审计与业务写入同一事务（fail-closed）。
- **setup token 哈希删除**：`/etc/lingedge/server/` 属主是 root 时 `lingedge` 无权 `unlink`——删除失败则退而清空文件内容（属主可写；空文件对校验是非法格式，fail-closed），并提示手工删除；结果消息与日志都会说明。
- **退出码**：`migrate` 非 TTY 且无任何口令来源时退出码取 `3`（`ExitPrecondition`，与 ADR §4、Tools 退出码表一致；与 `lint` 未通过共用数值 3，二者不会在同一命令里同时出现）。
- **账号主机部分**固定 `'%'`（迁移 SQL 里不带主机的 `GRANT` 等价于 `'%'`）；`lingedge_app`/`lingedge_maint` 的账号名固定（迁移 lint 只允许授权给这两个名字），请求里的 `username`/`maint_username` 若不是它们则 400。授予 `lingedge_migrate` 的库级权限按字面库名转义下划线（`db\_name`），避免 `_` 通配到别的库。
- **`db credential rotate` 的重启/健康检查**：Server 没有提权/重启通道，命令分两阶段（阶段一 `RETAIN CURRENT PASSWORD` + 重写文件；阶段二 `--finish` 执行 `DISCARD OLD PASSWORD`），中间由操作者（或 Tools 1.43）重启并确认健康。
- **未实现/等待环境验证**：真实 systemd 下 `lingedge` 用户写数据目录、root 写 `/etc/lingedge/server/migrate.yaml` 的属主/权限路径（ENV-LNX）；宝塔 MySQL 的默认权限与 socket 可访问性（ENV-BT）；MySQL 8.0.16 最低版本与 Redis <8 的真实拒绝路径（本机只有 8.0.46 与 Redis 8.x，版本判定逻辑与 `INFO server` 解析均有构造响应的单测）；socket 连接（运行期凭据文件只存 host/port，`config.Database` 没有 socket 键，Deferred）。
