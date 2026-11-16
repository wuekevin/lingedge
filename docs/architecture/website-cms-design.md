---
title: "LingEdge 官网与 CMS 技术设计"
status: draft
created: 2026-11-16
updated: 2026-11-16
inputs:
  - docs/prd/LINGEDGE_V2_REQUIREMENTS.md   # §5、§21.4
  - docs/prd/prd.md                        # FR-40..FR-46、D-8、R-12、F-5
  - docs/architecture/architecture.md
  - docs/ux/website.md                     # 路由、数据真实性规范、能力状态（UX 为准，本文落实技术）
related-adr: [ADR-014, ADR-015, ADR-017, ADR-019]
---

# 官网与 CMS 技术设计

> 视觉与体验规范以 `docs/ux/DESIGN.md`、`docs/ux/website.md` 为准，本文不重复；本文只定**技术落点**：工程结构、渲染模式、数据流、内容模型要点、安全、发布与缓存、接口映射、测试。
> 阶段：**M1** = 静态预渲染（SSG）官网框架，无 CMS；**P2** = CMS + SSR；P4/P5 的内容性约束见 §13。

## 1. 定位与模块

官网（Website）是面向访客的完整品牌站（基线 §5.1），不是着陆页；技术上是 **Nuxt 3 + Vue 3 + TypeScript + Tailwind CSS** 的独立前端工程，与 Admin/Console（Arco Design Pro）**不共享业务代码**（ADR-014）。

| 模块 | 阶段 | 职责 |
|---|---|---|
| 站点框架（导航、页脚、主题、i18n、SEO） | M1 | 路由、布局、统一组件、能力状态徽标、示意角标 |
| 内容页（首页/产品/解决方案/价格/开发者/帮助/关于） | M1 | 静态文案来自仓库内 i18n 内容文件，随构建发布 |
| 真实数据模块（节点数、节点地图等） | M1 | 发行产物为「暂无数据」占位 + 浏览器端读取 `public` 接口（§3.4）；构建时快照仅限运营者自建 |
| 登录/注册/控制台入口 | M1 | 随运行模式（§4.5） |
| 官网 CMS（Admin「官网运营」+ Server `cms/seo/media` 模块） | P2 | 草稿/预览/发布/定时/版本/回滚/多语言/SEO/媒体 |
| SSR 运行形态 | P2 | Nitro `node-server`，systemd 托管，显式 Node.js 依赖 |
| 新闻/博客/FAQ/套餐展示 | P2（套餐数据 P5） | CMS 驱动 |

## 2. 工程结构（`frontend/website`，Nuxt 3 约定目录）

```
frontend/website/
├── nuxt.config.ts            # 模块、i18n、routeRules、nitro preset、runtimeConfig
├── app.vue
├── layouts/                  # default、minimal（登录跳转/维护页）
├── pages/                    # 文件路由（见 §3 路由清单；zh 无前缀，en 前缀由 i18n 模块生成）
├── components/
│   ├── blocks/               # CMS 区块渲染组件（与 §10 区块类型一一对应，M1 也复用）
│   ├── common/               # Nav、Footer、LangSwitch、ThemeSwitch、CapabilityBadge、IllustrativeBadge、DataSource
│   └── ...
├── composables/              # useRuntimeSite()、usePublicStats()、useCms()
├── content/                  # M1：站点静态内容（i18n 文案 JSON/MD，非 CMS），含 data_source 声明
├── i18n/locales/{zh-CN,en}.json
├── plugins/                  # runtime-config 读取、主题
├── public/                   # 静态资源、robots.txt 模板
├── server/                   # P2：Nitro server routes（_lingedge/revalidate、预览代理）、中间件
├── scripts/                  # check-hardcoded-metrics.ts、check-nav-reachable.ts、check-capability-claims.ts
├── tests/{unit,e2e}/
└── tailwind.config.ts
```

- 使用 Tailwind 的方式（官方模块 / Vite 插件）、`@nuxtjs/i18n`、`@nuxtjs/sitemap` 等模块与 **Nuxt 3.x 线（npm tag `3x`，查询时 3.21.11）的兼容版本，实现时核对并以 lockfile 固定**；基线要求 Nuxt 3，故不跟随 `latest`（4.x）。TypeScript 版本以 Nuxt 3/`vue-tsc` 兼容范围为准（实现时核对）。
- 与 `frontend/shared` 的关系：**仅**使用无业务语义的包（设计令牌、i18n 工具、由 OpenAPI 生成的 `public` 接口类型）；Arco 组件不进入官网产物。
- 视觉令牌（深蓝科技风、蓝紫渐变、深/浅主题）取自 `docs/ux/DESIGN.md` 的令牌文件，映射为 Tailwind theme；主题切换用 `data-theme` + `prefers-color-scheme` 初值，选择记忆于 `localStorage`（读写均 try/catch）。

## 3. M1：静态预渲染（SSG）

### 3.1 路由清单（基线 §5.2 全部入口；与 `docs/ux/website.md` §2 对齐）

| 路由（zh 无前缀；en 加 `/en`） | 内容 | M1 状态 |
|---|---|---|
| `/` | 首页（基线 §5.3 的 13 个区块） | 完整 |
| `/products` | 产品总览 | 完整 |
| `/products/cdn` `/products/waf` `/products/dns` `/products/l4` `/products/ssl` `/products/analytics` | 六个产品页（带**能力状态徽标**） | 内容完整；能力状态如实（WAF/DNS/L4 为「规划中」，直到对应阶段） |
| `/solutions` `/solutions/website-acceleration` `/solutions/static-assets` `/solutions/api-acceleration` `/solutions/video` `/solutions/cross-border` `/solutions/enterprise-security` | 解决方案总览 + 6 页 | 完整（不得暗示未交付能力） |
| `/pricing`（含 `#plans` `#traffic` `#bandwidth` 三区） | 价格：套餐价格、流量计费、带宽计费 | M1 框架：**无 `plans` 数据时显示「即将提供」**；自用模式整页隐藏（§4.5） |
| `/developers` `/developers/api` `/developers/guides` `/developers/changelog` | 开发者 | 框架；Docs 未安装（P5 前）时 `api/guides/changelog` 显示「文档站即将提供」，**不跳转到不存在的地址** |
| `/help` `/help/docs` `/help/faq` `/help/tickets` | 帮助中心 | `faq` 为静态 FAQ；`tickets`（工单 P5）前隐藏或「即将提供」 |
| `/about` | 关于我们 | 模板；正文由用户提供 |
| `/news` `/blog` | 新闻公告 / 博客 | P2 |
| `/legal/*` | 隐私/条款 | 待用户提供，不起草 |
| 登录 / 注册 / 控制台 | **外部链接**（不是本站页面） | §4.5 |
| `/404`、`/500`、维护页 | 品牌化错误页 | 完整 |

