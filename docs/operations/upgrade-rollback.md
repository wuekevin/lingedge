# 升级与回滚（lingedge update / rollback / update self）

> 设计依据：tools-design §17.2–§17.4、§17.7；ADR-013、ADR-016；Story 1.43。**当前状态**：编排、决策表与回滚逻辑已在本机以真实 `lingedge-server` 进程 + 临时 MySQL 8.0 + 真实 SQL 迁移验证；真实 Linux/systemd/root 属主、宝塔、真实发行源与真实发布密钥（UD-3）下的演练属 ENV-LNX/ENV-BT（Story 1.45），在那之前闭环 J **不得整体标通过**。

## 1. 命令速查

| 命令 | 作用 |
|---|---|
| `lingedge update check [--channel stable\|beta]` | 经签名清单列出可升级组件与迁移预告（`classes[]`、`has_irreversible`）；只读，不要 root |
| `lingedge version --check` | 同上（附在 `data.check`） |
| `lingedge update <组件> [--dry-run]` | 升级一个组件；`--dry-run`/`--plan-only` 只输出计划，**不改任何东西** |
| `lingedge update --all` | 逐个升级所有已装组件（每个一个事务；某个失败即停止，已成功的保持） |
| `lingedge update <组件> --offline <bundle.tar>` | 离线包升级，**同一条验证链** |
| `lingedge update self` | 升级 `lingedge` 自身（原子替换，保留 `.prev`） |
| `lingedge rollback <组件> [--to V] [--from-backup F --confirm-name <主机名>]` | 回滚 |
| `lingedge recover [--rollback]` | 处理被中断的升级/回滚 |

常用标志：`--accept-irreversible`（非交互确认不可逆迁移）、`--no-backup`（仅非 server）、`--no-rollback`（健康失败时保留现场）、`--passphrase-file`（升级前备份加密口令；缺省取环境变量 `LINGEDGE_BACKUP_PASSPHRASE_FILE`，都没有则不加密）、`--db-host/--db-port/--db-socket/--db-name/--db-user/--db-tls`、`--mysql-bin-dir`（宝塔常见 `/www/server/mysql/bin`）。

## 2. 升级做了什么（13 步）

取锁并检查未完成事务 → 验签清单 → 兼容性（`min_upgrade_from`/`requires`/`conflicts`/系统/端口/资源）→ 计划（展示迁移级别、备份、将重启的服务）→ 下载并验证包、落地为 `releases/<新版本>` → **备份当前版本（server 含数据库，写完再校验可读）** → `migrate plan` → 停服务 → `migrate up` → 原子切换 `current` → 启动 → 健康检查（server：`/healthz`、`/healthz/ready` 且数据库迁移版本 = 期望）→ 确认并写 `state.json`（提交点），成功后才清理超出保留数（`tools.yaml` 的 `update.keep_releases`，缺省 2、不少于 2）的旧 release。

## 3. 迁移决策表

| 升级所跨越的迁移 | 备份 | 确认 | 失败时的回滚路径 | 对你的承诺 |
|---|---|---|---|---|
| 无迁移 | server 仍做 DB 备份 | 否 | 切回旧 release | 二进制回退即恢复 |
| 全部 `reversible` | 强制 | 否 | `migrate down` 到升级前 schema → 切回旧 release；**down 失败（含 dirty）→ 升级为「恢复备份」** | 二进制+库可一起回退 |
| 含 `expand` | 强制 | 否 | 切回旧 release（不执行 down） | **仅限升级失败的即时回滚**；新版本已运行并写入新结构之后的回退依赖 N-1 兼容性，不在承诺内 |
| 含 `irreversible` | **强制并验证可读** | **是**：交互键入 `confirm-irreversible`；非交互 `--accept-irreversible`，否则退出码 9（此时**尚未改动任何东西**） | **恢复升级前备份 + 旧版本二进制**（含启动旧 Server 前的 `lineage bump`） | **备份点之后产生的数据将丢失**；不承诺简单回退即可恢复 |
| 多类并存 | 取最严格 | 取最严格 | 取最严格 | 取最严格 |

迁移级别取「清单预告 ∪ 新二进制 `migrate plan` 的实际结果」中更严格者。

