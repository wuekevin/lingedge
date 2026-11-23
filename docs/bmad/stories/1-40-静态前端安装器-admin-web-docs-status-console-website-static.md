---
title: 'Story 1.40：静态前端安装器（admin-web / docs / status / console / website-static）'
type: 'feature'
created: '2026-11-24'
status: 'review'
route: 'dispatch'
baseline_commit: '7203360'
waiting_env_verification: true
context:
  - docs/bmad/epics.md Story 1.40（AC1–AC5）
  - docs/architecture/tools-design.md §11.3、§12、IMP-17；docs/operations/release.md；docs/bmad/stories/1-33-*.md、1-34-*.md、1-35-*.md、1-38-*.md
---

## Intent

**Problem:** 1.33 的 22 步状态机、1.34 的发行流水线、1.38 的 Admin 工程都已就绪，但 `lingedge install admin-web` 仍以 `install.component_spec_missing` 失败：没有静态前端的 `ComponentSpec`、发行流水线不能打包前端（组件恒为 `unreleased`）、没有任何 Nginx 站点片段的生成/检查/接入，Admin/Console/官网的独立主机名也没有强制。另有两项清理：Dependabot #2（minimatch）与 `make vuln` 不覆盖前端依赖。

**Approach:** 不造新机制：一个生产 `staticSpec` 复用于五个前端（差别只在托管形态表）；渲染器是纯函数、输入全部是校验后的 `--set`；落盘、冲突不覆盖、journal 撤销、回滚全部复用 1.33 的步骤；conf.d 软链作为第 19 步里一个带 journal undo 的可选动作。发行流水线按 `-openresty-tree` 的同一模式新增 `-frontend-dist`。验证用**真实 nginx**加载安装器生成的片段并断言 HTTP/HTTPS 响应。**无迁移、无 OpenAPI/proto 改动（无 CC-14）、不改 CF-6/contract-v1/迁移 000001–000018。**

## 逐条验收

- **AC1**：`lingedge install admin-web --set host=…`（console/docs/status/website 同框架）：解压到 `/opt/lingedge/<c>/releases/<v>/public/`，`current` 原子切换；生成 `/etc/lingedge/nginx/<c>.conf` 与 `<c>-security-headers.conf`（域名/根目录/API 上游均为校验后的 Plan 值，渲染前先过严格字符集校验，原始字符串从不拼进配置）；预压缩 `.gz` 由发行流水线生成、片段 `gzip_static on`（真实 nginx 断言 `Content-Encoding: gzip` 且解压后与原文一致）；**既有 Nginx 配置零改动**（`TestStaticInstallLayoutAndNeverTouchesExistingNginx` 对整棵 `/etc/nginx` 前后快照逐字节比对，且未调用 nginx）。包布局在落地前校验（`payload/public/index.html`、顶层只有 `public/` 与 `share/`），不合格 → 第 14 步失败回滚。
- **AC2**：原生 Nginx（非宝塔、主配置在 `/etc/nginx` 且 include 了 `conf.d/*.conf`）：**询问**是否创建 `/etc/nginx/conf.d/lingedge-<c>.conf` 软链并 `nginx -t && nginx -s reload`（非交互用 `--set nginx-link=yes` 显式启用；`--yes` 只确认冲突，不隐式 reload；非交互默认不创建）；`-t` 或 reload 失败 → 撤销软链并带 nginx 自己的输出告警（`nginx.test_failed`/`nginx.reload_failed`），事务回滚（如第 20 步失败）按 journal undo 摘除软链并再 reload 一次，整棵系统树与安装前相同。宝塔环境一律只打印「新建站点并 include 该文件 / root 指向 current/public」指引（`baota_guide`），`nginx-link=yes` 在宝塔下以退出码 2 明确失败。独立的 `service.NewWebServerRunner` 只允许 `nginx`/`openresty`，**未放宽** `safeexec.DefaultAllowed` 与 service 的白名单。
- **AC3**：同名 conf（`/etc/lingedge/nginx/<c>.conf` 或 conf.d 同名项）与同域名站点（探测到的其他 server 块声明同一 `server_name`）→ 冲突（`file`/`site`），非交互无 `--yes` → 退出码 9 且**零改动**（连 `.new` 也不写）；确认后既有文件逐字节原样、新内容写 `.new`，同域名站点存在时不创建软链。计划之后才出现的同名文件同样不被覆盖（`TestStaticNginxLinkNeverOverwritesLateAppearingFile`）。`api-upstream` 可指向远端 Server（`http(s)://主机[:端口]`；https 须同时给 `api-ca`，不会不校验证书就代理）。
- **AC4**：安装后 `state.json` 记 `installed`、`list` 可见；真实 nginx 提供入口页、SPA 历史路由回落、哈希资源 immutable、`/api/` 同源反代（上游收到的 `X-Forwarded-For` 被覆盖为 `$remote_addr`，Host 原样，Cookie 不带 Domain）。**浏览器登录（AC-A-4 的 Admin 部分）未在本机经 Nginx 用 Chromium 复跑**——Admin 的登录/TOTP 浏览器 E2E 由 1.38/1.39 在真实 server 上完成，本 Story 验证的是 Tools→Nginx→静态/反代这一段；真实 Linux/宝塔上的浏览器验收属 1.45。`uninstall` 保留数据属 1.41（静态前端无数据目录）。
- **AC5**：Admin/Console/官网（及 docs/status）主机名互不相同：Plan 阶段最先（只读、无需网络）读取已装前端片段首行标记 `# lingedge-site: component=<c> host=<h>`，同主机名（不区分大小写）→ **退出码 2**，点名占用者，`--dry-run` 同样拒绝，**不存在绕过开关**（`allow-*` 之类的键是未知键，同样 2）；读不了已装前端的片段 → 失败关闭（3）。会话 Cookie host-only、realm 各异是 Server/前端侧既有事实（ADR-018、1.38 的 `le_admin_sid`），片段静态检查禁止 `proxy_cookie_domain` 改写与自行下发 `Set-Cookie`，真实 nginx 用例断言反代后的 Set-Cookie 无 `Domain`。安全头集中在 `/etc/lingedge/nginx/<c>-security-headers.conf`（每条 `always`），每个含 `add_header` 的 location 显式 `include`，server 级无 `add_header`；CI 静态检查 `tests/security/nginxsnippet`（随 `make security`）覆盖 5 组件 × 6 种设置组合与全部 golden，并带 8 处变异负向样本。官网额外：`/api/v1/public/` 之外一律 404，**`/api/v1/public/setup` 及子路径显式 404**。

