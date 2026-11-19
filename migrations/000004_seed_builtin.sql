-- lingedge:class=expand
-- lingedge:summary=种子数据：内置权限字典、内置角色与授权、默认租户（仅 DML，可重放；不含口令、不创建任何用户）
-- 管理员账号只由安装向导创建（database/README §8；NFR-6、G-6）；本文件不含任何口令、哈希或用户行。
-- 权限字典 = docs/api/openapi.yaml 全部 x-permissions（含 P2–P5 阶段的权限串，仅字典，功能随阶段启用）+ tenant:impersonate（ADR-018 §10）。
-- 权限 scope：仅被 platform 路由使用 = platform；仅被 tenant 路由使用 = tenant；被 either 路由或两类路由共用 = both。
-- 租户角色只授予 scope ∈ {tenant, both} 的权限（platform 权限永不进入租户角色）。内置角色 tenant_id 为 NULL（平台定义的角色模板）。
-- +goose Up

-- ---- 权限字典（81 项）----
INSERT INTO permissions (code, resource, action, scope, module, description) VALUES
  ('alert:read', 'alert', 'read', 'both', 'notification', '告警规则/历史'),
  ('alert:write', 'alert', 'write', 'both', 'notification', '告警规则/历史'),
  ('audit:read', 'audit', 'read', 'both', 'audit', '审计日志'),
  ('billing:manage', 'billing', 'manage', 'platform', 'billing', '计费/订单/用量/账单'),
  ('billing:read', 'billing', 'read', 'both', 'billing', '计费/订单/用量/账单'),
  ('billing:write', 'billing', 'write', 'both', 'billing', '计费/订单/用量/账单'),
  ('cache:disk:read', 'cache:disk', 'read', 'platform', 'cache', '缓存策略/刷新/统计/磁盘'),
  ('cache:purge', 'cache', 'purge', 'tenant', 'cache', '缓存策略/刷新/统计/磁盘'),
  ('cache:read', 'cache', 'read', 'both', 'cache', '缓存策略/刷新/统计/磁盘'),
  ('cache:write', 'cache', 'write', 'tenant', 'cache', '缓存策略/刷新/统计/磁盘'),
  ('certificate:distribute', 'certificate', 'distribute', 'tenant', 'certificate', '证书'),
  ('certificate:read', 'certificate', 'read', 'both', 'certificate', '证书'),
  ('certificate:write', 'certificate', 'write', 'both', 'certificate', '证书'),
  ('cluster:bind', 'cluster', 'bind', 'platform', 'node', '集群、成员、站点绑定'),
  ('cluster:manage', 'cluster', 'manage', 'platform', 'node', '集群、成员、站点绑定'),
  ('cluster:read', 'cluster', 'read', 'platform', 'node', '集群、成员、站点绑定'),
  ('cms:content:write', 'cms:content', 'write', 'platform', 'website', '官网运营（CMS）'),
  ('cms:media:write', 'cms:media', 'write', 'platform', 'website', '官网运营（CMS）'),
  ('cms:nav:write', 'cms:nav', 'write', 'platform', 'website', '官网运营（CMS）'),
  ('cms:page:write', 'cms:page', 'write', 'platform', 'website', '官网运营（CMS）'),
  ('cms:plan-display:write', 'cms:plan-display', 'write', 'platform', 'website', '官网运营（CMS）'),
  ('cms:publish', 'cms', 'publish', 'platform', 'website', '官网运营（CMS）'),
  ('cms:read', 'cms', 'read', 'platform', 'website', '官网运营（CMS）'),
  ('cms:seo:write', 'cms:seo', 'write', 'platform', 'website', '官网运营（CMS）'),
  ('cms:settings:write', 'cms:settings', 'write', 'platform', 'website', '官网运营（CMS）'),
  ('config:publish', 'config', 'publish', 'both', 'node', '配置修订、发布、回滚'),
  ('config:read', 'config', 'read', 'both', 'node', '配置修订、发布、回滚'),
  ('config:rollback', 'config', 'rollback', 'both', 'node', '配置修订、发布、回滚'),
  ('credential:read', 'credential', 'read', 'both', 'plugin', '第三方凭据'),
  ('credential:write', 'credential', 'write', 'both', 'plugin', '第三方凭据'),
  ('deployment:read', 'deployment', 'read', 'both', 'node', '部署记录'),
  ('dns:manage', 'dns', 'manage', 'platform', 'scheduler', 'DNS 与 Zone/记录'),
  ('dns:read', 'dns', 'read', 'both', 'scheduler', 'DNS 与 Zone/记录'),
  ('dns:write', 'dns', 'write', 'both', 'scheduler', 'DNS 与 Zone/记录'),
  ('domain:write', 'domain', 'write', 'tenant', 'website', '站点域名'),
  ('l4:read', 'l4', 'read', 'both', 'l4proxy', '四层代理'),
  ('l4:write', 'l4', 'write', 'both', 'l4proxy', '四层代理'),
  ('log:read', 'log', 'read', 'both', 'monitoring', '访问日志与安全日志查询'),
  ('login-log:read', 'login-log', 'read', 'platform', 'audit', '登录日志'),
  ('metrics:platform:read', 'metrics:platform', 'read', 'platform', 'monitoring', '指标/rollup/工作台'),
  ('metrics:read', 'metrics', 'read', 'both', 'monitoring', '指标/rollup/工作台'),
  ('node:enroll', 'node', 'enroll', 'platform', 'node', '节点管理、注册令牌、升级'),
  ('node:manage', 'node', 'manage', 'platform', 'node', '节点管理、注册令牌、升级'),
  ('node:read', 'node', 'read', 'platform', 'node', '节点管理、注册令牌、升级'),
  ('node:upgrade', 'node', 'upgrade', 'platform', 'node', '节点管理、注册令牌、升级'),
  ('notification:read', 'notification', 'read', 'both', 'notification', '通知渠道/模板'),
  ('notification:write', 'notification', 'write', 'both', 'notification', '通知渠道/模板'),
  ('origin:read', 'origin', 'read', 'tenant', 'origin', '源站组/源站'),
  ('origin:write', 'origin', 'write', 'tenant', 'origin', '源站组/源站'),
  ('plugin:manage', 'plugin', 'manage', 'both', 'plugin', '插件'),
  ('plugin:read', 'plugin', 'read', 'both', 'plugin', '插件'),
  ('probe:manage', 'probe', 'manage', 'platform', 'monitoring', '探测/Reporter'),
  ('probe:read', 'probe', 'read', 'platform', 'monitoring', '探测/Reporter'),
  ('role:read', 'role', 'read', 'platform', 'auth', '角色与权限分配'),
  ('role:write', 'role', 'write', 'platform', 'auth', '角色与权限分配'),
  ('rule:read', 'rule', 'read', 'both', 'ruleengine', '统一规则'),
  ('rule:write', 'rule', 'write', 'tenant', 'ruleengine', '统一规则'),
  ('scheduling:read', 'scheduling', 'read', 'platform', 'scheduler', '调度策略'),
  ('scheduling:write', 'scheduling', 'write', 'platform', 'scheduler', '调度策略'),
  ('setting:read', 'setting', 'read', 'platform', 'system', '系统设置（含品牌、运行模式）'),
  ('setting:write', 'setting', 'write', 'platform', 'system', '系统设置（含品牌、运行模式）'),
  ('site:delete', 'site', 'delete', 'tenant', 'website', '站点基础、设置、网络（真实 IP/可信代理/出口 IP）、TLS'),
  ('site:read', 'site', 'read', 'both', 'website', '站点基础、设置、网络（真实 IP/可信代理/出口 IP）、TLS'),
  ('site:tls:write', 'site:tls', 'write', 'tenant', 'website', '站点基础、设置、网络（真实 IP/可信代理/出口 IP）、TLS'),
  ('site:write', 'site', 'write', 'tenant', 'website', '站点基础、设置、网络（真实 IP/可信代理/出口 IP）、TLS'),
  ('system:read', 'system', 'read', 'platform', 'system', '版本信息、密钥元数据、已安装组件'),
  ('task:cancel', 'task', 'cancel', 'both', 'system', '任务中心'),
  ('task:read', 'task', 'read', 'both', 'system', '任务中心'),
  ('tenant:impersonate', 'tenant', 'impersonate', 'platform', 'tenant', '平台主体经 X-Tenant-ID 代租户操作（acting-as）的前置权限'),
  ('tenant:manage', 'tenant', 'manage', 'platform', 'tenant', '租户与成员、配额'),
  ('tenant:member:manage', 'tenant:member', 'manage', 'platform', 'tenant', '租户与成员、配额'),
  ('tenant:member:read', 'tenant:member', 'read', 'platform', 'tenant', '租户与成员、配额'),
  ('tenant:read', 'tenant', 'read', 'platform', 'tenant', '租户与成员、配额'),
  ('ticket:manage', 'ticket', 'manage', 'platform', 'billing', '工单'),
  ('ticket:read', 'ticket', 'read', 'both', 'billing', '工单'),
  ('ticket:write', 'ticket', 'write', 'both', 'billing', '工单'),
  ('token:manage', 'token', 'manage', 'both', 'auth', '自己名下的 API Token'),
  ('user:read', 'user', 'read', 'platform', 'auth', '平台用户管理'),
  ('user:write', 'user', 'write', 'platform', 'auth', '平台用户管理'),
  ('waf:read', 'waf', 'read', 'both', 'waf', 'WAF/CC/IP 限制'),
  ('waf:write', 'waf', 'write', 'both', 'waf', 'WAF/CC/IP 限制')
