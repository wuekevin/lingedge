// Package platform 是 Server 依赖图最底层的基础设施容器：根包只承载本说明，实现位于子包（子包布局自由，ADR-001 §3）。
//
// 已交付（Story 1.4）：
//   - config：YAML 配置加载（schema 校验、环境变量覆盖、未知键拒绝、Secret 类型）；
//   - logx：Zap 集成与日志脱敏（引擎在 pkg/redact）；
//   - apierr：统一错误码、HTTP 状态映射与错误体；
//   - requestid：Request ID（ULID）的入口读取/生成与 context/gRPC 元数据传递；
//   - clock：唯一允许读取系统时钟的位置（其余 internal/ 业务代码由 lint 禁止直接调用 time.Now）；
//   - uid：ULID 生成与校验。
//
// 已交付（Story 1.5）：
//   - db：MySQL 连接（UTC/utf8mb4/parseTime/参数化）、三类账号（app/maint/migrate）的独立连接池与经 context 传播的事务；
//     db/tenantdb：租户范围表的查询构建器（自动追加 tenant_id、INSERT 强制写入 Scope 租户、AcrossTenants 方法族）；
//   - tenantscope：租户 Scope 的类型与唯一构造入口（FromPrincipal/FromTask/PlatformForJob，包外无法构造非零 Scope）；scope：Scope 的类型别名（Repository 签名用）。
//
// 已交付（Story 1.6）：
//   - migrate：迁移框架（goose v3 嵌入；分类标注与结构 lint、DDL 预检、逐表授权检查；`lingedge-server migrate plan|up|down|status|recover|lint`：
//     版本门槛、迁移互斥、dirty/校验和、不可逆确认）；迁移文件在仓库根 migrations/（migrations.FS）。
//
// 其余基础设施（httpx、task 运行时、keyprovider、eventbus、audit 等）随后续 Story 增加；ports 子包见其自身说明。
package platform