构建检查脚本 `check-nav-reachable.ts`：导航树中每个目标要么对应已生成路由，要么被标注为「即将提供」；不存在悬空链接（FR-40）。

### 3.2 i18n URL 策略（决策）

- **zh-CN 为默认语言，无前缀**；**en 使用 `/en/` 前缀**（策略 `prefix_except_default`，与 `docs/ux/website.md` §7 的 `[ASSUMPTION]` 一致）。理由：品牌与首批运营者为中文场景；默认语言路径最短、已有外链不变；可被推翻（改为全部带前缀只影响 `nuxt.config.ts` 与 `hreflang` 生成）。
- **不做服务端/构建时自动跳转**（SSG 无法按 `Accept-Language` 响应）；首次访问若浏览器语言与当前不同，客户端显示一条**非阻塞的语言建议条**；用户选择记忆于 `localStorage`（try/catch，缺失时不影响渲染）。
- 缺失翻译键：构建期检测（`check-i18n-keys`，NFR-29）阻断；运行期回退到 zh-CN 并在开发模式告警。

### 3.3 主题、SEO

- 主题：亮/暗，**Hero/CTA 渐变区两种主题下都保持深蓝基调**（UX §8）；初始值通过阻塞的内联小脚本在首屏前设置 `data-theme`，避免闪烁（该脚本经 CSP hash，§11.4）。
- SEO（FR-42）：每页 `title/description`（来自 i18n 内容）、OG/Twitter 元数据、`<link rel="canonical">`、`hreflang`（zh/en 互指 + `x-default`）、`sitemap.xml`（构建时生成，含 alternates；**自用模式下由 Tools 生成的 Nginx 片段对 `/pricing` 返回 404 并从 sitemap 剔除**，见 §4.5）、`robots.txt`、语义化标题层级、结构化数据仅输出真实内容（Organization；Product 仅在能力为「已提供」时）。
- 无 JS 可读：SSG 产物 HTML 含全部正文（FR-42 可验证项：`curl` 抓取后能在 HTML 中检索到正文）。

### 3.4 真实数据原则落地（FR-41、F-5、UX §3）

目标：**页面上任何数字都能回答「来源/时间/谁能验证」**；无法回答则不出现或标「示意」。

1. **数据源声明**：内容文件中每个数据模块必须声明 `data_source ∈ {live, snapshot, illustrative}`，`live/snapshot` 必须绑定公开数据源标识（如 `public.nodes.online_count`），`illustrative` 不得绑定数字。缺省/不合法 → **构建失败**。
2. **构建时快照仅限运营者自建，发行产物一律禁用**（IMP-17）：官方发行流水线构建的 `static` 变体**不调用** `public/stats`（构建参数缺省 `--stats-snapshot=off`），`live` 模块在产物 HTML 中一律渲染「暂无数据」占位——因为发行产物会被分发到所有部署，快照来自 CI 所连的 Server，会在浏览器端覆盖前显示别人（或过期）的数字，违反 FR-41。运营者自建（显式 `--stats-snapshot`）时才在 `nuxt generate` 调用其自己的 Server，产物含快照值 + 「截至 <构建时间>（构建时快照）」；Server 不可达则同样渲染「暂无数据」，**不使用任何缓存的旧数字或示例数字**。
3. **浏览器端实时覆盖**：页面加载后客户端 `usePublicStats()` 读取同源 `/api/v1/public/stats`（Nginx 仅把 `/api/v1/public/` 反代到 Server，或配置 CORS 白名单）；成功 → 数字切换为「实时 · 更新于 <时间> · 来源」；失败 → 保持占位（自建快照模式下保持快照并显示快照时间戳）；**从不静默沿用旧值**。
4. **公共接口形态**（OpenAPI `public` 模块）：`GET /api/v1/public/stats` 只返回聚合、脱敏数据：`{ generated_at, nodes: { online_count, regions: [{ code, city?, ... }] } }`；**不含 IP、机房、运营商**；节点地图点数必须等于 `online_count`（UX §3.2-3）。可用率、性能指标字段**在 Status（P5）与《性能基线报告》存在之前不提供**，因此官网无可展示的可用率（FR-41、NFR-31）。
5. **CI 防线**（`check-hardcoded-metrics.ts`，生产构建门禁）：
   - 扫描 `content/`、`components/`、i18n 文案：出现形如 `\d+(\.\d+)?\s*%`、`\d+\s*(ms|毫秒|Gbps|Tbps|Mbps)`、`\d+\s*(个)?节点` 的字面量且未位于 `data-source` 组件或带 `illustrative` 标注的上下文 → 失败；
   - 扫描 `dist/**/*.html`：同样模式在无 `data-source`/`data-illustrative` 祖先元素时失败；
   - 禁用词表（UX §9：「无惧任何攻击」「全球最快」「100% 可用」等）命中失败。
6. **示意**：`IllustrativeBadge` 组件强制渲染（虚线轮廓 + 「示意」+ `aria-label`「示意数据，非实际」），内容作者无法移除；示意版节点地图用图形而非数字。

### 3.5 注册/登录/控制台入口随运行模式（FR-46）

构建产物对运行模式**不可知**（同一产物可用于自用与商业部署），因此入口由**安装时生成的运行时配置**决定：

- `lingedge install website` 写入 `/etc/lingedge/website/runtime-config.js`（Nginx 以 `location = /runtime-config.js` 提供，`Cache-Control: no-store`），内容形如：
  `window.__LE_RUNTIME__ = { "mode": "self_hosted", "admin_url": "https://…", "console_url": null, "registration_open": false, "docs_url": null, "status_url": null }`
  值来自 Tools 的安装参数与 Server 公共接口 `GET /api/v1/public/site-info`（`mode`、`console_installed`、`registration_open` 等）；Tools 在运行模式切换后（`lingedge config …`）重写该文件。
