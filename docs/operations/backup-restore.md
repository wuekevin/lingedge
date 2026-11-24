# 备份、恢复与卸载运维手册（Story 1.41；FR-10、NFR-18）

设计与取舍见 `docs/architecture/tools-design.md` §17.6；本文只讲怎么用、出错怎么办。命令都需要 root（`sudo lingedge …`，Tools 不自提权）。

## 1. 先记住两件事

1. **数据库与私钥是密文，恢复必须有对应的 `master.key`。** 备份默认不含 `master.key`，请把它与数据库备份**分开保管**（不同介质/不同人）。每次 `backup create` 成功后都会醒目提醒；`lingedge install server` 输出的「主密钥指纹」与备份清单里的 `master_key_fingerprint` 是同一个值（`sha256sum /etc/lingedge/server/master.key` 的前 16 位）。
2. **没有 `master.key` 的备份只能恢复配置，恢复不了数据库里的加密内容**（证书/签名私钥、运行期凭据、TOTP 密钥）。恢复时 Tools 会先比对指纹，不匹配直接拒绝并说明，**此时尚未改动任何东西**。

## 2. 创建备份

```bash
# 数据库凭据：迁移账号口令（不要写在命令行里）
export LINGEDGE_MIGRATE_DB_PASSWORD=…          # 或 LINGEDGE_MIGRATE_DB_PASSWORD_FILE=<0400 文件>，或事先 sudo lingedge-server migrate credential init，或交互输入
sudo -E lingedge backup create --component server --encrypt --passphrase-file /root/backup.pass
```

- 数据库连接目标缺省取 `/etc/lingedge/server/migrate.yaml`（`migrate credential init` 写入）；也可 `--db-host/--db-port` 或 `--db-socket`，加 `--db-name`。口令只能来自环境变量、口令文件、`migrate.yaml` 或终端交互，**永远不是命令行参数**；Tools 把它写进 0600 的临时 `--defaults-extra-file`（0700 暂存目录），用后立即清零并删除。
- 输出目录缺省 `/var/lib/lingedge/backups`（root 0700）。`--output` 指到别处时：未加密备份要求该目录仅属主可访问，否则拒绝。
- **不加密**：文件 0600、会警告——备份里有数据库内容（敏感值为密文）与配置，离机存放前请加密。`--encrypt` 用 argon2id 派生密钥 + AES-256-GCM 分块；口令至少 10 个字符，来自 `--passphrase-file`（root 属主 0400/0600）、环境变量 `LINGEDGE_BACKUP_PASSPHRASE_FILE` 或交互输入（两次）。**口令丢失 = 备份不可恢复**，没有后门。
- `--include-master-key`、`--include-migrate-credential`（`migrate.yaml`）、`--include-identity`（node 身份私钥）**必须同时 `--encrypt`**，否则退出码 2。把主密钥放进备份会削弱「分开保管」，仅在你有可靠的口令保管时使用。
- 默认排除：`master.key`、`migrate.yaml`、`setup-token.hash`、node 的身份私钥与 staging；日志默认不含（`--include-logs`）。
- `--dry-run`：只列出会备份的文件，不写任何东西。

```bash
sudo lingedge backup list                       # 无需口令；展示每个备份所需的 kek_id、主密钥指纹、schema 版本、deploy.lineage
sudo lingedge backup verify <文件> [--passphrase-file …]   # 完整校验结构、SHA-256（加密备份同时解密认证）；只读
```

**请定期演练 `backup verify` 与恢复**：不可读的备份等于没有备份。

## 3. 恢复

```bash
sudo -E lingedge backup restore <文件> [--passphrase-file …] [--replace --confirm-name $(hostname)]
```

顺序：完整校验备份 → 只读前置检查（拒绝时**一切原样**）→ **恢复前安全备份** → 停服务 → 恢复文件与数据库 → 用恢复后的主密钥解密一条探针记录 → `lingedge-server lineage bump` → 启动并健康检查。安全备份之后任何一步失败都会自动回滚到安全备份（文件、数据库、`master.key`、服务状态），退出码 10；回滚也失败退出码 12，journal 保留，`sudo lingedge recover --rollback` 重试（需要同样的数据库凭据；若恢复前安全备份是加密的，还需 `LINGEDGE_BACKUP_PASSPHRASE_FILE`，`recover` 没有命令行标志，数据库目标走 `LINGEDGE_MIGRATE_DB_HOST/PORT/SOCKET/NAME`）。

| 退出码 | 含义 |
|---|---|
| 3 `precondition.master_key_mismatch` | 本机 `master.key` 缺失或与备份的 `kek_id` 指纹不一致 → 见 §4 |
| 3 `precondition.schema_newer` | 备份的 schema 比当前二进制新 → 先 `lingedge update server` 再恢复 |
| 3 `precondition.db_not_empty` | 目标库非空 → 确需覆盖加 `--replace`（再要求 `--yes --confirm-name <主机名>` 或交互键入主机名，否则退出码 9） |
| 3 `precondition.passphrase_required` / `db_credentials` | 缺口令/数据库凭据（非交互）→ 见 §2 |
| 5 | 备份损坏、被篡改或口令错误 |
| 10 / 12 | 失败已回滚 / 回滚失败需人工 |
| 13 / 14 | 存在未完成事务（`recover`）/ 组件未安装（先 `lingedge install`，restore 只恢复配置与数据，不装二进制） |

