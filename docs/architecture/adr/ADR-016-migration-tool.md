# ADR-016 Migration 工具选型：goose v3（嵌入式）+ 分级标注

- 状态：已接受（架构代理非交互决策）｜日期：2026-11-16｜关联：基线 §18、§4.7；PRD FR-8、FR-22、NFR-23、R-3；architecture AD-13、AD-16｜阶段：M1

## 背景

基线要求「必须使用 Migration」，PRD R-3/NFR-23 要求每个迁移分类为 **可逆 / 前向兼容（expand-contract）/ 不可逆**，Tools 升级前要能展示待执行迁移及其分类并据此决定是否强制备份。迁移必须随 Server 二进制发行（Tools 不单独携带 SQL），且能在宝塔等无额外工具的主机上执行。MySQL 的 DDL 不是事务性的（隐式提交），失败会留下半完成状态。

## 决策

1. **采用 pressly/goose v3 作为库，嵌入 `lingedge-server`**（`embed.FS` 内含 `migrations/*.sql`）。2026-11-16 查询的最新版本 v3.30.1，实现时核对并固定；开发机上的 goose CLI 仅作开发便利，生产不依赖。
2. **文件规范**：`migrations/NNNNNN_<name>.sql`（6 位递增序号，**不使用时间戳**，避免并行分支冲突后顺序歧义）；SQL 迁移为主，必要时 Go 迁移（仅用于数据回填，必须幂等）。
3. **文件头强制标注**：第一行 `-- lingedge:class=reversible|expand|irreversible`，另可有 `-- lingedge:contract-after=<版本>`（expand 类对应的收缩迁移将在哪个版本执行）。CI lint 校验：缺标注失败；`reversible` 必须有可执行的 `-- +goose Down`；`irreversible` 的 Down 必须为空或显式 `SELECT 'irreversible'` 并带原因注释。
4. **版本表**：使用自定义表名 `schema_migrations`（以 goose 的表名设置接口实现，API 实现时核对），额外在 `system_settings` 记录 `schema.latest_class_irreversible_applied` 辅助 doctor。
5. **MySQL DDL 约束的应对**：每个迁移文件单职责（一个表/一组紧密相关 DDL）；使用 `IF NOT EXISTS`/存在性守卫使重跑安全；含数据回填的迁移分两个文件（先 DDL 后 DML），回填可断点重入；goose 对非事务 DDL 使用 `-- +goose NO TRANSACTION`。
6. **MySQL 版本与账号前置**：设计依赖 CHECK 约束真正生效，故最低支持版本为 **MySQL ≥ 8.0.16**；`lingedge-server migrate` 启动时先 `SELECT VERSION()`，低于该版本拒绝执行并说明（安装向导同样预检，tools-design §13）。迁移使用独立账号 `lingedge_migrate`：`ALL ON <db>.* WITH GRANT OPTION` + 全局 `SET_USER_ID`（binlog 开启且 `log_bin_trust_function_creators=0` 时建触发器必需，实测见 docs/database/README.md）；分区表的维护账号 `lingedge_maint` 的表级 `SELECT, ALTER, DROP` 授权**写在创建该分区表的迁移 SQL 里**（`GRANT … ON <db>.<table> TO lingedge_maint`），无需再次使用管理员凭据。
7. **执行入口**：`lingedge-server migrate plan|up|down|status`。`plan` 输出待执行迁移清单（序号、名称、分类、是否含 Down）及汇总「是否含不可逆」，**供 Tools 在升级前展示**（tools-design 的升级 plan）；`up` 获取 `job_locks` 名为 `migrate` 的租约（多实例并发安全）。Tools 不直接连库跑 SQL，只调用 Server 的 `migrate` 子命令。
8. **Expand/contract 优先**：破坏性变更（删列/改类型）拆成两个版本：vN 只 expand（新旧二进制均可运行），vN+1 或更后版本才 contract（此时才是 `irreversible` 或 `reversible` 视是否可保留数据）。M1 的初始建表迁移标 `expand`（新增表，旧二进制不存在，回退二进制即可）。
9. **不可变表触发器**（config_revisions 等，见 database README §5）随迁移创建，同样受分类约束。
10. **迁移测试**：空库在**最低支持版本 8.0.16 与当前 8.0.x** 上全量升级；逐个升级/降级路径（仅 reversible）；expand 迁移用「上一版本二进制 + 新库结构」兼容性测试；数据库使用独立 `lingedge_test` 库（dev-environment §2）。

