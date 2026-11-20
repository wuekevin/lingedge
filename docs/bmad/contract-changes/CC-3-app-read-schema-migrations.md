# 契约变更说明 CC-3：lingedge_app 只读授权 `schema_migrations`（迁移版本探针）

> 用法与格式见 `_template.md`。对应 Story 1.13（`docs/bmad/stories/1-13-lingedge-server-进程装配-roles-子命令-sd_notify.md`），发生在 `contract-v1`（CF-1）与 `migrations-m1`（CF-2）之后：M1 冻结范围（`000001`–`000013`、61 张表）不动，仅追加新迁移 `000014`。

- **日期**：2026-11-21
- **基线标签**：contract-v1
- **影响范围**：新增迁移 `migrations/000014_app_read_schema_migrations.sql`（一条 `GRANT SELECT ON schema_migrations TO lingedge_app`）；`migrations/CHECKSUMS` 追加登记；迁移 lint（`internal/platform/migrate/lint.go`）增加「框架版本表 `schema_migrations` 对 app 仅允许 SELECT」的例外；`docs/database/README.md` §8.1 增加追加迁移说明。OpenAPI 与 proto 无任何改动；`000001`–`000013` 一字未改。
- **是否破坏**：否
- **关联 ADR**：N/A（非破坏；约束依据 database/README §5.1 三类账号最小权限、ADR-016 §4/§6）
- **变更动机**：Story 1.12 遗留：`/healthz/ready` 需要核对「库内迁移版本 == 二进制内嵌最高版本」，但 `lingedge_app` 对 `schema_migrations` 没有任何授权（版本表由迁移框架创建，迁移文件里从无授权语句），app 池读不到版本。
- **方案选择（二选一，选 (a)）**：
  - (a) **为 app 增加只读授权（采用）**：授权面仅为「一张只含 `version_id/is_applied/tstamp` 的版本表的 SELECT」，不含 `schema_migrations_runs`（运行记录含错误文本与校验和，探针不需要），不含任何写权限；信息暴露面仅是「已应用的迁移序号与时间」，对已能运行在 app 账号下的进程无额外价值。
  - (b) 在 Server 进程内长期持有 migrate 池只读使用：**否决**。`lingedge_migrate` 是 `ALL ON db.* WITH GRANT OPTION`（可建/删表、改授权、删审计分区），「只读使用」只是代码约定而非权限边界；把它的凭据放进常驻的、面向网络的进程，一旦该进程被攻破即获得整库 DDL/授权能力，直接违反三账号最小权限（IMP-19）。同时 migrate 凭据的落点（AA-7）尚未定稿，Server 运行期也不应依赖 `migrate.yaml`（root 0400）。
- **兼容与迁移方案**：
  1. 新迁移 `expand`、无 Down、幂等（重复 `GRANT` 无副作用）；框架会在迁移前自行创建 `schema_migrations`，所以 `GRANT` 执行时表必然存在（真实 MySQL 测试覆盖：空库全量 up、重放为空操作）。
  2. **lint 约束**：`GRANT … ON schema_migrations TO lingedge_app` 只允许恰为 `SELECT`；`INSERT/UPDATE/DELETE/ALL`、`WITH GRANT OPTION`、授予 `lingedge_maint`、授权其他「未创建」的表均被 lint 拒绝，且每种都有负向样本（`internal/platform/migrate/lint_test.go` 的 `TestLintGrantRules`）。
  3. **M1 冻结口径**：「M1 = 000001–000013、61 张表」不变；骨架 `m1-fk-skeleton.sql` 与 README §8.1 的 13 行表不改（000014 不建表，作为追加迁移在 §8.1 下方的说明中登记）；`freeze-check` 不变。依赖「迁移文件总数 == 13」的测试改为只取 M1 冻结范围（前 13 个）。
  4. 并行泳道返工：无。Tools 安装（Story 1.24+）本就运行内嵌全部迁移，自动带上 000014；升级路径是普通 `migrate up`。
  5. 未升级到 000014 的库：就绪检查读 `schema_migrations` 会因无权限失败 → `/healthz/ready` 报告迁移 `unknown` 并 503（fail-closed），提示先 `migrate up`；这是期望行为（二进制与库不一致本就不应就绪）。
- **验证**：`tests/migrations`（`TestCC3AppReadsSchemaMigrationsOnly`：真实临时 MySQL 上 app 能读版本、对版本表的 INSERT/UPDATE/DELETE/DROP 与读 `schema_migrations_runs` 均被拒绝、授权恰为 SELECT、重放幂等；`TestM1C…` 仍断言 000001–000013 哈希封存）；`make migrations-check`（相对 `migrations-m1` 只新增、未改动已发布文件）、`make migrate-test`、`make contract-check` 全绿（见 Story 1.13 开发记录）。
- **评审**：开发代理（bmad-build）依据机器验证出具，2026-11-21；未经人工签收，用户可事后推翻（推翻则以新的 expand 迁移 `REVOKE` 并改用其它探针方案）。