ON DUPLICATE KEY UPDATE code = code;

-- ---- 内置角色（is_builtin=1，tenant_id 为 NULL）----
INSERT INTO roles (uid, tenant_id, scope, code, name, description, is_builtin) VALUES
  ('01M7QP5V01PKB0GJD6YK5GHNZY', NULL, 'platform', 'platform_admin', '平台超级管理员', '平台全部权限（含 tenant:impersonate）；安装向导创建的首个管理员获授此角色', 1),
  ('01M7QP5V02QZNJ17CD3WYPDME6', NULL, 'platform', 'platform_operator', '平台运维', '运维日常：节点/集群/发布/缓存/证书写操作与全部只读；不含用户、角色、系统设置与租户管理的写权限', 1),
  ('01M7QP5V03TSB3A6VR92V1GTM8', NULL, 'platform', 'platform_auditor', '平台审计员', '全部只读权限（不含第三方凭据），无任何写权限、不可代租户操作', 1),
  ('01M7QP5V04ZT8DFEF3DH4MZ0HF', NULL, 'tenant', 'tenant_owner', '租户所有者', '租户内全部权限（scope 为 tenant 或 both 的权限）', 1),
  ('01M7QP5V05FXDENXW99YVF792J', NULL, 'tenant', 'site_operator', '站点运维', '站点、域名、源站、规则、缓存、证书、发布与观测的读写（不含站点删除）及自有 API Token', 1),
  ('01M7QP5V06ZR9NCFE3TMZ0N5DZ', NULL, 'tenant', 'tenant_viewer', '租户只读成员', '租户内全部只读权限（不含第三方凭据 credential:read）', 1)