11. **MySQL 8.0 DDL 约束清单（迁移 lint/`make migrate-test` 的 DDL 预检门禁，均已在 8.0.46 实测，结果见 docs/database/README.md「DDL 实测」）**：
    - **生成列 × 外键**：STORED 生成列的基列上，外键不得使用 `CASCADE/SET NULL/SET DEFAULT`（ERROR 1215）；用于唯一性的生成列默认 **VIRTUAL**（VIRTUAL 可带唯一索引，且基列 FK 不受此限）；STORED 仅在基列 FK 为 `RESTRICT` 时使用。
    - **CHECK × 外键**：CHECK 引用的列若同时是外键列，只许 `RESTRICT/NO ACTION/ON DELETE CASCADE`；`ON DELETE SET NULL`、`ON UPDATE CASCADE/SET NULL` 报 ERROR 3823。
    - **前向外键**：子表不得先于父表创建（ERROR 1824）；迁移按依赖排序，循环依赖先建表、后以 `ALTER TABLE … ADD CONSTRAINT` 追加。
    - **分区表**：分区表不能作为外键的父表或子表（ERROR 1506）；按时间 RANGE 分区的表必须带 `pmax VALUES LESS THAN (MAXVALUE)` 兜底（否则越界 INSERT 报 ERROR 1526），维护用 `REORGANIZE PARTITION`。
    - **触发器权限**：binlog 开启且 `log_bin_trust_function_creators=0` 时，建触发器需要 `SUPER` 或全局 `SET_USER_ID`（ERROR 1419）；见第 6 点与安装向导预检。
    - **TEXT/JSON/BLOB 不设 DEFAULT 字面量**（团队规范；TEXT 字面量默认在 MySQL 报 ERROR 1101）。

## Binds / Prevents / Rule

- 约束：所有写迁移的开发者、Tools 升级流程、CI。
- 防止：迁移无分类导致升级回滚承诺失真；半完成 DDL 无法重入；Tools 与 Server 对「是否可回退」产生分歧。
- 规则（AD-16）：**Migration 工具 = goose v3（嵌入式、SQL 为主），文件头强制 `lingedge:class` 标注；分类由 `migrate plan` 输出，Tools 以其为唯一判据。**

## 备选方案与权衡

- **golang-migrate/migrate**（v4.20.1）：否决——Up/Down 配对简单，但对 Go 数据迁移与自定义标注/注释解析支持较弱，且需要自写嵌入与计划输出；goose 的库接口与 SQL 注解更贴合。
- **Atlas 声明式迁移**：否决——声明式 diff 难以表达分类与不可逆语义，且引入外部工具/许可评估；与「升级前可审阅的明确迁移清单」冲突。
- **自研迁移器**：否决——收益有限、测试负担重；仅在 goose 的版本表与标注解析被证明不够用时再议。

## 后果

- 需要一个 CI lint（解析头标注与 Down 块）；自写，约百行级。
- goose 在 MySQL 上的非事务 DDL 无法自动回滚中途失败：靠单职责文件 + 幂等守卫 + 升级前备份（ADR-013）兜底。
- Server 二进制随迁移增大（嵌入），可接受。

## 可被推翻处

若改选 golang-migrate 或其他工具：需重写 `migrate` 子命令与 lint，文件头标注与 `plan` 输出契约（tools-design 升级章节）保持不变；database README 的迁移命名约定同步修改。

## 验证

- `make migrate-test`：空库全量、升/降级路径、标注检查（dev-environment §3 Makefile）。
- 对含 `irreversible` 的版本，Tools 升级 plan 测试强制备份与确认（AC-J-5）。
- 重入测试：在每个迁移中途中断并重跑，结果与一次性执行一致。

## 修订记录 2026-11-16

- **M12 / IMP-19**：新增决策 6（最低支持版本 **≥ 8.0.16**，`migrate` 启动校验 `VERSION()`；迁移账号 `GRANT OPTION` + `SET_USER_ID`；分区表的 maint 表级授权写进创建该表的迁移 SQL），原因：设计依赖 CHECK 强制执行（8.0.16 起），binlog 开启时建触发器需额外权限（本机 8.0.46 实测 ERROR 1419），评审 IMP-19(3)/M12 成立。原第 6–9 点顺延为 7–10，外部对“ADR-016 §9 迁移测试”的引用改为 §10。
- **BLK-01 / H1 / H2**：新增决策 11（DDL 约束清单）与 `make migrate-test` 的 DDL 预检门禁（空库全量 up、生成列×FK、CHECK×FK、前向 FK、分区表无 FK）。实测结论：STORED 生成列+CASCADE 失败、VIRTUAL 通过；CHECK+`ON DELETE CASCADE` 通过而 `SET NULL` 失败；前向 FK 失败（评审对 roles/node_certificates 的 CHECK 判断部分不成立，见修订日志）。
- 决策 10 的迁移测试覆盖最低支持版本 8.0.16 与当前 8.0.x。