- **恢复后的 lineage**：恢复到更早的备份后，Tools 在启动 Server **之前**执行 `lingedge-server lineage bump`（`deploy.lineage = max(恢复后 DB 值 + 1, 当前 UNIX 秒)`），使恢复后产生的修订、站点 epoch、URL 失效标记按 `(lineage, …)` 字典序严格大于节点已应用值——节点仍能接收新修订。请保证本机时钟没有倒退（`doctor` 的 `sys.time_sync` 属 Story 1.42）。
- **`runtime-credentials.json`**：本机已有且仍能被主密钥解开时保留本机文件（它与当前 MySQL 账号口令一致）；否则用备份里的。恢复后若口令与 MySQL 账号不一致，`lineage bump` 会失败并整体回滚——先 `lingedge-server db credential rotate …` 对齐。
- **`state.json` 不会被覆盖**（它反映本机已装的二进制）；备份里只作参考。恢复只覆盖备份里有的文件，不删除备份里没有的文件。
- 恢复前安全备份会保留（`backup list` 可见，用途 `pre-restore`）；确认无误后自行删除。需要撤销这次恢复，就用它再 `restore` 一次。

## 4. 主密钥不匹配时怎么办

Tools 逐个核对备份所需的 `kek_id`：当前 `master.key` 必须有它且指纹相同（备份之后**新增**了 KEK 没关系）。不匹配时：

1. 从保管处取回备份创建时对应的 `master.key`（核对 `backup list` 显示的指纹）。
2. `sudo lingedge backup restore <文件> … --master-key-file <取回的 master.key>`：指纹匹配才会安装；**本机现有的 `master.key` 保留为 `master.key.pre-restore-<事务>`（0400）**，Tools 绝不删除任何主密钥——请核对后自行归档。
3. 备份是以 `--include-master-key` 创建的：用 `--use-backup-master-key`。

## 5. 轮换 KEK 与旧备份

新增 KEK、`secrets.rewrap` 完成后，旧 KEK 只有在**没有保留备份需要它**时才能移除（`backup list` 的「所需 kek_id」）。需要它的备份在没有旧主密钥时无法解密；确要放弃兼容须显式 `--drop-backup-compat` 并知会相关人员。守卫函数 `backup.Service.GuardKEKRemoval` 已就绪，执行移除的命令随 Story 6.18 交付——在此之前移除 KEK 是手工编辑 `master.key`，请先 `backup list` 核对。

## 6. 卸载

```bash
sudo lingedge uninstall <组件> --dry-run          # 看会删除/保留什么，零改动
sudo lingedge uninstall <组件> --yes              # 默认：保留配置、数据与备份
sudo lingedge uninstall <组件> --purge --yes --confirm-name $(hostname)   # 连同配置、数据与该组件的备份一起删除
```

- 默认只摘除运行时：停止并停用 systemd 单元、删除单元文件、摘除 Nginx `conf.d` 软链（`nginx -t` 通过才 reload）、删除 `/opt/lingedge/<组件>`、从 `state.json` 移除。运行用户、数据库、`/etc/lingedge/<组件>`、`/var/lib/lingedge/<组件>`、备份都保留；重新 `install` 会沿用它们。
- 静态前端（admin-web/console/docs/status/website）**没有数据目录**；其 Nginx 片段（`/etc/lingedge/nginx/<c>.conf`）默认保留，`--purge` 只删本组件的两个文件。若你在 Nginx/宝塔里手工 `include` 过它，请自行移除该 include。
- **`--purge` server 会删除 `master.key`**（计划里有醒目警告，附指纹）：没有它，既有数据库备份永久无法解密。请先确认已另存。`--purge` 不删除数据库（Tools 从不 DROP 你的数据库）。
- `--purge` 必须给出主机名确认：交互键入，或 `--yes --confirm-name <主机名>`；否则退出码 9。
- 卸载中途失败：报告 `partial`，各步骤幂等，重新运行同一条命令即可完成；进程被杀后先 `lingedge recover --rollback`（它只会结束事务记录并提示重跑 uninstall）。

## 7. 等待环境验证

真实 systemd 下的服务停启、root 属主的真实 chown 与文件属主检查、真实宝塔 MySQL 的客户端路径（`--mysql-bin-dir`）与权限、node 的真实数据布局与 Agent 心跳、全新机器上的灾后重装恢复演练，均在 ENV-LNX/ENV-BT 验证前标「等待环境验证」（汇总于 Story 1.45）。
