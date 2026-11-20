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

`RecordStore`（按 `enc_key_id` 扫描各 `*_enc` 列、CAS 写回、计数）由持有这些列的仓储层实现；后续 Story 接线 `retention`/`rekey` 任务与 `lingedge … backup list`。本 Story 用内存实现验证原语语义，**未**做真实库表的接线（见 Story 1.11 开发记录）。

## 4. 进程内保护

密钥材料一律 `[]byte`，使用后 `Zero`；`FileKeyProvider.Close` 清零全部 KEK。已知限制：`crypto/aes` 展开的轮密钥由 Go 运行时持有，无法清零；因此 systemd 单元必须 `LimitCORE=0`。

## 5. 记入 Tools 渲染清单（Story 1.32）的要求

- `lingedge-server.service` 必须含 `LimitCORE=0`（ADR-011 §9；tools-design §15.1 已有，1.32 的 golden 与加固项检查必须保留）。
- `LoadCredential=master.key:/etc/lingedge/server/master.key`；原文件 `root:root 0400`（Tools 步骤 18 生成）。
- 不得用环境变量传递主密钥。

## 6. 等待环境验证

- 真实 systemd 下的 `$CREDENTIALS_DIRECTORY`（凭据文件的实际属主/权限由 systemd 设定）与 `root:root 0400` 原文件经 `LoadCredential` 的端到端启动；本机（macOS、无 systemd、非 root）仅验证了权限、属主判定逻辑（以允许 uid 集合注入）与路径解析。
- 以 root 身份运行时 `root:root 0400` 的接受路径（本机以非 root 运行，只能用「属主=运行用户」覆盖同一判定逻辑）。
- `LimitCORE=0` 在 Linux 单元中的实际生效（属 Story 1.32 渲染与验收）。