## 实现记录 2026-11-17（Story 1.6）

按本 ADR 落地迁移框架（`internal/platform/migrate`、`migrations/embed.go`、`lingedge-server migrate`）。实现时核对并固定的细节，未改变决策：

- **goose 版本固定 v3.30.1**（2026-11-17 查询的最新版）；以 `goose.Provider` 库形式使用，`goose.WithTableName("schema_migrations")` 实现自定义表名；goose 与 `database/sql` 只允许出现在 `internal/platform/migrate`（depguard，`make lint`）。`make vuln` 无漏洞。
- **dirty 由框架自己的运行记录表实现**（goose 本身没有 dirty 概念）：`schema_migrations_runs` 记录每个迁移的 running/failed/applied、方向、SHA-256 校验和与错误文本；dirty 时 `up/down` 拒绝并给出恢复指引，`migrate recover --version N` 是清除标记的唯一入口。MySQL DDL 隐式提交，失败的迁移可能留下部分对象，但不会被记为已应用；纯 DML 迁移在事务内整体回滚。
- **互斥（决策 7 的细化）**：`GET_LOCK` 始终持有作为互斥本身；`job_locks` 存在后另记录 `migrate` 租约（holder、fence、lease_until）。原因：`job_locks` 由 000002 创建，若「有表则只用租约」，则用 GET_LOCK 起步的进程与之后改用租约的进程之间出现互斥缝隙。
- **校验和**：已应用迁移的文件字节 SHA-256（与 `migrations/CHECKSUMS` 同口径）记录在运行表，之后任何不一致、乱序、数据库有而二进制没有的迁移都拒绝执行。
- **`plan`**：连库时只列待执行迁移（含 `db_version`、`mysql_version`），不连库时列全部内嵌迁移；`has_irreversible`、`max_class` 为汇总；`plan/status` 只读，不创建任何表。
- **`up` 的不可逆确认**：待执行集含 `irreversible` 时须 `--accept-irreversible`（退出码 9，与 ADR-013 §2 一致），否则一个迁移都不执行；`down` 只回退 `reversible`，先整体校验再执行。
- **逐表授权**（database/README §5.1，Story 1.5 的发现）：lint 强制每张新表有 `GRANT … ON <表> TO lingedge_app`（追加只写表仅 `SELECT, INSERT`），分区表另有 `lingedge_maint` 的表级 `SELECT, ALTER, DROP`；授权必须是不带库名的表级语句。
- **三路评审后的加固**：`recover` 只在「版本表/运行记录/校验和」一致时才清除标记（防止把被篡改文件的哈希洗白、防止 down 已完成后撞上主键冲突）；`plan` 与 `up` 同样校验完整性；账号预检增加 GRANT OPTION 探针（决策 6）；持锁连接加心跳；退出码 10 表示「数据库与二进制的迁移集合不一致」；lint 增加可执行注释、同行多语句、账号/库管理语句、多账号/`WITH GRANT OPTION` 授权、`expand` 的 NOT NULL 无默认列等规则；`VALUES()` 仍用于 `ON DUPLICATE KEY UPDATE`（行别名语法自 8.0.19 起才有，而最低版本是 8.0.16）。
- **未在本 Story 实现（有意延后）**：决策 4 中在 `system_settings` 记录 `schema.latest_class_irreversible_applied`——`system_settings` 随 Story 1.7（000002）才存在，随该表落库后由 `up` 在成功应用 `irreversible` 迁移后写入；`doctor` 的消费随 Tools 泳道。
- **AA-7 过渡**：迁移账号凭据尚无定稿的交接方式（`decisions-adopted.md` §3）；本 Story 只约定环境变量 `LINGEDGE_MIGRATE_DB_USER`（缺省 `lingedge_migrate`）、`LINGEDGE_MIGRATE_DB_PASSWORD` 或 `LINGEDGE_MIGRATE_DB_PASSWORD_FILE`，口令不经命令行。AA-7 定稿后只需改 `internal/platform/migrate/cli.go` 的凭据来源。
- **最低版本 8.0.16**：版本门槛已实现并有单元测试，但 8.0.16 空库全量 `up` 本机无法验证（仅 8.0.46），仍为「等待环境验证」（修订日志 §4 ①）。
