# ADR-013 升级、回滚与不可逆 Migration 策略

- 状态：已接受（架构代理非交互决策）｜日期：2026-11-16｜关联：基线 §4.7、§18；PRD FR-8、FR-10、FR-22、NFR-18、NFR-23、R-3；architecture AD-13；细节见 tools-design §17、ADR-016｜阶段：M1

## 背景

基线 §4.7：升级流程"检测版本→获取签名清单→检查兼容性→下载→校验→备份当前版本→检查数据库迁移→安装新版本→启动及健康检查→成功确认或回滚"；**存在不可逆 Migration 时，禁止承诺简单回退二进制即可恢复**。PRD R-3 定义 Migration 三级分类。

## 决策

### 1. Migration 三级分类（每个迁移文件必须标注，ADR-016）

| 级别 | 头标注 | 定义 | 升级与回滚 |
|---|---|---|---|
| 可逆 `reversible` | `-- lingedge:class=reversible` | 提供经测试的 Down | 二进制与数据库可一起回退 |
| 前向兼容 `expand` | `-- lingedge:class=expand` | 只"增"（新表/列/索引，新列可空或带应用侧默认）；旧二进制仍可运行 | **升级失败的即时回滚**可回退二进制（新二进制尚未写入新语义数据）；新版本已运行并写入新列/新枚举值之后的回退依赖 N-1 兼容性测试（ADR-016 §10），不做无条件承诺；清理（contract）步骤单独版本发布 |
| 不可逆 `irreversible` | `-- lingedge:class=irreversible` | 破坏性变更或数据转换 | 升级前**强制备份并要求显式确认**；回滚 = 恢复备份 + 旧二进制；明示备份点后数据丢失 |

**工程规则**：优先 expand/contract 两阶段；`contract` 迁移（删列/改类型）出现在至少**一个发行版本之后**，且标 `irreversible`（或 `reversible` 当可提供数据保全 Down 时）；每个版本的清单声明 `migrations: {max_class, ids[]}` 供 Tools 在下载前计划。

### 2. 升级流程（Tools，所有组件统一）

1. `update check`：拉取并验证清单（ADR-012），解析出目标版本与兼容性（`min_upgrade_from`、`requires`）。
2. **plan**：展示将执行的步骤、迁移列表与最大级别、停机预估；`--dry-run` 只做 plan。
3. 下载并校验包到临时目录；
4. **备份**（升级前强制）：当前 release 目录引用（不复制二进制，保留旧 release 目录即可）、`/etc/lingedge/<comp>/` 配置、**数据库逻辑备份**（Server 升级；含迁移表 `schema_migrations` 当前版本）。不可逆迁移：备份**必须成功**，否则中止；
5. 若迁移最大级别为 `irreversible`：**交互确认**（TUI 键入 `confirm-irreversible` / 非交互 `--accept-irreversible` 显式标志（与 tools-design §3/§4 一致，此前误写 `--yes-irreversible`）；非交互且未带标志 → 退出码 9）；明示"回滚路径=恢复备份，备份点之后的数据会丢失"；
6. 停止服务 → 原子切换 `current` → 运行 `lingedge-server migrate up`（先 `migrate plan` 校验）→ 启动 → **健康检查**（`/healthz` + 组件特定检查：Server 迁移版本一致与依赖连通；Node Agent 与 OpenResty 状态与 `config_hash`；超时由清单 `health_timeout` 指定，默认值待基线测试）；
7. 成功 → 确认并保存状态；保留最近 N 个 release 目录（默认 **2**，可配，不少于 2；与 tools-design §8 一致，此前误写 3）；失败 → **回滚**。

### 3. 回滚

- **可逆/前向兼容**：切回旧 release 目录（符号链接）→ 若有 `reversible` 迁移则 `migrate down` 到升级前版本 → 启动旧版本 → 健康检查；
- **含不可逆迁移**：恢复升级前数据库备份 + 旧 release；Tools 拒绝宣称"简单回退"；`lingedge rollback <component>` 在含不可逆的场景进入"恢复流程"并二次确认；**数据库恢复后、启动 Server 之前必须执行 `lingedge-server lineage bump`**（见 §4），否则节点已应用的配置序号高于恢复后的 DB 计数，会永久拒绝新修订；
- **回滚后验证**：版本号、配置、迁移版本与升级前一致（AC-J-4/J-5）。
- 节点升级：Agent 层面的"配置回滚"（ADR-002）与二进制回滚是**两件事**；节点二进制无数据库迁移，回滚为切回旧 release 目录。