- `index.html` 在 `<head>` 以**阻塞脚本**加载它并设置 `<html data-mode="…">`；少量 CSS（`[data-mode="self_hosted"] .only-commercial{display:none}`）在首屏前隐藏商业专属入口，避免闪烁；组件随后读取同一对象渲染链接。
- 规则（与 `docs/ux/website.md` §5 一致）：自用——「登录」「控制台」指向 Admin，**隐藏「注册」与价格类入口**；商业——「登录/注册/控制台」指向 Console；Console 未安装则隐藏而非 404；已登录判定**不**在官网实现（跳转后由目标站处理）。
- 自用模式的 `/pricing`：Tools 生成的 Nginx 片段含 `location ^~ /pricing { return 404; }`（及 `/en/pricing`），同时 sitemap 不含；即使 HTML 被直接请求也不暴露价格页。

### 3.6 构建与发行

- 构建：`pnpm --filter website generate` → `.output/public`（Nuxt 3 SSG 产物）。CI 在构建机上完成，**服务器安装时不需要 Node.js**（D-8、FR-14）。**发行构建禁用统计快照**（§3.4(2)），同一份签名产物可安全分发到所有部署与运行模式。
- 产物打包为 `website` 组件的 `static` 变体（平台 `any`），进入签名清单（tools-design §9.2 的 `variants.static`）；Tools 解压到 `/opt/lingedge/website/releases/<v>/public`，`current` 符号链接原子切换。
- 预压缩：`compressPublicAssets` 生成 `.gz/.br`；Nginx 使用 `gzip_static on;`，`brotli_static`（仅当模块存在，由 Tools 检测，否则不写该指令）。
- Nginx 片段（Tools 生成于 `/etc/lingedge/nginx/website.conf`，值经校验，**不写入宝塔站点配置**，tools-design §11.3）：

```nginx
server {
    listen 443 ssl;
    http2 on;
    server_name example.com;                       # 由 Plan 校验后的域名（官网主机名，必须不同于 Admin/Console 主机名）
    root /opt/lingedge/website/current/public;
    index index.html;

    # 注意（IMP-17）：nginx 的 add_header 一旦在 location 内出现任何一条，就不再继承上级。
    # 因此安全头（CSP/nosniff/HSTS 等，全部带 always，见 §11.4）**必须 include 进每一个含 add_header 的 location**，
    # server 级的 include 仅作兜底（对不含 add_header 的 location 生效）。
    include /etc/lingedge/nginx/website-security-headers.conf;

    location = /runtime-config.js {
        alias /etc/lingedge/website/runtime-config.js;
        include /etc/lingedge/nginx/website-security-headers.conf;
        add_header Cache-Control "no-store" always;
    }
    location /_nuxt/ {                              # 带内容哈希的资源
        include /etc/lingedge/nginx/website-security-headers.conf;
        add_header Cache-Control "public, max-age=31536000, immutable" always;
        try_files $uri =404;
    }
    location ^~ /api/v1/public/ {                   # 仅放行公开只读接口
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $remote_addr;   # 覆盖而非追加，不信任访客值
        proxy_set_header X-Real-IP $remote_addr;
    }
    location ^~ /api/ {                             # 其余 /api/（admin、auth、setup 等）在官网公网域一律不可达
        return 404;
    }
    location / {
        include /etc/lingedge/nginx/website-security-headers.conf;
        add_header Cache-Control "no-cache" always; # HTML 每次校验
        try_files $uri $uri/index.html =404;
    }
    error_page 404 /404.html;
}
```

- **独立主机名**（IMP-17 / ADR-014 §5）：Tools 在 `install website|admin-web|console` 的预检与 Plan 中校验三者的 `server_name` **互不相同**（相同则退出码 3），Cookie 为 host-only（不设 `Domain`），realm 隔离因此同时有“Cookie 名 + 主机”两层，官网公网域不再暴露 `/api/v1/admin/*`、`/auth/*`、`/public/setup/*`。租户数据面域名与控制面域名亦不得处于同一注册域（threat-model §5.7）。

## 4. P2：SSR 运行形态

### 4.1 为什么 P2 起默认 SSR（D-8）

CMS 的预览、定时发布、内容版本回滚在 SSR 下实现更直接：发布后无需重建整站。保留「发布触发重新生成静态」作为降级（§4.6）。

### 4.2 运行与部署

- Nitro preset `node-server`；产物目录 `.output/`（`server/index.mjs` + `public/`）。包为 `website` 组件的 `ssr` 变体（平台 `any`，**依赖 Node.js 运行时**）。
- **必须明确声明并检测 Node.js 依赖（基线 §21.4）**：清单 `requires.runtime.node` 给出版本范围（发布时按 Nuxt 3.x 的引擎要求填写）；`lingedge install website --mode ssr` 在预检步骤探测 `node`（绝对路径、版本），缺失/版本不满足 → 退出码 3 并说明；**不会**把 Nuxt SSR 当作 Go 二进制运行，也不静默安装 Node.js。本机开发环境 Node v26.8.2 的兼容性**实现时核对**。
- systemd：`lingedge-website.service`（tools-design §15.4），监听 `127.0.0.1:3000`，Nginx 反代：

```nginx
location / {
    proxy_pass http://127.0.0.1:3000;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $remote_addr;
    proxy_set_header X-Forwarded-Proto $scheme;
}
location ^~ /_lingedge/ { return 404; }            # revalidate 等内部路由对公网不可达（§4.3）
location ^~ /api/v1/public/ { proxy_pass http://127.0.0.1:8080; proxy_set_header Host $host; proxy_set_header X-Forwarded-For $remote_addr; }
location ^~ /api/ { return 404; }                  # 与 §3.6 相同：官网域不暴露非 public 接口
location /_nuxt/ { alias /opt/lingedge/website/current/public/_nuxt/; include /etc/lingedge/nginx/website-security-headers.conf; add_header Cache-Control "public, max-age=31536000, immutable" always; }
location /media/  { alias /var/lib/lingedge/media/; include /etc/lingedge/nginx/website-security-headers.conf; add_header Cache-Control "public, max-age=31536000, immutable" always; }   # §12.3
```

（SSR 模式下 Nginx 片段同样遵守 §3.6 的“add_header 不继承”规则与独立主机名约束；公开媒体目录 `/var/lib/lingedge/media/` 位于 `/var/lib/lingedge/server/` 之外，权限 0755（目录）/0644（文件），Nginx 运行用户可读，理由见 §12.3。）

### 4.3 内容获取、缓存与失效