ON DUPLICATE KEY UPDATE code = code;

-- ---- 角色授权 ----

INSERT INTO role_permissions (role_id, permission_id)
SELECT r.id, p.id FROM roles r JOIN permissions p ON 1 = 1
WHERE r.scope = 'platform' AND r.tenant_id IS NULL AND r.code = 'platform_admin'
ON DUPLICATE KEY UPDATE role_permissions.role_id = role_permissions.role_id;

INSERT INTO role_permissions (role_id, permission_id)
SELECT r.id, p.id FROM roles r JOIN permissions p ON ((p.action = 'read' AND p.code <> 'credential:read') OR p.code IN ('cache:write', 'cache:purge', 'certificate:write', 'certificate:distribute', 'cluster:manage', 'cluster:bind', 'config:publish', 'config:rollback', 'domain:write', 'node:manage', 'node:enroll', 'node:upgrade', 'origin:write', 'rule:write', 'site:write', 'site:tls:write', 'task:cancel', 'probe:manage', 'token:manage', 'tenant:impersonate'))
WHERE r.scope = 'platform' AND r.tenant_id IS NULL AND r.code = 'platform_operator'
ON DUPLICATE KEY UPDATE role_permissions.role_id = role_permissions.role_id;

INSERT INTO role_permissions (role_id, permission_id)
SELECT r.id, p.id FROM roles r JOIN permissions p ON p.action = 'read' AND p.code <> 'credential:read'
WHERE r.scope = 'platform' AND r.tenant_id IS NULL AND r.code = 'platform_auditor'
ON DUPLICATE KEY UPDATE role_permissions.role_id = role_permissions.role_id;

INSERT INTO role_permissions (role_id, permission_id)
SELECT r.id, p.id FROM roles r JOIN permissions p ON p.scope IN ('tenant', 'both')
WHERE r.scope = 'tenant' AND r.tenant_id IS NULL AND r.code = 'tenant_owner'
ON DUPLICATE KEY UPDATE role_permissions.role_id = role_permissions.role_id;

INSERT INTO role_permissions (role_id, permission_id)
SELECT r.id, p.id FROM roles r JOIN permissions p ON p.scope IN ('tenant', 'both') AND SUBSTRING_INDEX(p.code, ':', 1) IN ('site', 'domain', 'origin', 'rule', 'cache', 'certificate', 'config', 'deployment', 'metrics', 'log', 'task', 'token') AND p.code <> 'site:delete'
WHERE r.scope = 'tenant' AND r.tenant_id IS NULL AND r.code = 'site_operator'
ON DUPLICATE KEY UPDATE role_permissions.role_id = role_permissions.role_id;

INSERT INTO role_permissions (role_id, permission_id)
SELECT r.id, p.id FROM roles r JOIN permissions p ON p.scope IN ('tenant', 'both') AND p.action = 'read' AND p.code <> 'credential:read'
WHERE r.scope = 'tenant' AND r.tenant_id IS NULL AND r.code = 'tenant_viewer'
ON DUPLICATE KEY UPDATE role_permissions.role_id = role_permissions.role_id;

-- ---- 默认租户（自用模式的唯一租户；is_default=1，至多一个；owner_user_id 由安装向导在创建管理员后回填）----
INSERT INTO tenants (uid, name, slug, status, is_default, locale) VALUES ('01M7QP5V346STC0AT56NME5NR3', '默认租户', 'default', 'active', 1, 'zh-CN')
ON DUPLICATE KEY UPDATE tenants.id = tenants.id;