### 4. 备份恢复（NFR-18）

`backup create`：配置 + DB 逻辑备份 + 元数据（版本、迁移版本、校验和）打包，权限 `0600`（可选加密：age/GPG 实现时核对），**不含主密钥**（ADR-011）；`backup restore`：恢复前校验迁移版本与二进制兼容性、`master_key_fingerprint` 与 `kek_ids_required`，恢复后校验可解密探针记录；**恢复后、启动 Server 前执行 `lingedge-server lineage bump`**：`deploy.lineage = max(恢复后 DB 值 + 1, 当前 UNIX 秒)`，节点按 `(lineage, seq/epoch/marker)` 字典序严格大于比较（ADR-002 §12），因此恢复到较早 DB 状态后节点仍接受新修订、失效标记不被 `max()` 吞掉；恢复演练纳入验收（耗时目标待基线），**AC-J-5 增补“备份恢复后节点仍能接收并应用新修订”**。备份清单记录 `kek_ids_required`，KEK 移除前核对保留备份（ADR-011 §3）。

### 5. 数据库迁移执行约束

- 单次升级自动应用所有待执行迁移，按文件序执行；MySQL DDL 非事务（ADR-016），每个迁移文件职责单一并带幂等守卫；迁移失败 → 标记 `dirty`，Tools 触发回滚路径（切回旧二进制 + 按级别决定是否恢复备份）；`migrate` 子命令拒绝在 `dirty` 状态下继续。
- 多实例 Server 滚动升级不在 M1 范围（单实例）；`expand` 规则为未来滚动升级预留。

## Binds / Prevents / Rule

- 约束：所有 migrations/、Tools 升级/回滚/备份、发布流水线（清单迁移元数据）。
- 防止：把不可逆变更当成可回退发布；升级中途失败无恢复路径；破坏性 DDL 与代码同版本发布导致无法回退。
- 规则（AD-13）：**迁移三级分类并强制标注；升级前备份；优先 expand/contract；不可逆迁移显式确认，回滚=恢复备份+旧二进制，不承诺简单回退。**

## 备选方案与权衡

- **所有迁移都必须可逆**：否决——数据转换类变更无法保证；强行写 Down 带来虚假安全感。
- **蓝绿数据库（双库）**：否决——自部署场景成本高。
- **仅依赖二进制回退**：违反基线 §4.7。

## 后果

- 发布纪律更严（两阶段删除、级别标注）；迁移 lint 与测试成本增加。
- 不可逆升级必须有停机窗口与备份时间。

## 可被推翻处

保留 release 目录数、备份加密工具、健康检查超时；是否允许自动 `migrate down`（默认仅 `reversible` 且升级失败时自动）。

## 验证

- 构造健康检查失败的新版本 → 自动回滚（AC-J-4）；构造含不可逆迁移的版本 → 升级前警告+强制备份+确认（AC-J-5）；回滚后状态一致性比对。
- CI 迁移 lint：缺少 class 标注失败；`expand` 迁移出现 `DROP/RENAME/MODIFY` 失败；迁移测试覆盖升级与（对 reversible）降级路径（NFR-23）。

## 修订记录 2026-11-16

- **MIN-05**：expand 迁移“回退二进制即可”限定为**升级失败的即时回滚**；新版本运行并产生数据后的回退须依赖 N-1 兼容性测试，不做无条件承诺（原措辞偏强：新枚举值/新列数据可能令旧二进制读取异常）。KEK 移除前核对保留备份（与 ADR-011 §3 联动）。
- **IMP-07 / H6**：§3、§4 增“恢复数据库后须 `lingedge-server lineage bump`”及 AC-J-5 增补；原因：恢复备份使 DB 中 `clusters.current_seq/cache_epoch/marker_version` 低于节点已持久化值，节点会永久拒绝新修订（ADR-002 §12 的 lineage 机制）。
- **M16**：不可逆确认标志统一为 `--accept-irreversible`（删 `--yes-irreversible`）；release 保留份数默认 2（原 3，与 tools-design §8 对齐）。