## 发现的事实（须披露）

1. **tools-design §11.3 的「官网只放行 `location ^~ /api/v1/public/`，使 `/api/v1/public/setup/*` 不可达」自相矛盾**：`/public/setup/*` 就在 `/api/v1/public/` 前缀之内（openapi 有 `/public/setup/status` 等 9 个路径）。仅按原文生成会让安装向导接口经官网公网域可达。已加两条显式 404 location（`=` 与 `^~ …/setup/`），真实 nginx 用例验证上游收不到请求；tools-design 已补注。
2. **DNS 名上游在 nginx 载入配置时解析**：`api-upstream` 主机名解析不了则 `nginx -t` 失败（真实 nginx 暴露）。这是 nginx 的行为，`-t` 守卫了它；文档建议用 IP 或确保可解析。
3. **`ssl_protocols TLSv1.3`**（threat-model 要求最低 1.3）经真实 nginx + 运行时自签证书验证：TLS 1.2 握手被拒。

## 发行流水线（`scripts/release`）

新增 `-frontend-dist <组件>=<目录>`（可重复；admin-web|console|docs|status|website，website 变体 `static`）：要求 `index.html`、重跑 `frontend/scripts/check-mock.mjs --dist`（缺 node 即失败）、读取 `frontend/pnpm-lock.yaml` 哈希写入 `build-record.json` 与包内 SBOM、复制到 `payload/public/`（含符号链接即失败）、确定性预压缩 `.gz`、平台 `any`。未提供目录的组件保持 `unreleased`（docs/status 目前没有产物源、console 是基座探针、website 属 Epic 2——不发布空壳包）。流水线**不能证明**目录确由该提交构建（不重新构建前端），见 release.md §12。

## 测试（全部真跑；伪的只有「操作系统」与观察转发用的 API 上游）

- `internal/tools/installer/static_test.go`：渲染 golden 8 份 + 结构断言（API 范围）、lint 负向 8 例 + 头文件 3 例、`--set` 校验 27 个负例（含配置注入、IP、通配符、非 ASCII、不回显取值）、规格契约与包布局校验、完整安装布局与「不触碰既有 Nginx」、五组件全装、独立主机名（含大小写、dry-run、读不了即失败关闭、无绕过键）、同名 conf/同域名站点不覆盖、软链全部场景（显式/交互是否/非交互默认/`-t` 失败/reload 失败/同名既有/宝塔/无 conf.d/`--skip-start`/计划后出现的文件）、回滚摘除软链并再 reload 且整树与安装前相同、dry-run 零改动、包布局不符落地前拒绝。
- `internal/tools/cli/static_cmd_test.go`：命令层退出码（2/9）、envelope 与 `data.install` schema、dry-run、软链、人类可读输出、`list`。
- `tests/release/frontend_test.go`：**发行流水线 → 离线签名 → Tools 验签链 → 生产规格安装 → 真实 nginx 加载并服务**；admin-web 包来自**真实 `frontend/dist/admin`**（存在时；否则夹具并在日志标注），website 为夹具。真实 nginx 断言：入口页/独立主机名、SPA 回落、gzip_static、immutable、404 也带安全头、点文件、`/api/` 反代（XFF 覆盖、Host、Cookie host-only）、官网 public-only 与 setup 404（被禁路径上游收到 0 次）、`/_nuxt/`、TLS（301、TLS1.3 only、HSTS）；`nginx -t` 接受全部渲染组合（5 组件 × 默认/TLS/https 上游/关 IPv6/自定义端口+IPv6 上游）；流水线目录守卫（无 index.html、含 mock 标记、非法参数）。
- `tests/security/nginxsnippet`：CI 静态检查（见 AC5）。

