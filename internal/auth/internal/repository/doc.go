// Package repository 是认证模块的 MySQL 访问（Story 1.16）：users、sessions、login_logs、tenant_members/tenants、角色权限读取、system_settings 读取。
//
// 关于 Scope 首参：这些方法在认证钩子里被调用——此时 Scope 还没有产生（Scope 钩子在认证之后才由主体生成，ADR-009 §5）——
// 且访问键就是用户自己的标识（email / token_hash / user_id / session uid + user_id）。因此导出方法在文档注释里登记
// `lingedge:platform-table=` / `lingedge:identity-table=`（清单见 scripts/gates/data，门禁 make repo-check 核对）。
// 所有查询都以「某个用户」为边界：没有任何方法能按租户或全局枚举他人的会话/登录日志。
// 只接受 app 连接池；SQL 均为常量，值经占位符绑定。
package repository