- SSR 服务端经**内部地址**读取 Server：`GET {WEBSITE_API_INTERNAL}/api/v1/public/cms/...`（只读、无认证、仅返回**已发布**内容；`runtimeConfig` 私有项，不暴露到浏览器）。
- 渲染结果缓存：Nitro `routeRules` 对公开页使用 `swr`（默认 TTL **配置项**，建议值待基线测试后确定）；预览路径、带 token 请求**一律不缓存**。
- **发布即失效**：Server 在 `cms.publish`/回滚/定时翻转后由 Worker 任务 `cms.revalidate` **直连 `127.0.0.1:3000`**（不经 Nginx）调用 Website 的 `POST /_lingedge/revalidate`（body：受影响路径或内容键；头：`X-LingEdge-Signature: HMAC-SHA256(secret, timestamp|body)` + `X-LingEdge-Timestamp`，±5 分钟窗口）。**该路由唯一有效的防线是 HMAC 签名**：经 Nginx 反代后来源恒为回环，“仅接受回环来源”的检查没有意义（MIN-08），故 Nginx 对公网 `location ^~ /_lingedge/ { return 404; }`（§4.2），仅 Server 的直连可达；失败由任务重试（幂等）。即使 webhook 丢失，SWR 的 TTL 也使最终一致。
- **revalidate 密钥分发**（M17）：共享密钥 `website.revalidate_secret`（≥32B 随机）由 Tools 在 `install website --mode ssr` 时生成，写入 Website（`/etc/lingedge/website/revalidate.secret`，root:lingedge-web 0440，经 systemd `LoadCredential` 或 `EnvironmentFile` 注入 SSR 进程），同时登记到 Server：Server 与 Website 同机时由 Tools 调 `lingedge-server config set website.revalidate_secret --stdin` 写入 `system_settings.value_enc`（信封加密，ADR-011）；Server 远端时由运维在 Admin「官网设置」粘贴同一值。轮换＝重新生成并双端更新（过渡期 Website 接受新旧两个值，窗口≤1 个 SWR TTL）。不再使用“由 KeyProvider 派生”的说法（SSR 进程无 master.key，无法派生）。
- SSR 必须保证 **FR-41 的数据真实性规则同样成立**：服务端读取 `public/stats`，失败则渲染「暂无数据」，不使用进程内旧值。

### 4.4 预览（FR-44）

1. Admin 调用 `POST /api/v1/admin/cms/pages/{uid}/preview-links`（权限 `cms:page:write`；其它实体的预览链接 operation 在 P2 补齐，路径形态一致，OpenAPI 事实源）→ Server 生成**不透明随机预览令牌**（≥32B CSPRNG，与会话/API Token 同一模式，ADR-018）：库内仅存 `SHA-256` 于 `website_preview_links`（`entity_type, entity_id, version_id, locale, token_hash, expires_at, revoked_at`，database/cms.md §6.2），明文只在创建时返回一次；`expires_at` 默认 ≤ 30 分钟（官网设置可调，UX §6）。**不再使用签名令牌/`jti` 黑名单**：撤销＝置 `revoked_at`，一个版本可有多个并存链接。
2. **令牌不进入 URL query**（MIN-08：query 会进 access log 与 Referer）：链接形如 `https://site/__preview#t=<token>`（fragment 不发送给服务器）；`/__preview` 页面脚本读取 fragment，立即 `POST /__preview/exchange`（Website 自己的 SSR 路由，body 含令牌）→ Website 以 `X-Preview-Token` 头调用 Server **既有**的 `GET /api/v1/public/cms/preview`（OpenAPI `publicGetCmsPreview`）校验（哈希匹配、未过期、未撤销），成功后以 `Set-Cookie: le_preview=<该不透明令牌>; HttpOnly; Secure; SameSite=Strict; Path=/__preview…; Max-Age≤令牌剩余寿命`（令牌只存在于 HttpOnly cookie，不进 URL/日志/Referer），随后跳转到干净 URL；后续预览请求带该 cookie，Website 把它作为 `X-Preview-Token` 转给 Server 换取**对应草稿版本**内容。令牌即能力，不要求登录；**Server 不需要额外的 exchange 端点**（修订后与 OpenAPI 一致，评审 M17）。
3. 预览响应：`Cache-Control: private, no-store`、`X-Robots-Tag: noindex, nofollow`、`<meta name="robots" content="noindex">`；预览不经过 SWR、不写入任何缓存、不进入 sitemap；带 `le_preview` cookie 的请求一律绕过 SWR。
4. 静态降级模式（§4.6）下，预览域始终由 SSR 进程服务（仅用于预览与管理员路径），公网流量走静态目录。

### 4.5 定时发布

- 内容实体有 `published_version_id`（当前线上）与 `scheduled_version_id + scheduled_publish_at`（database/cms.md；`status='scheduled'`）。Worker 周期任务 `cms.publish_due`（经 `job_locks` 租约选主，ADR-019；**幂等**）每分钟以数据库 `NOW(3)` 为准检查 `scheduled_publish_at <= now`：在事务中**再次校验引用**（媒体、套餐、被链接页面均存在且已发布；失败则不发布，写入 `schedule_error`（原因码）并通知，**不静默失败**，UX §6；状态保持 `scheduled` 且 UI 显示“排期失败”，运营者可改期/取消）→ 翻转 `published_version_id`、清空 `scheduled_version_id/scheduled_publish_at/schedule_error`、写审计 → 发出 `cms.published` 事件 → 入队 `cms.revalidate`（或静态降级下的 `cms.prerender`）。
- 取消/改期定时同样审计。

### 4.6 静态降级模式（发布触发重新生成）

适用于希望公网流量零 Node 依赖、仅发布时才需要 Node 的场景（可被推翻，**PoC 项**）：
- 公网流量：Nginx `try_files $uri $uri/index.html @ssr`，优先命中 `/var/lib/lingedge/website/static/`，未命中才回源 SSR。
- 发布/回滚/定时翻转 → Worker 任务 `cms.prerender`：读取路由集合（sitemap + 内容实体列表），逐路由对**本机 SSR 实例**发起请求，把 HTML 与产物写入临时目录，**全部成功后原子换目录**（`rename`）；失败保留旧静态目录并重试，Admin 显示「正在重新生成静态页面（N/M）」与失败重试（UX §6）。
- 该模式不是 Nuxt 内建能力，属自定义爬取式预渲染；上线前须 PoC（并发、增量、一致性），未验证前默认 SSR 模式。

## 5. CMS 数据模型要点

> 表结构以 [`docs/database/cms.md`](../database/cms.md) 为准（基线 §18 的 `website_*` 14 张表，整体 **P2**；M1 官网为纯静态，不依赖这些表）。本节只给出**设计约束**，字段细节若与数据库文档不一致，以数据库文档为准并回写本文。

