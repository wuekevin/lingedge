// Package audit 是审计模块（Story 1.21、FR-23、NFR-21、IMP-19、ADR-009 §5、ADR-018 §13）：
//
//   - 记录：业务 Service 在**自己的业务事务内**调用 Recorder.Record 写 audit_logs（与业务变更同一事务）。审计写入失败 = Record 返回错误 =
//     事务整体回滚 = 该请求 500 可重试（fail-closed；原「审计失败不阻断业务」的假设作废）。Record 拒绝在事务之外调用（ErrNotInTx），
//     因此不可能出现「业务已提交、审计没写」的尽力而为写法。
//   - 不可变：audit_logs 只 INSERT——lingedge_app 账号只有 SELECT/INSERT（迁移 000003），表上另有 BEFORE UPDATE/DELETE 触发器；
//     本包与 API 没有任何修改/删除路径。保留期清理是 lingedge_maint 的 DROP PARTITION（Story 3.48，不在此实现）。
//   - 钩子：Writer.Auditor() 是 httpx.Auditor 的真实实现，只做运行期兜底——声明 x-audit: true 的成功请求若没有任何审计行，记 Error 日志并计数。
//   - 目录：Catalog 登记每个要求审计的 operation 可写哪些动作；make audit-check（scripts/gates 的 audit 门禁）静态核对
//     「声明 Audit 但未记录」「写 operation 缺 Audit 声明」。
//   - 读取：auditapi 提供 GET /audit-logs、GET /audit-logs/{auditId}（租户主体仅见自己租户且字段裁剪）。
//
// 变更摘要（Event.Changes）写入前一律经 pkg/redact 脱敏；调用方仍不得把口令、令牌、TOTP 种子、恢复码放进去（G-5 测试断言）。
package audit