## 变异验证

对关键防线临时改动 → 对应测试必须失败 → 还原：官网 setup 404 删除（真实 nginx 与结构两处检出）、XFF 改为追加、public-only 改整体反代、安全头去 `always`、独立主机名检查关闭、主机名不规范化、`-t` 失败不撤销软链、回滚不 reload、conf.d 同名既有文件被覆盖、非交互默认创建软链、包布局校验跳过——**全部检出**（其中「包布局校验跳过」与「覆盖既有软链」初次**存活**，据此补了 `TestStaticBrokenPackageLayoutIsRejectedBeforeLanding` 与 `TestStaticNginxLinkNeverOverwritesLateAppearingFile` 后检出）。「宝塔下允许软链」的变异是等价变异（宝塔分支在到达该判断前已返回），未算作检出。

## 清理项

- **Dependabot #2（minimatch，GHSA-g3hv-75wg-m249，medium，glob 展开 DoS，漏洞区间 `>=7.0.0 <11.0.1`）**：`frontend/pnpm-workspace.yaml` 新增 `overrides: 'minimatch@>=7.0.0 <11.0.1': 11.0.9`（精确版本，锁里已有的 11.x，同被 eslint 链使用），刷新 `pnpm-lock.yaml`（`minimatch@10.2.6` 消失；5.x 在 openapi-typescript 链上、不在区间，保持不动）。不改 Node/pnpm 版本矩阵。`make frontend-check` 全绿（vitest、build、隔离、mock）。
- **`make vuln` 覆盖前端**：评估后加入 `frontend-audit`（`pnpm audit --audit-level moderate`，含开发依赖，无豁免）。**联网**且结果随公告库变化（与 govulncheck 同类；联网失败按失败处理）；可确定、可复现的部分（锁文件固定、overrides 精确版本由 `check:deps` 校验、`--frozen-lockfile`）本就在 `frontend-check` 内。落地时 `pnpm audit` 实测还报出同链的 **glob GHSA-3g3m-2fjr-6pw4（moderate）**——旧锁文件下退出码 1（同时命中 minimatch 与 glob），新锁文件退出码 0；一并用 `glob@>=9.0.0 <14.0.2 → 14.0.6` 覆盖（只经 js-beautify，test-utils 的开发依赖，只用其 html 美化函数）。

## 对既有测试/防护的改动（逐条，均为随功能交付的必要更新，无放宽）

| 文件 | 改动 |
|---|---|
| `tests/security/toolsbinary/binary_test.go` | 生产规格断言由「恰为 server」改为「恰为已交付的 admin-web,console,docs,server,status,website」，仍禁止多登记、仍断言类型不是测试替身 |
| `internal/tools/cli/validate_test.go` | `install website --mode ssr` 的合法组合补 `--set host=www.example.com`（website 现为静态前端规格，host 必填；仍期望 5 进入清单验证） |
| `tests/release/e2e_test.go` | 共享演练带 `-frontend-dist`；`admin-web`/`website` 断言由 unreleased 改为 available，其余（console/docs/status/dns/…）仍断言 unreleased |
| `internal/tools/installer/helpers_test.go` | `sysSnap` 的排除项追加伪 nginx 调用记录（与 `fake/reloads` 同类） |
| `internal/tools/cli/schema/data.install.schema.json` | 追加式：访问类型 `site_url`/`nginx_include`/`baota_guide`，冲突类型 `site` |
| `tests/toolsworld/world.go` | 追加 `Options.Static`/`StaticBrokenLayout`、伪 nginx、`WebRunner`；缺省世界不变 |
| `scripts/gates` 等门禁规则与词表、`.golangci.yml`、`safeexec.DefaultAllowed`、`SafeExtract` 限额 | **未改动** |

## 属后续 Story / 代码已完成待环境验证

**代码已完成，待环境验证（ENV-LNX / ENV-BT，汇总于 1.45）**：真实 Linux 发行版（Debian/Ubuntu、RHEL 系）的系统 Nginx 经 `conf.d` 软链 + `nginx -t && reload` 的真实行为（本机用 homebrew nginx 验证了「片段被接受并正确服务」与伪 nginx 验证了软链/撤销编排，未经真实发行版 nginx 的 reload）；真实宝塔面板上「新建站点并 include」的人工路径；真实公网证书的 HTTPS（本机仅自签证书）；浏览器（含 Safari）经 Nginx 的 Admin 登录与 Secure Cookie；各发行版 nginx 是否带 `gzip_static`/`http2` 的 listen 参数（缺失时 `nginx -t` 会明确报错）。

**属后续 Story**：`uninstall` 保留数据（1.41）；前端升级/回滚（1.43）；`doctor`/`list` 对前端的深度检查（1.42）；website 的 ssr 变体、`runtime-config.js`、自用模式 `/pricing` 404、公开媒体目录（2.5/2.14）；docs/status/console 的真实构建产物与其发行（对应各自 Story，目前流水线对它们如实 `unreleased`）；真实发行密钥/发行源（UD-3）。

## 门禁结果

见 sprint-status 的 `done_locally` 与最终报告（以最后一次完整运行为准）。