- **内容实体**：页面（`website_pages`）、文章（`website_articles`，含新闻公告/博客，经 `website_categories` 分类）、FAQ（`website_faqs`）、产品展示（`website_products`，引用正式 `products`）、解决方案（`website_solutions`）、Banner（`website_banners`）、导航（`website_navigation`，header/footer，树形）、媒体（`website_media`）、SEO（`website_seo`，多态指向实体+locale）、站点设置（`website_settings`）、多语言内容（`website_localizations`，实体×locale）。
- **版本**：页面有版本表（`website_page_versions`）+ 区块表（`website_page_blocks`，按 `sort` 排序）；其他内容实体的版本复用统一内容版本机制（数据库文档中的 `website_content_versions`）。**已发布的版本不可变**（Repository 无更新方法 + 触发器保护，同 ADR-002 的不可变哲学）；草稿可就地编辑（乐观锁 `version_no` 冲突检测，UX 的「编辑冲突」对话框）。
- **状态机**（所有内容类型共用，状态词与 UX 一致）：`draft → scheduled | published → (archived)`；每次发布冻结当前草稿为不可变版本，并更新 `published_version_id` 指针。
- **多语言**：每个版本按 locale 持有内容；`zh-CN`、`en` 为 M1 固定集，后续语言经 `website_settings` 扩展；缺失翻译按「回退 zh-CN + 在官网概览/多语言内容矩阵标红」处理。
- **与套餐的关系（R-12）**：`website_*` 表**没有任何结算价格字段**；`plan_table` 区块只存放 `plan_uid` 引用与展示选项（顺序、推荐标记、是否显示），价格由 `GET /api/v1/public/plans` 实时读 `products/plans`。

### 5.1 内容版本与回滚

- 发布 = 把草稿冻结为 `version_no = n+1` 的不可变版本并上线。
- **回滚 = 以目标旧版本的内容复制出一个新版本（`rolled_back_from` 指向旧版）并发布**，历史不被改写；回滚前同样做引用校验，回滚动作写审计并触发 revalidate/prerender（AC-I-5）。
- 版本保留策略随 `data retention`（FR-28）配置，默认保留全部（内容体量小）；被媒体引用的文件不随旧版本清理。

## 6. Admin「官网运营」菜单 → API → 权限映射（P2）

约定：路径前缀 `/api/v1/admin/cms`（基线 §19 的 `admin/cms` 模块）；认证为 Admin 会话（realm `admin`）；写操作需 `X-CSRF-Token`；均写审计。权限串 `cms:<scope>:<action>`（RBAC，ADR-018）。**机器可读的最终路径以 `docs/api/openapi.yaml` 为准**，本表为设计意图（已与 openapi.yaml 的 `admin/cms` 标签逐项核对一致：路径、权限串 `cms:read|page:write|content:write|plan-display:write|media:write|seo:write|nav:write|settings:write|publish` 均相同；OpenAPI 中路径参数命名为 `pageUid/productUid/…/entityType/entityUid`）。

| Admin 菜单 | API（相对 `/api/v1/admin/cms`） | 读权限 | 写/发布权限 |
|---|---|---|---|
| 官网概览 | `GET /overview`（待发布数、定时队列、最近发布/回滚、翻译完整度、示意数据模块清单） | `cms:read` | — |
| 首页装修 | `GET/PUT /pages/{uid}/draft`（`slug=home`）、`GET/POST/DELETE /banners`、`POST /pages/{uid}/publish`、`POST /pages/{uid}/schedule`、`DELETE /pages/{uid}/schedule`、`GET /pages/{uid}/versions`、`POST /pages/{uid}/rollback`、`POST /pages/{uid}/preview-links` | `cms:read` | `cms:page:write`、发布/定时/回滚 `cms:publish` |
| 页面管理 | `GET/POST /pages`、`GET/PATCH/DELETE /pages/{uid}`（同上草稿/版本/发布接口） | `cms:read` | `cms:page:write`、`cms:publish` |
| 产品管理 | `GET/POST /products`、`GET/PATCH/DELETE /products/{uid}`、`…/publish|rollback|versions`（**能力状态必选**） | `cms:read` | `cms:content:write`、`cms:publish` |
| 解决方案 | `GET/POST /solutions`、`…/{uid}`、发布/版本同上 | `cms:read` | `cms:content:write`、`cms:publish` |
| 套餐展示 | `GET /plan-display`（只读 `plans` + 展示选项）、`PUT /plan-display`（顺序/推荐/显隐，**无价格字段**） | `cms:read` | `cms:plan-display:write` |
| 新闻公告 | `GET/POST /articles?category=news|announcement`、`…/{uid}`（置顶、有效期） | `cms:read` | `cms:content:write`、`cms:publish` |
| 博客文章 | `GET/POST /articles?category=blog`、`…/{uid}` | `cms:read` | `cms:content:write`、`cms:publish` |
| FAQ 管理 | `GET/POST /faqs`、`PATCH/DELETE /faqs/{uid}`、`PUT /faqs/order`、`GET/POST /categories` | `cms:read` | `cms:content:write`、`cms:publish` |
| 媒体资源 | `GET /media`、`POST /media`（multipart，§12.3）、`GET /media/{uid}/usages`、`PATCH /media/{uid}`（alt）、`DELETE /media/{uid}`（被引用拒绝） | `cms:read` | `cms:media:write` |
| SEO 管理 | `GET/PUT /seo/{entity_type}/{entity_uid}?locale=`、`GET/PUT /seo/site`（sitemap、robots） | `cms:read` | `cms:seo:write` |
| 顶部导航 | `GET/PUT /navigation/header`（树；链接目标校验：不得指向未发布页/未交付功能，否则阻止发布） | `cms:read` | `cms:nav:write`、`cms:publish` |
| Footer 管理 | `GET/PUT /navigation/footer` | `cms:read` | `cms:nav:write`、`cms:publish` |
| 多语言内容 | `GET /localizations/matrix`（内容单元×语言完整度）、各内容实体接口带 `locale` 参数编辑 | `cms:read` | 同对应内容权限 |
| 官网设置 | `GET/PUT /settings`（站点名、默认语言、默认主题、预览时效、统计代码） | `cms:read` | `cms:settings:write` |