> **已知缺口（expand）**：现行 Server 的就绪探针在「库里有二进制不认识的迁移」时报告 `/healthz/ready` 503。所以含 `expand` 的升级失败回滚后，旧版本进程**存活但不就绪**；Tools 的回滚复核对该路径只要求存活探针 + schema 等于升级后版本（报告 `health: healthy_live_only`）。如需旧版本重新就绪，请改走 `rollback --from-backup`。真正的 N-1 兼容是后续工作。

## 4. 迁移凭据（口令永远不经命令行）

升级的 `migrate plan`/`migrate up`（以及回滚的 `migrate down`、备份/恢复）使用迁移账号 `lingedge_migrate`，来源优先级：**环境变量 `LINGEDGE_MIGRATE_DB_PASSWORD` ＞ 口令文件（`LINGEDGE_MIGRATE_DB_PASSWORD_FILE`，root 属主 0400/0600）＞ `/etc/lingedge/server/migrate.yaml`（root:root 0400，`lingedge-server migrate credential init` 写入）＞ 交互输入（仅 TTY，无回显）**。非交互且无任何来源 → 退出码 3，此时尚未改动任何东西。Tools 只经环境变量把口令传给 `migrate` 的直接子进程；口令不写入 journal、`state.json`、日志、systemd 环境或输出。连接目标取自命令行/环境变量、`migrate.yaml`、root 属主的 `server.yaml`，**绝不**读取 `runtime-credentials.json`。备份默认**不含** `migrate.yaml`；恢复后需重新 `migrate credential init`。

## 5. 失败与退出码

| 退出码 | 含义 | 你该做什么 |
|---|---|---|
| 0 | 成功（或已是最新） | — |
| 3 | 前置条件不满足（无迁移凭据、不可逆升级被要求简单回退、需要同时升级别的组件…） | 按提示处理；系统未改动 |
| 5 | 清单/包校验失败（篡改、过期、sequence 回退、吊销） | 不要绕过；换取可信来源 |
| 9 | 含不可逆迁移需要确认 / 覆盖数据库需要主机名确认 | 阅读承诺后确认；系统未改动 |
| 10 | 升级失败，**已自动回滚**并通过复核（版本号、schema、服务健康、doctor 子集） | 看 `details.rollback` 与日志；升级前备份保留 |
| 12 | 回滚也失败，journal 保留 | 排除原因（磁盘/凭据/口令文件…）后 `lingedge recover --rollback`；升级前备份在提示的路径 |
| 13 | 存在未完成事务 | `lingedge recover --rollback` |
| 14 | 组件未安装 | — |

提交点（`state.json` 写入）之后被中断：`recover` 只补写收尾，**不会回滚已提交的升级**；要回退用 `lingedge rollback`。提交点之前被中断：`recover --rollback` 逆序撤销并复核；`--resume` 不被支持。

## 6. 手动回滚

- 默认回到上一次升级的来源版本（其 release 目录须仍保留）。上一次升级含**不可逆迁移** → **拒绝简单回退**（退出码 3），提示 `--from-backup <升级前备份>`；备份路径在 `lingedge list`/`state.json` 的 `upgrade.backup` 与升级输出里。
- 会改动数据库的路径（`migrate_down`、`--from-backup`）先做一份**回滚前安全备份**（`backup list` 里用途 `pre-rollback`，保留，Tools 不自动清理），失败则撤销回到回滚之前。
- `--from-backup` 覆盖当前数据库与配置：`--yes` 不足以确认，须 `--confirm-name <本机主机名>`（或交互键入）；恢复后在启动旧 Server 之前执行 `lineage bump`，使节点仍能接收新修订。
- 回滚不清理 release；下一次成功升级时才按保留策略清理。

## 7. update self

清单里的 `tools` 组件，同一条验证链取包；写 `lingedge.new` → 执行 `lingedge.new --version` 自检（版本必须等于目标版本）→ 旧版本保留为 `lingedge.prev` → 原子 `rename`。失败（自检不符、包被篡改、低于当前版本、不满足 `min_upgrade_from`）时当前可执行文件**原封不动**。要退回：`mv /usr/local/bin/lingedge.prev /usr/local/bin/lingedge`。

## 8. 不在本阶段的内容

节点的升级路径（`lingedge-node-upgrade.path/.service`、`update node --apply-staged`，ADR-024，Epic 3）；`secrets.rewrap` 与移除 KEK 的命令（Story 6.18）；Server 就绪探针的 N-1 兼容（见 §3）；真实 Linux/宝塔/真实发行源环境下的升级回滚演练（Story 1.45，需 ENV-LNX/ENV-BT 与 UD-3）。
