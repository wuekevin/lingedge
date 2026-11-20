// Package repository 是 secretbox.RecordStore 的真实 MySQL 实现（Story 1.11 遗留项、ADR-011 §3）：
// 供 secrets.rewrap 按 kek_id 扫描各 *_enc 列、CAS 写回并在移除 KEK 前统计引用数。
//
// 白名单：被扫描的（表,列）只来自本包的 targets 声明，且每个目标的 SQL 都是编译期常量（db.Q），
// 没有任何外部输入的标识符参与 SQL 文本；外部只能传入「值」（kek_id、uid、密文），一律经 ? 占位符绑定。
//
// 覆盖范围以 migrations/ 中实际存在的 *_enc 列为准（M1，000001–000013）：
//
//	certificates.private_key_enc   （有 enc_key_id 列）
//	signing_keys.private_key_enc   （有 enc_key_id 列）
//	system_settings.value_enc      （无 enc_key_id 列：以密文第 2 段 kek_id 判定）
//	users.totp_secret_enc          （无 enc_key_id 列：以密文第 2 段 kek_id 判定）
//
// provider_credentials.credential_enc 随 P2 建表（见 docs/database/platform-identity.md），届时按本包的声明方式追加目标，
// 不得在此提前声明不存在的表。上述表对 lingedge_app 均有 SELECT/INSERT/UPDATE/DELETE（见各迁移的 GRANT），
// 因此重包装只用 app 池（New 拒绝其他角色；maint 账号对这些表没有任何授权，也不需要），不涉及 maint 约定；audit_logs 等仅追加表不含 *_enc 列。
//
// 租户范围：certificates 是租户表，而重包装是跨租户批处理，故所有方法名含 AcrossTenants 并以平台范围 Scope 为首参（逐次校验，
// 租户范围或零值 Scope 一律拒绝；repo-scope 门禁）。secretbox.RecordStore 接口无 Scope 参数，由 internal/platform/secrets.NewRecordStore
// 绑定 tenantscope.PlatformForJob 后适配（该调用点受 [tenantscope-mint] 白名单约束，须登记于 Story 1.14/1.15 的作业清单）。
package repository