公开读取（官网用，无认证，仅已发布，可缓存）：`GET /api/v1/public/cms/{pages|articles|faqs|navigation|settings|banners|products|solutions}`、`GET /api/v1/public/plans`、`GET /api/v1/public/stats`、`GET /api/v1/public/site-info`、以预览令牌读取草稿的 `GET /api/v1/public/cms/preview`（令牌经 `X-Preview-Token` 头，仅 Website SSR 服务端内部调用，Nginx 官网域不放行 `/api/` 其余路径）。令牌交换发生在 Website 自己的 `POST /__preview/exchange` 路由内，不是 Server 端点。自用模式下 `public/plans` 返回 `403 feature.disabled`，官网据此隐藏套餐模块。

## 7. 区块 Schema 校验

- 每种区块类型（`hero`、`feature_grid`、`product_cards`、`node_map`、`stat_group`、`plan_table`、`faq_list`、`solutions_grid`、`advantages`、`cta`、`rich_text`、`image`、`security_boundary_notice` 等）有一份 **JSON Schema（2020-12）**，存放于 `pkg/cmsblocks/schemas/*.json`，作为**单一事实来源**（`website_page_blocks.block_type` 的取值即这些文件名，数据库**不对它设 CHECK**，由 Service 按 Schema 注册表校验，M17）：
  - Server 保存草稿与发布时校验（`additionalProperties: false`，字符串长度上限、数组条数上限、`format` 限制）；
  - Admin 编辑器用同一 Schema 生成表单与前端校验（ADR-014 的 `shared` 生成物）；
  - Website 区块组件的 props 类型由 Schema 生成。
  Go 端 JSON Schema 校验库与 Admin 表单生成库的选择：**实现时核对**，需标注版本。
- **数据模块的约束写入 Schema**（落实 UX §3.2）：`stat_group`/`node_map` 区块**没有数值输入字段**，只有 `data_source`（枚举）与 `source_id`（引用公开数据源标识）；选 `illustrative` 时 Schema 禁止出现 `source_id` 与数字字段。`plan_table` 区块 `additionalProperties:false`，含 `price`/`amount` 的任何字段都校验失败（R-12）。
- **能力状态**：产品/解决方案/页面的能力项必须带 `capability_status ∈ {available, planned}`；高级能力（HTTP/3、L2 缓存、DNSSEC、边缘计算）在 P6 验证前**禁止**取 `available`（校验失败，NFR-27）；选 `available` 时必须引用「已交付能力标识」（由 Server 的能力注册表提供，防止文案超前，UX §6）。
- 发布前校验链：Schema → 引用完整性（媒体/页面/套餐）→ 能力声明 → 禁用词表（UX §9，警告或阻断由配置）→ 链接目标合法性 → 必含块检查（见 §13）。

## 8. XSS 防护（NFR-9）

1. **区块优先于富文本**：绝大多数内容是结构化字段（纯文本），渲染时默认转义（Vue 模板插值）；仅 `rich_text` 与文章正文允许富文本。
2. **富文本存储为已净化的 HTML 子集**：Server 在**保存时**用白名单净化器处理（Go 端候选库 `bluemonday` 一类，版本实现时核对）：
   - 允许标签：`p br strong em u s code pre blockquote ul ol li h2 h3 h4 a img table thead tbody tr th td hr` 等有限集合；
   - 属性：`a[href|title|rel|target]`、`img[src|alt|width|height]`，其余全部剥离，**无 `style`、无事件处理器、无 `class`/`id`（避免样式劫持）**；
   - 链接协议仅 `http/https/mailto` 与站内相对路径，`target=_blank` 强制 `rel="noopener noreferrer"`；
   - `img[src]` 仅允许媒体库路径（`/media/...`）或经显式白名单的 https 域；
   - 丢弃 `script/style/iframe/object/embed/form/svg/math`。
   净化器配置有**回归语料测试**（OWASP XSS 向量、mXSS 变体、协议混淆）。
3. **渲染侧**：只有标记为 `sanitized` 的字段经 `v-html`；ESLint 规则禁止其他位置使用 `v-html`。**SSR 渲染前再做一次轻量断言**（检测出 `<script`/`on*=` 则丢弃该字段并告警，不信任单一防线）。
4. **统计代码**（官网设置）属于可注入脚本，**仅 `cms:settings:write` 可改**、变更强审计，且受 CSP 约束（需显式把脚本来源加入 CSP 配置，默认空，UX §7 隐私）。
5. **CSP**（§11.4）禁止内联脚本/事件处理器，使其成为纵深防线。

## 9. 套餐展示读取正式产品（FR-45、R-12）

- 官网实际价格只来自 `GET /api/v1/public/plans`（读 `products/plans`，P5 的 billing 模块提供；P5 前无数据 → 「即将提供」）。
- 金额以 `{ "amount_minor": 12345, "currency": "CNY" }` 传输；前端格式化用**整数运算**拆分整数/小数部分（按币种指数，如 `padStart`），不得用浮点除法产生价格字符串（保持「金额不得浮点」的精神，FR-109）。
- CMS 只存展示选项（顺序、推荐、显隐、文案摘要），**数据库无价格列**（可验证项：FR-45）。
- 自用模式：`public/plans` 返回 `feature.disabled`，官网 `/pricing` 与首页价格区块隐藏（§3.5）。

## 10. 区块类型与首页（M1 复用）

首页 13 个区块（基线 §5.3）在 M1 以代码内静态内容渲染，其区块组件与 P2 CMS 的区块渲染组件是**同一批组件**（`components/blocks/*`），M1 的静态内容文件结构 = CMS 区块 JSON 的子集；这样 P2 把「内容来源」从仓库文件切换为 API 时无需重写组件，也保证 M1 的 Schema 校验脚本可复用。

## 11. 安全与缓存策略

### 11.1 缓存策略总表

| 资源 | 策略 |
|---|---|
| `/_nuxt/*`（带哈希） | `public, max-age=31536000, immutable` |
| HTML（SSG） | `no-cache`（带 ETag/Last-Modified 校验） |
| HTML（SSR 公开页） | Nitro SWR（TTL 配置）+ 发布 revalidate；对外 `Cache-Control: public, max-age=0, must-revalidate` + ETag |
| 预览/带 token | `private, no-store` + `X-Robots-Tag: noindex` |
| `/runtime-config.js` | `no-store` |
| `/media/*`（内容哈希文件名） | `public, max-age=31536000, immutable` |
| `/api/v1/public/*` | 短 TTL（服务端设置 `Cache-Control: public, max-age=<秒>`；统计类以秒级为单位，值待基线测试后确定） |

### 11.2 性能原则

