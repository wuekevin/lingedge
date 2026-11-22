# 契约变更说明 CC-13：安装向导的 MySQL Unix socket 连接目标与初始化步骤的迁移明细（`SetupDbConfig.socket`、`SetupStepResult.migrations`；均为加法/放宽）；配置键 `database.socket`；无迁移变更

> 用法与格式见 `_template.md`。由 Story 1.37（向导前端）落地 Story 1.24 / 1.27b 登记的「运行期凭据文件不支持 Unix socket」遗留（已由 1.24、1.27b 预告为 CC-13）。发生在 `contract-v1`（CF-1）与 `migrations-m1`（CF-2）之后。**没有任何迁移改动**，**proto 一字未改**。

- **日期**：2026-11-22
- **基线标签**：contract-v1
- **影响范围**：
  1. `docs/api/openapi.yaml`：`components.schemas.SetupDbConfig` 新增可选属性 `socket`（MySQL Unix 域套接字绝对路径，与 `host`/`port` 互斥，二者恰设其一）；`host`、`port` 由 `required` 移出（请求体校验放宽：改由服务端按「host+port 或 socket 恰其一」校验并以 400 报告，只发 host/port 的旧客户端行为不变）；`components.schemas.SetupStepResult` 新增只读可选属性 `migrations`（仅 `setupInitializeDb` 返回，逐个迁移的版本/名称/分类（字段名 `category`：生成的前端类型门禁禁止出现 `class` 字样）/耗时），供向导前端逐条展示（tools-design §13 步骤 4「显示每个迁移的级别」，此前只在 `message` 的自由文本里）。所有 operation、`x-*` 扩展与响应码集合不变。
  2. 配置（非 OpenAPI/proto）：`server.yaml` 新增可选键 `database.socket`（与 `database.host/port` 互斥；环境变量 `LINGEDGE_DATABASE__SOCKET`）；运行期凭据文件 `runtime-credentials.json` 的 `database` 段新增可选字段 `socket`（v1 内的向后兼容加法：旧文件无该字段照常读取；含 `socket` 的新文件不被旧二进制识别为合法——严格解析拒绝未知字段，属降级场景，ADR-026 §3 已声明「未知字段 fail-closed」）。
  3. 迁移、proto、错误码：无新增（沿用 `validation.failed`、`setup.precheck_failed`）。
- **是否破坏**：否（新增可选请求属性、放宽请求必填、新增只读可选响应属性；无字段删除/改名/收紧）
- **关联 ADR**：N/A（非破坏；决策沿用 ADR-026）
- **变更动机**：宝塔（ENV-BT）及发行版 MySQL 常只开 Unix socket 或 socket 更适合同机部署；Story 1.24/1.27b 明确把「运行期凭据文件支持 socket」登记为需要 CC 加法 + 1.37 向导字段的整体特性，避免只改 `server.yaml` 一侧形成「手写可用、向导不可用」的半截特性。向导前端（1.37）需要表达该连接目标，并需要初始化步骤的结构化迁移明细。
- **兼容与迁移方案**：旧客户端（只发 host/port）无变化；旧 `runtime-credentials.json` 无需迁移。无需升 `contract-vN`。socket 方式在真实宝塔环境的可访问性（`lingedge` 用户对套接字文件的权限）仍属 ENV-BT 等待环境验证，本机只验证了代码路径与真实 mysqld 的 socket 连接。
- **验证**：`make openapi-check`（含相对 `contract-v1` 无破坏性变更）、`make contract-check`（含前端生成类型无漂移）；实现与集成测试见 `tests/integration/setup`（socket 全流程）。
- **评审**：开发代理（bmad-method:bmad-build）自检，2026-11-22；未经人工签收。
