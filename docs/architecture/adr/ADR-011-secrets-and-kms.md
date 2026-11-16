# ADR-011 密钥与证书私钥加密存储（信封加密）与主密钥（KMS 抽象）

- 状态：已接受（架构代理非交互决策）｜日期：2026-11-16｜关联：基线 §14、§24、§4.8；PRD NFR-6、NFR-12、NFR-13、FR-25、FR-85、OQ-8、反馈 F-7；architecture AD-11｜阶段：M1

## 背景

基线 §24：敏感凭据加密保存、日志脱敏；§14：证书私钥加密存储、按站点授权。需要保管的秘密：证书私钥、Provider/SMTP/Telegram/支付凭据、节点 mTLS CA 私钥、配置签名私钥、TOTP 密钥、会话/API Token（哈希）。栈内无外部 KMS（MySQL/Redis），OQ-8 要求由架构提出主密钥保管方案。

## 决策

1. **信封加密**：`AES-256-GCM`。每个被保护值使用**独立随机 DEK（32B）**加密，DEK 再由 **KEK**（主密钥派生）加密存储；密文格式 `v1:<kek_id>:<base64url(wrapped_dek)>:<base64url(nonce)>:<base64url(ciphertext+tag)>`，AAD = `表名|列名|记录 uid`（防密文被搬到别的记录/列）。列（以 docs/database 为准）：`certificates.private_key_enc`、`acme_accounts.private_key_enc`、`signing_keys.private_key_enc`、`provider_credentials.credential_enc`（通知渠道/DNS Provider 等的凭据均在此，`notification_channels` 无独立加密列）、`users.totp_secret_enc`、`system_settings.value_enc` 统一用 `pkg/secretbox` 封装，**一律使用上述 5 段格式**（数据库文档此前的 4 段写法已回写）。各表另保留 `enc_key_id` 列 = 当前包裹 DEK 的 `kek_id`，仅用于 `secrets.rewrap` 的扫描与 KEK 移除前的引用核对，解密以密文内的 `kek_id` 为准。
2. **KeyProvider 接口**（KMS 抽象）：
   ```go
   type KeyProvider interface {
       CurrentKeyID() string
       Wrap(ctx, kekID string, dek []byte, aad []byte) ([]byte, error)
       Unwrap(ctx, kekID string, wrapped []byte, aad []byte) ([]byte, error)
   }
   ```
   M1 默认实现 `FileKeyProvider`：主密钥文件 `/etc/lingedge/server/master.key`（**`root:root 0400`**，含一个或多个 `kek_id → 32B 随机值`；**由 Tools 在安装步骤 18 用 `crypto/rand` 生成**——Web 向导不生成、不接触该文件，tools-design §13），经 systemd `LoadCredential=master.key:/etc/lingedge/server/master.key` 提供给进程（进程读取 `$CREDENTIALS_DIRECTORY/master.key`，运行用户 `lingedge` 无法直接读原文件）；**主密钥不入库、不入日志、不入备份产物**（与数据库备份分开，F-7）。接口预留外部 KMS/HSM/云 KMS 插件（P5 插件体系，实现时选型）。
3. **轮换**：`kek_id` 版本化；新增 KEK 后新写入使用新 KEK；`retention`/`rekey` 任务（`secrets.rewrap`）后台重新包装 DEK（只重加密 DEK，不重加密大数据）；旧 KEK 在全部记录重包装后才可移除，**且移除前必须核对仍被保留备份需要的 KEK**：备份清单记录 `kek_ids_required`（tools-design §17.1），`backup list` 展示；被某个保留备份需要的 KEK 拒绝移除，除非显式 `--drop-backup-compat`（此后这些备份在不带旧主密钥时不可解密，MIN-05）。恢复旧备份与 KEK 移除相冲突的场景因此在移除时被拦截，而不是在恢复时才发现。DEK 泄露只影响单条记录。
4. **三个签名/身份密钥的保管**：
   - 配置修订签名私钥（Ed25519）与节点 mTLS CA 私钥：存 `signing_keys` 表（`private_key_enc` 信封加密，状态 `active|retiring|revoked`），仅在签名/签发时解密到内存，使用后清零；
   - **Server gRPC 叶证书私钥**（:9443/:9444 的 TLS 服务端证书，由同一私有 CA 签发，ADR-003）：同样信封加密存 `signing_keys(purpose='grpc_server', cert_pem, private_key_enc)`，进程热重载，Scheduler 任务 `server.cert_rotate` 在 2/3 周期轮换；
   - **发行清单签名私钥不在 Server**：离线保管（ADR-012），Server 与节点不持有。