SSG 首屏无需 JS 才可读；图片带尺寸与懒加载、现代格式（P2 媒体管线先产出重编码的 PNG/JPEG/GIF；WebP/AVIF 待 cgo 决策，见 §12.1）；字体自托管子集；**无第三方脚本/字体/CDN**（离线与私有化友好）；Hero 动画尊重 `prefers-reduced-motion`。性能数值（LCP 等）**待基线测试后确定**，不在官网文案出现。

### 11.3 其他安全

- 官网无需登录态；不读取/存储用户凭据；跨站请求仅限 GET 公共接口；`/_lingedge/revalidate` 仅 HMAC 有效，Nginx 对公网 404（§4.3）。
- Header 伪造：Nginx 反代到 Server/SSR 时覆盖 `X-Forwarded-For`/`X-Real-IP`（不追加访客值）。

### 11.4 CSP 与安全头

```
Content-Security-Policy:
  default-src 'self';
  script-src 'self' 'sha256-<主题初始化脚本哈希>' 'sha256-<runtime-config 引导哈希(若内联)>';
  style-src 'self' 'unsafe-inline';      # 如实现时 Tailwind/Nuxt 可去除 unsafe-inline 则去除（核对）
  img-src 'self' data:;
  font-src 'self';
  connect-src 'self';
  frame-ancestors 'none';
  base-uri 'self';
  form-action 'self'
X-Content-Type-Options: nosniff
Referrer-Policy: strict-origin-when-cross-origin
Permissions-Policy: camera=(), microphone=(), geolocation=()
Strict-Transport-Security: max-age=<秒>（启用 HTTPS 后）
```

- SSG 使用**哈希**白名单内联脚本（构建时计算并写入片段 `website-security-headers.conf`）；SSR 若 Nuxt 的内联 payload 需要，则采用 nonce（`nuxt-security` 一类模块是否适配 Nuxt 3.x：实现时核对）。目标是**无 `script-src 'unsafe-inline'`**。
- 配置了统计代码的来源时才扩展 `script-src/connect-src`。

## 12. 媒体上传安全（`media` 模块，P2）

### 12.1 约束

| 项 | 规则 |
|---|---|
| 允许类型 | `image/png`、`image/jpeg`、`image/gif`（P2；`image/webp`/`avif` 待 cgo 决策后追加，MIN-09）；**不允许 SVG**（可含脚本）、HTML、PDF、视频（P2 范围外）。需要 SVG 图标时作为仓库内静态资源随构建发布，不经上传 |
| 校验 | 以**魔数**（文件头，`http.DetectContentType` + 解码器确认）判定类型，**忽略**客户端 `Content-Type` 与扩展名；声明类型与实际不符 → 拒绝 |
| 尺寸 | 单文件上限（默认 10 MiB，配置项）；像素上限（默认每边 ≤ 8192、总像素有上限以防解压炸弹，配置项）；解码前先读头部尺寸 |
| 重新编码 | 通过校验的图片**解码后重新编码**（去除 EXIF/ICC 之外的元数据、嵌入载荷与多语种文件 polyglot 内容）；解码失败 → 拒绝。**P2 仅做 PNG/JPEG/GIF 重编码**（Go 标准库 + `x/image`，无 cgo）；**WebP/AVIF 编码需 cgo/libwebp，与“单一静态 Go 二进制”倾向冲突，待用户决策后再开放**（MIN-09）；在该决策前不接受 WebP/AVIF 上传 |
| 存储路径 | 服务端生成**内容寻址文件名** `<yyyy>/<mm>/<sha256>.<ext>`（= `website_media.storage_key`），根目录为公开媒体目录 `/var/lib/lingedge/media/`；**永不使用用户提供的文件名构造路径**；原始文件名仅作展示字段，经净化（去路径、控制字符、长度限制）；落盘前 `realpath` 必须在媒体根目录内；上传临时文件在私有目录 `/var/lib/lingedge/server/tmp/`（0700） |
| 重复 | 相同 sha256 复用同一文件与同一 `website_media` 行（活动行 `sha256` 唯一，database/cms.md §5.1），引用计数由实体引用关系体现 |
| alt 文本 | 必填提示（UX §6）；被引用的媒体删除需确认并拒绝（返回引用列表） |
| 权限 | 上传/删除 `cms:media:write`；审计 |
| 服务 | Nginx 直接托管只读媒体目录；响应头 `X-Content-Type-Options: nosniff`、`Content-Security-Policy: default-src 'none'; sandbox`、不返回 `Content-Disposition` 内联脚本类型；上传目录**不执行**任何动态内容 |
| 速率 | 上传接口按用户限速（Redis，降级为本地） |

### 12.2 流程

`POST /media`（multipart，流式读取，超限即中断）→ 限长读入临时文件（非公开目录）→ 魔数/解码/尺寸校验 → 重新编码 → 计算 sha256 → 原子 `rename` 到最终路径 → 写 `website_media` → 审计 → 返回媒体 `uid` 与 `/media/...` URL。

### 12.3 访问

静态由 Nginx `location /media/` 提供（§4.2），`alias /var/lib/lingedge/media/`；不通过 Server 动态读文件，避免路径遍历面。**目录权限**（MIN-08）：公开媒体本就是公开内容，故目录独立于 `/var/lib/lingedge/server/`（后者属 `lingedge` 0750，Nginx 运行用户无法穿越）：`/var/lib/lingedge/` 为 root 0755，`/var/lib/lingedge/media/` 属 `lingedge:lingedge`，目录 0755、文件 0644，由 Tools 在安装 server（P2 起）时创建；上传临时文件与未发布内容不放入该目录。

## 13. 内容与合规约束的技术落点

- **DDoS 边界声明（FR-101，P4）**：固定文案（`docs/ux/website.md` §9）存放于 i18n 资源 `legal.ddosBoundary`（zh/en）。`security_boundary_notice` 区块**不可被编辑者改写，只能引用**；WAF 产品页与首页 WAF 区块的页面模板**必含**该区块——发布前校验链的「必含块检查」对 `/products/waf` 与首页强制，缺失则阻止发布；构建脚本 `check-capability-claims.ts` 对 `dist` 检查这两处出现该文案，缺失构建失败（可验证项：三处文案之官网两处）。
- **能力声明**：见 §7；`check-capability-claims.ts` 同时扫描「已支持 HTTP/3」「支持 DNSSEC」等未验证能力措辞与 UX §9 禁用词。
- **法律页/关于我们**：内容由用户提供，不在仓库中起草。
- **示意数据**：数据模块「示意」状态在官网概览中列出（UX §6），便于运营者清理。

## 14. Server 侧模块边界

