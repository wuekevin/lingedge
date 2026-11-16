# ADR-015 官网渲染形态：M1 SSG 静态、P2 起 Nuxt SSR

- 状态：已接受（架构代理非交互决策）｜日期：2026-11-16｜关联：基线 §2.3、§5、§21.4；PRD D-8、FR-14、FR-41、FR-42、FR-44、OQ-9；architecture AD-15｜阶段：M1（SSG）/ P2（SSR + CMS）

## 背景

基线 §21.4：官网支持 Nuxt SSR 或预渲染静态站点；SSR 须明确 Node.js 运行依赖，**不得把 Nuxt SSR 当成可由纯 Go 二进制执行**。PRD D-8 的默认：M1 静态（无 CMS），P2 起默认 SSR 并保留「发布触发重新生成静态」降级。FR-41 要求首页数值真实或标注示意。详细设计见 [website-cms-design.md](../website-cms-design.md)，本 ADR 只固定决策与约束。

## 决策

1. **M1：Nuxt 3 SSG**（`nuxt generate`，固定 Nuxt 3.x 线，npm tag `3x` 查询为 3.21.11，lockfile 固定精确版本；`latest` 为 4.x，**不采用**，升级须新 ADR）。产物为纯静态文件，随发行包交付，由 Nginx 托管，**生产机无需 Node.js**；Tools 在静态模式下不检测 Node（FR-14）。
2. **P2：Nuxt SSR**（Nitro `node-server` 预设），作为独立 systemd 服务 `lingedge-website.service`（`User=lingedge-web`，监听 `127.0.0.1:3000`，Nginx 反代），发行包标注 `requires.runtime.node` 版本范围；`lingedge install website --mode ssr` 在预检中探测 `node` 绝对路径与版本，缺失/不满足则退出码 3，**不静默安装 Node.js**。
3. **真实数据原则（FR-41，反馈 F-5）**：静态 HTML 含「示意/暂无数据」占位；**发行产物构建一律禁用构建时统计快照**（快照来自 CI 所连 Server，随同一份签名产物分发到所有部署会显示别人的数字；仅运营者自建时可显式 `--stats-snapshot`）；浏览器端从 `GET /api/v1/public/stats` 读取真实值并覆盖；SSR 模式服务端读取同一接口，失败渲染「暂无数据」，不使用进程内旧值。CI 检查源码无硬编码性能/可用率数字。
4. **内容来源**：P2 起内容经内部地址 `GET /api/v1/public/cms/*`（只读，仅已发布内容）读取；发布/回滚/定时翻转后由 Worker 任务 `cms.revalidate` **直连 `127.0.0.1:3000`** 以 HMAC 签名 webhook 通知 Website 失效 Nitro SWR 缓存（HMAC 是该路由唯一有效防线，Nginx 对公网 `/_lingedge/` 返回 404；密钥 `website.revalidate_secret` 由 Tools 生成并分发）；SWR TTL 保证最终一致。
5. **预览**：**不透明随机预览令牌**（库存 SHA-256，`website_preview_links`；≤30 分钟，可单独撤销），令牌置 URL fragment 并由页面脚本 POST 交换为 HttpOnly 短期 cookie（避免进入 access log/Referer）；响应 `Cache-Control: private, no-store` + `noindex`；预览不进入任何缓存与 sitemap。
6. **静态降级模式**（SSR 之外的可选形态）：发布触发 Worker 任务 `cms.prerender` 对本机 SSR 实例做爬取式预渲染，原子换目录；**属自定义能力，上线前须 PoC**，默认仍为 SSR。
7. **入口随运行模式**：登录/注册/控制台入口由安装时生成的 `runtime-config.js`（含 `run_mode`）决定，自用模式隐藏注册与价格类入口（D-3、FR-46）；官网与 Server 通过 `public` 接口耦合，**不共享代码**。
8. i18n：中文为默认无前缀、英文 `/en/`（website-cms-design §3.2 的决策）；SEO（sitemap、hreflang、OG）在 SSG/SSR 下行为一致。

## Binds / Prevents / Rule

- 约束：官网前端、Tools 的 website 安装器、CMS/Worker 的发布流程。
- 防止：官网被当成 Go 二进制部署；生产展示虚假指标（含“发行产物内嵌 CI 快照”）；SSR 默认缓存把草稿或个人化内容泄漏给访客。
- 规则（AD-15）：**官网 M1 为 SSG 静态；P2 起 Nuxt SSR（Node.js 由 systemd 托管，安装时显式声明并检测依赖）。**

## 备选方案与权衡

- **M1 即上 SSR**：否决——M1 无 CMS，SSR 只增加 Node 运行依赖与进程，违背「MVP 最简」；且会让 MVP 安装在无 Node 的宝塔机上失败。
- **全程 SSG + 发布时重建**：否决——重建需要服务器具备 Node 与构建工具链，且预览/定时发布/回滚的实现更绕；仅保留为降级模式。
- **用 Go 模板自写官网**：否决——基线 §2.3 固定 Nuxt 3，且 SEO/i18n/CMS 区块渲染复用 Vue 组件更自然。
- **Nuxt 4**：否决——基线写明 Nuxt 3；4.x 迁移影响面需单独评估。

## 后果

- P2 起官网增加一个需运维的 Node 进程与安全更新面；Tools 的 `doctor` 须检查 Node 版本与服务健康。
- SSG 的数值覆盖依赖浏览器 JS：禁用 JS 的爬虫只看到占位（F-5，已知限制）。
- 从 SSG 到 SSR 的切换需要升级路径（Tools `update` 变体 `website:ssr`，切换前后目录互不覆盖）。

## 可被推翻处

用户（OQ-9）若要求 P2 起仍保持纯静态：CMS 预览改为管理员专用的独立 SSR 实例，发布走 `cms.prerender`；影响 website-cms-design §4、tools-design 的 website 变体与 Node 依赖声明。若要求 M1 即 SSR：M1 安装向导增加 Node 检测，tools-design 退出码 3 路径提前。

## 验证

- 静态模式在无 Node.js 的机器上安装并由 Nginx 托管；SSR 模式在无 Node 时中止（FR-14 可验证项）。
- 路由可达性、`check-hardcoded-metrics`、`check-capability-claims` 脚本进 CI；SEO/hreflang 校验。
- P2：草稿不可见、预览 no-store、定时发布到点生效、回滚恢复（闭环 I）；`cms.prerender` PoC 报告。

## 修订记录 2026-11-16

- **IMP-17 / H9**：§3 发行产物禁用构建时快照（占位 +浏览器覆盖），快照仅限运营者自建；与 website-cms-design §3.4/§3.6、architecture F-5 一致。Nginx 片段与独立主机名要求见 website-cms-design §3.6、ADR-014。
- **MIN-08 / M17**：§4 revalidate 的真实防线是 HMAC（不依赖“仅内网来源”）；§5 预览令牌由“签名令牌 + `jti` 黑名单”改为“不透明随机令牌 + 库存哈希 + fragment 交换 cookie”，与 database/cms.md §6.2 对齐。