5. **API Token / 会话令牌**：高熵随机（≥32B），库内仅存 `SHA-256`（慢哈希无必要）；明文只在创建时展示一次；日志不记录（ADR-018）。**用户密码**：argon2id（参数实现时按基准测试设定，`golang.org/x/crypto` v0.59.0 为查询值）。
6. **证书私钥的使用**：上传/签发时即加密入库；仅在 `FetchCertificate`（节点 mTLS）响应时解密并经 TLS 传给授权节点（节点属于该证书覆盖站点所在集群），每次取用写审计；节点落盘 `0600`、属主 `lingedge-node`；任何 REST 响应永不回显私钥（创建后）；导出私钥不提供。
7. **日志与错误脱敏**：zap 自定义 `Redact` 字段类型与 JSON 编码器钩子，按键名模式（`password|secret|token|private_key|authorization|cookie`）与已知格式（`le_…` API Token、`lenr1.…` enroll Token、PEM 私钥块）脱敏；panic/错误链中的值同样过滤；CI 日志扫描（NFR-12）。
8. **备份**：数据库备份含密文；主密钥另行备份（加密介质、双人保管说明，tools-design §17）；恢复流程在恢复后校验"能解密一条探针记录"，否则中止并提示缺少主密钥；备份清单记录 `master_key_fingerprint` 与 `kek_ids_required`（见 §3）。
9. **进程内保护**：密钥材料用 `[]byte` 并在使用后清零；不放入 `string`；`core dump` 在 systemd 单元中禁用（`LimitCORE=0`，已写入 tools-design §15.1 的 `lingedge-server.service` 模板，此前模板缺失该项）。
10. **失陷爆炸半径**：Server 进程被攻破 = 能解密所有记录并用 CA 签发新节点证书（威胁模型已列）。缓解：最小权限运行、systemd 加固、审计、签发速率限制与告警；高安全部署可替换 KeyProvider 为 HSM/KMS。

## Binds / Prevents / Rule

- 约束：所有存放秘密的列与服务、安装向导、备份/恢复、日志框架。
- 防止：明文存储凭据；主密钥随数据库备份一起泄露；密文被替换到他处；日志泄露 Token/私钥；不可轮换的密钥。
- 规则（AD-11）：**敏感值信封加密（DEK/KEK）、AAD 绑定记录；主密钥经 `KeyProvider` 提供，不入库、不入日志、不随数据库备份；轮换只重包装 DEK。**

## 备选方案与权衡

- **直接用单一主密钥加密所有值**：否决——轮换需全量重加密，且单点泄露影响面大。
- **把主密钥放环境变量**：否决——易经 `/proc`、崩溃报告、子进程泄露；采用文件 + systemd credential。
- **依赖云 KMS**：M1 否决——自部署场景不一定有；留接口。
- **数据库内置加密（MySQL TDE/`AES_ENCRYPT`）**：否决——密钥进入 SQL 层与日志、慢查询；不可移植。
- **Shamir 分片主密钥/启动人工输入**：对自动重启不友好；列为高安全选项（不在 M1）。

## 后果

- 备份/恢复流程多一份必须保管的主密钥，运维需培训（F-7 反馈）。
- 启动时必须能读取主密钥，否则 Server 拒绝启动（fail-closed）。
- 解密热路径有 CPU 成本；仅在 FetchCertificate/签名等低频路径使用，并带短时内存缓存策略（待评估，默认不缓存私钥明文）。

## 可被推翻处

主密钥保管方式（文件 → 外部 KMS/HSM）、argon2id 参数、是否缓存解密后的 DEK。

## 验证

- 数据库转储中所有敏感列为 `v1:` 密文（扫描测试）；AAD 搬迁测试（把 A 记录密文复制到 B 记录解密失败）。
- 日志扫描（注入含私钥/Token 的值后断言输出被脱敏）；固定密码/默认口令扫描（NFR-6）。
- 轮换测试：新增 KEK → 重包装任务 → 移除旧 KEK 后仍可解密；缺少主密钥时恢复被拒。

## 修订记录 2026-11-16

- **M15（密文格式/列名）**：§1 明确统一为 5 段格式 `v1:<kek_id>:<wrapped_dek>:<nonce>:<ct>`（`kek_id` 在密文内，数据库文档此前的 4 段写法回写为 5 段）；列名以数据库文档为准：`secret_enc`→`credential_enc`，删除不存在的 `notification_channels.config_enc`；保留 `enc_key_id` 列的用途说明。原因：内部评审 M15（ADR 与 DB 的密文格式与列名漂移）。
- **M16（master.key）**：§2 `master.key` 由 **Tools 步骤 18** 生成、`root:root 0400`、经 `LoadCredential` 注入（此前写“安装向导生成、root:lingedge 0440”与 tools-design §13/§15.1 矛盾）；§9 `LimitCORE=0` 已在 Server 单元模板落实。
- **MIN-05（KEK 移除 × 备份恢复）**：§3 旧 KEK 移除前须核对保留备份的 `kek_ids_required`，被需要时拒绝，除非 `--drop-backup-compat`；§8 备份清单记录 `kek_ids_required`。
- **M13 / IMP-05**：§4 补充 Server gRPC 叶证书私钥的保管（`signing_keys.purpose='grpc_server'`）。