- `cms`（内容与版本、发布状态机、定时任务、导航）、`seo`（SEO 字段与 sitemap/robots 生成）、`media`（上传管线）为 Server L2 模块（architecture §5.3），互不依赖业务模块；`public` 读取接口由其只读 Service 提供，缓存头由 handler 设置。
- **套餐只读依赖**：`cms` 通过 `billing` 的 `api.go`（只读接口 `PlanCatalog`）获取套餐，经 `platform/ports` 注入，避免模块间直接依赖；P5 之前该实现返回空集合。
- 与 Worker 协作的任务类型（白名单，**事实源为 OpenAPI `TaskType` 枚举**，此处不另列权威清单）：涉及 `cms.publish_due`、`cms.revalidate`、`cms.prerender`（降级模式）、`media.cleanup_orphans`；均幂等。
- 官网读取 Server 全部经 `public` 接口，**不直连数据库**。

## 15. 测试策略

| 层 | 内容 | 阶段 |
|---|---|---|
| 单元（Vitest） | 区块组件渲染、`IllustrativeBadge` 不可移除、数据源声明缺失即失败、i18n 键完整性、runtime-config 驱动的入口显隐、价格格式化（整数运算） | M1 |
| 静态检查 | `check-hardcoded-metrics`、`check-nav-reachable`、`check-capability-claims`（禁用词、DDoS 声明）、ESLint 禁用 `v-html` 规则；CI 门禁 | M1 |
| 构建/产物 | SSG 产物含正文（无 JS 抓取）、`hreflang`/sitemap 正确、`/en/` 与默认语言 meta、无第三方外链资源 | M1 |
| E2E（Playwright） | 导航全部入口可达（AC-B-2）、中英/深浅主题切换（AC-B-1）、移动端 375px 无横向滚动、自用模式不出现「注册」与价格入口（AC-B-5）、Server 停止时数据模块显示「暂无数据」（自建快照模式为快照时间戳）而非旧值（AC-B-4） | M1，需浏览器环境 |
| Schema/净化 | 区块 Schema 正反例；XSS 回归语料（OWASP、mXSS、协议混淆）对净化器；`plan_table` 含价格字段被拒 | P2 |
| 媒体 | 恶意样本：伪装扩展名、polyglot、SVG、超大像素炸弹、`../` 文件名、重复上传；断言无越界写、重新编码后元数据已剥离 | P2 |
| CMS 流程 E2E（真实 Server+DB+SSR，闭环 I，AC-I-1..6） | 草稿前台不可见 → 预览链接 → 发布前台更新 → 定时发布到点生效 → 回滚恢复 → 中英文与 SEO 字段生效；套餐展示来自 `plans`（需 P5 数据前以真实 `plans` 夹具之外的**真实表数据**验证，禁止 Mock） | P2 |
| 安全 | 预览令牌过期/篡改/重放；`revalidate` 无签名/过期时间戳被拒；统计代码仅授权角色可改；CSP 违规零上报 | P2 |
| 环境 | 本机（macOS）可跑单元/静态检查/SSG 构建；SSR + systemd + Nginx 联调与 E2E 在验收机；缺环境者标「**等待环境验证**」（NFR-3） | — |

## 16. 待决与风险

- **PoC**：静态降级模式的爬取式预渲染（§4.6）；CSP 无 `unsafe-inline` 在 Nuxt 3.x 下的可行性（§11.4）；`runtime-config.js` 阻塞脚本对首屏的影响（待基线测试后确定）。
- **依赖版本**：Nuxt 3.x 线与 i18n/sitemap/Tailwind/TS 模块的兼容矩阵（实现时核对并固定）。
- **OQ（新增）**：官网默认语言与 URL 策略是否认可（沿用 UX 的 `[ASSUMPTION]`）；法律页与关于我们的内容提供方；统计代码是否需要内置支持。

## 17. 对 PRD 的反馈（官网相关）

- **W-1（承接 F-5）**：FR-41 的「读取自 Server 公共接口」在 SSG 下改为「**发行产物不含构建时快照（占位「暂无数据」）+ 浏览器端实时覆盖**；运营者自建可显式启用构建时快照」，并由 CI 脚本强制数据源声明；PRD 可将验收口径写为「产物中无无来源数字」。（2026-11-16 修订：此前“构建时快照”会让同一份签名产物在所有部署显示 CI 所连 Server 的数字，违反 FR-41。）
- **W-2 运行模式的运行时配置**：FR-46「入口与运行模式一致」要求官网在**同一份静态产物**上适配模式，因此引入 `runtime-config.js` 与 Tools 生成的 Nginx 片段；PRD 无需改动但应知晓该安装期产物。
- **W-3 `/pricing` 自用模式**：D-3 说「价格类入口可隐藏」，本设计更严格：自用模式对 `/pricing` 返回 404（而不仅隐藏导航）。

## 修订记录 2026-11-16

依据 `docs/bmad/review-architecture.md`（IMP-17、MIN-08、MIN-09）与 `review-architecture-internal.md`（H9、M17），按统一决策表修订：

- **IMP-17 / H9**：§3.4、§3.6、W-1——发行产物禁用构建时快照（快照会让同一份签名产物在所有部署显示 CI 所连 Server 的数字，违反 FR-41），占位「暂无数据」+ 浏览器端覆盖；Nginx 片段重写（安全头 include 进每个含 `add_header` 的 location 并带 `always`；`/api/` 只放行 `/api/v1/public/`，其余 404；Admin/Console/官网主机名互不相同由 Tools 强制）。
- **MIN-08 / M17**：§4.3 revalidate 仅靠 HMAC（经 Nginx 后来源恒为回环，检查无意义；Nginx 对公网 404，Server 直连 127.0.0.1:3000），密钥 `website.revalidate_secret` 由 Tools 生成并分发；§4.4 预览令牌改不透明随机令牌+库存哈希（`website_preview_links`）+ URL fragment 交换 HttpOnly 短期 cookie；§4.5 定时发布对齐数据库 `scheduled_version_id/schedule_error`；§12 `/media/` 目录改 `/var/lib/lingedge/media/`（0755，Nginx 可读）；内容寻址存储键与 sha256 唯一对齐数据库；区块类型名以 `pkg/cmsblocks/schemas` 为准。
- **MIN-09**：P2 媒体管线仅重编码 PNG/JPEG/GIF；WebP/AVIF 需 cgo，待用户决策。
- **M14（引用部分）**：§14 任务类型清单改为“见 OpenAPI `TaskType`”。
