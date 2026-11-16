# LingEdge（凌界云）— 需求基线 V2.0

> 本文件是 LingEdge 唯一的产品与开发需求基线（用户原始总指令 V2.0 的归档）。所有 BMAD 产物（Brief / PRD / 架构 / Epics）必须以本文为准；若产物与本文冲突，以本文为准并记录 ADR。

**项目名称：** LingEdge　**中文品牌：** 凌界云　**定位：** Intelligent Edge Cloud Platform
**模式：** BMAD + Claude Code　**性质：** 独立研发、可自主部署、可商业化运营的 CDN 边缘云平台

---

## 第一章：项目总目标

### 1.1 定位
从零开发完整商业级 CDN 平台。英文 LingEdge，中文 凌界云。参考 CDN1、Cloudflare、腾讯云 EdgeOne、阿里云 ESA 的公开功能与设计理念；重点参考：CDN1 组件化部署与节点集中管理、Cloudflare 站点规则系统、EdgeOne 网站配置体验、阿里云 ESA 缓存与安全功能、Certimate 证书 Provider 扩展设计。
**只参考公开功能和工作流程，不复制闭源代码、商业资源或专有协议。**
必须独立拥有：品牌、架构、数据库、API、节点通信协议、管理后台、用户控制台、安装工具、商业运营系统。

### 1.2 最终交付（12 个模块）
1. Website 官方网站　2. Admin 管理员后台　3. Console 用户控制台　4. Server 中央主控 API　5. Node CDN 边缘节点　6. DNS 智能 DNS 服务　7. Scheduler 调度服务　8. Reporter 区域监控探针　9. Worker 后台任务服务　10. Tools 统一安装/升级/运维 CLI　11. Docs 帮助与开发者文档　12. Status 服务状态页
统一品牌、权限规范、配置协议、版本管理；可独立安装并组合部署。

### 1.3 核心交付原则（禁止项）
- 只生成前端页面不实现业务
- 用 Mock API 冒充真实接口；用模拟节点代替真实 CDN 节点；用虚假流量/安全数据冒充生产指标
- 只建目录和空接口就宣称完成；未测试就标记 Story 完成；一次性堆砌不可运行代码
- 绕过权限、认证、租户隔离
- 直接拼接不可信用户输入生成 Nginx 指令
- 生产环境使用固定默认密码
- 将 Docker 作为必选组件
允许测试 Mock/Fixture/模拟服务器，但必须与生产实现严格区分。每个阶段必须产出可运行、可验证的交付物。

---

## 第二章：技术框架

### 2.1 Go 后端
Go、Gin、sqlx、MySQL 8.0、Redis 8、Zap、gRPC、Protobuf、mTLS、ACME、OpenAPI 3.1。
架构：**MVC + Service + Repository + Domain Modules**。控制平面第一阶段为**模块化单体**；Node/DNS/Reporter 等独立部署服务用独立二进制；Scheduler/Worker 小规模时作为 Server 内置模块，也须支持后续独立部署。第一阶段不得无必要拆分微服务。

### 2.2 管理后台
Vue 3、TypeScript、Vite、Arco Design Pro、Pinia、Vue Router、Vue I18n、ECharts。响应式、暗/亮主题、中英文、动态权限菜单、多标签页、全局搜索、操作审计、完整表单校验、加载/失败/空数据状态。Admin 与 Console 是不同前端入口，可复用组件但权限必须隔离。

### 2.3 官方网站
Nuxt 3、Vue 3、TypeScript、Tailwind CSS、SSR/SSG、SEO、国际化。完整官网 + 后台 CMS、响应式、多语言、文章与公告、产品介绍、套餐展示、注册登录入口。

### 2.4 CDN 边缘
OpenResty、Nginx、Nginx Stream、Lua、Go Agent、Nginx Proxy Cache。Go Agent 负责节点控制、配置生成、部署、状态采集、升级；OpenResty 负责 HTTP 处理、反代、缓存及可支持的 WAF/规则执行。**HTTP/3 必须使用经验证、与所选代理及安全模块兼容的实现；不得仅以 HTTP/3 握手成功作为完整功能验收。**

### 2.5 DNS
Go、miekg/dns、MySQL、Redis、DNS Provider 接口。自建权威 DNS、第三方 DNS API、地理位置调度、运营商线路、权重、健康检查、故障切换。

### 2.6 CLI
Go、Cobra、Bubble Tea（或等效 TUI）、YAML、systemd、版本清单、数字签名、SHA256、HTTPS 下载。主程序 `lingedge`；**不再开发独立的 lingedge-updater**。

### 2.7 部署
第一部署方式：**宝塔面板 + Linux + Go + systemd**；第二：原生 Linux + Nginx + systemd。
不使用 Docker / Docker Compose，不依赖 Kubernetes。支持 Linux AMD64、ARM64；Reporter 支持 Windows AMD64（Windows x86 视依赖兼容性）。

---

## 第三章：整体系统架构

### 3.1 三层分离
- **Control Plane**：用户与租户、网站配置、节点管理、规则管理、证书管理、DNS 控制、配置发布、套餐计费、CMS、审计日志
- **Data Plane**：CDN 反代、静态缓存、回源、WAF、CC、四层代理、请求处理、实时安全控制
- **Observability Plane**：节点监控、访问日志、流量统计、缓存统计、DNS 查询统计、区域探测、安全事件、告警、计费计量
三层间使用明确接口协议。**主控暂时离线时，已发布配置的节点必须继续处理用户访问。**

### 3.2 核心业务模型
```text
Platform
  └── Tenant
       ├── Users
       ├── Sites
       │    ├── Domains / Origins / Rules / Cache Settings / TLS Settings / WAF Settings / Cluster Bindings
       ├── Subscriptions / Usage / Billing
Cluster
  ├── Nodes / Regions / Network Lines / Scheduling Policies
```
一站点多域名；一站点多源站及源站组；一集群服务多站点；一节点可按资源与策略加入一个或多个集群；普通租户只能操作已授权资源。

---

## 第四章：LingEdge Tools 统一 CLI（第一阶段核心）

### 4.1 定义
正式名称 LingEdge Tools；程序 `lingedge`；入口 `cmd/lingedge/`；负责整个平台的安装、管理、升级、维护；完全取代 Updater。

### 4.2 可安装组件
server（中央主控）、admin-web、console、website、node、dns、scheduler、worker、reporter、docs、status。
允许单机多组件，也允许分布式部署；必须识别版本依赖、资源要求、端口冲突。

### 4.3 交互菜单
运行 `lingedge` 进入：1 安装组件　2 查看已安装组件　3 升级组件　4 服务管理　5 节点注册　6 系统诊断　7 配置管理　8 备份恢复　9 查看运行日志　10 卸载组件　11 查看版本　0 退出。
安装界面为多选复选框列表（Server / Admin Web / Console / Website / Node / DNS / Reporter / Docs / Status）。

### 4.4 CLI 命令（必须提供）
```bash
lingedge --help | version | list | status
lingedge install server|admin-web|console|website|node|dns|reporter|docs|status
lingedge update check | update node | update --all
lingedge service status --all | start node | stop node | restart node
lingedge doctor --all
lingedge config validate
lingedge node enroll
lingedge backup create | backup restore
lingedge rollback node
lingedge uninstall node
```
规范：交互与无交互、明确退出码、JSON 输出、详细日志、参数验证、安装任务锁、执行进度、安装失败恢复。

### 4.5 安装过程（必须执行）
1 OS 检测 2 CPU 架构 3 内存磁盘 4 端口冲突 5 宝塔环境 6 Nginx/OpenResty 7 MySQL/Redis 8 组件依赖分析 9 版本选择 10 下载签名版本清单 11 验证清单签名 12 下载组件包 13 校验 SHA256 与签名 14 安全解压 15 创建运行用户 16 创建配置目录 17 安装 systemd Unit 18 初始化配置 19 启动服务 20 健康检查 21 保存安装状态 22 输出访问/管理信息。
**不得静默覆盖已有网站、数据库、证书或 Nginx 配置。**

### 4.6 宝塔兼容
检测宝塔已装的 Nginx/MySQL/Redis/PHP（仅识别）/站点配置/WAF/防火墙/系统服务；不得要求用户卸载宝塔。Node 默认使用**独立 OpenResty 运行环境**避免冲突；明确端口、配置目录、进程归属。

### 4.7 升级
所有组件通过 Tools 升级。流程：检测版本 → 获取签名清单 → 检查兼容性 → 下载 → 校验 → 备份当前版本 → 检查数据库迁移 → 安装新版本 → 启动及健康检查 → 成功确认或回滚。**存在不可逆 Migration 时，禁止承诺简单回退二进制即可恢复。**

### 4.8 Tools 安全
数字签名验证；禁止未验证发行包；防压缩包路径穿越；防命令注入；Token/密码不进普通日志；不默认删除用户数据；最小权限运行；配置与数据库备份；安全卸载；支持离线安装可信发行包。

---

## 第五章：官方网站 Website

### 5.1 定位
完整商业级品牌官网，非简单着陆页。深蓝科技风、蓝紫渐变、企业级布局、响应式、深/浅主题、中英双语。

### 5.2 主导航
首页 / 产品（CDN 内容分发、WAF 网站防护、智能 DNS、TCP/UDP 四层代理、SSL 证书管理、数据分析）/ 解决方案（网站加速、静态资源加速、API 加速、视频资源加速、跨境访问、企业安全）/ 价格（套餐价格、流量计费、带宽计费）/ 开发者（API 文档、接入指南、更新日志）/ 帮助中心（产品文档、常见问题、工单支持）/ 关于我们 / 登录 / 注册 / 控制台。

### 5.3 首页
顶部导航、Hero Banner、产品能力、全球节点地图、CDN 网络介绍、WAF 安全介绍、智能 DNS 介绍、解决方案、产品优势、套餐价格、FAQ、注册引导、Footer。
**生产展示的节点数、可用率、性能指标必须来自真实数据，或明确标注为示意信息。**

### 5.4 官网 CMS（Admin「官网运营」）
菜单：官网概览、首页装修、页面管理、产品管理、解决方案、套餐展示、新闻公告、博客文章、FAQ 管理、媒体资源、SEO 管理、顶部导航、Footer 管理、多语言内容、官网设置。
支持：草稿、预览、发布、定时发布、内容版本、回滚、多语言、SEO、图片资源、模块排序。
**套餐展示必须读取正式产品套餐信息，不允许独立维护第二套实际结算价格。**

---

## 第六章：管理员后台 Admin

### 6.1 一级菜单
工作台 / 网站管理 / 节点管理 / 智能调度 / DNS 管理 / 缓存管理 / 安全中心 / 四层代理 / SSL 证书 / 监控统计 / 用户管理 / 产品计费 / 工单通知 / 插件中心 / 自动化运维 / 官网运营 / 系统设置

### 6.2 工作台
平台运行状态、在线节点数、节点区域分布、当前带宽、今日流量、今日请求数、缓存命中率、站点数量、用户数量、安全事件、告警中心、运行状态大屏。

### 6.3 网站管理
网站列表、添加网站、网站分组、配置模板、批量操作。进入网站后：站点概览、基本设置、域名管理、源站管理、缓存管理、规则管理、**网络设置**、SSL/TLS、安全防护、DNS 与调度、数据统计、访问日志、部署记录。

### 6.4 节点管理
节点总览、节点列表、节点分组、节点集群、地域管理、运营商线路、节点 IP 池、节点注册、节点安装、节点配置、配置下发、节点维护、节点升级、节点日志、节点故障。

### 6.5 智能调度
调度总览、调度策略、节点权重、地域调度、运营商调度、健康检查、故障切换、备用节点池、流量迁移、调度日志。

### 6.6 DNS 管理
DNS 概览、域名 Zone、DNS 记录、智能线路、DNS 节点、DNS Provider、DNS 同步任务、DNSSEC、DNS 查询统计、DNS 日志。

### 6.7 缓存管理
全局缓存模板、缓存刷新、缓存预热、L1/L2 缓存、缓存任务、缓存磁盘、缓存统计。

### 6.8 安全中心
WAF 总览、WAF 托管规则、自定义规则、CC 防护、Bot 防护、IP 黑白名单、ASN 限制、国家地区限制、防盗链、安全事件、攻击日志、误拦截分析。

### 6.9 四层代理
TCP 应用、UDP 应用、监听端口、源站组、健康检查、连接控制、流量统计。

### 6.10 SSL 证书
证书列表、证书申请、证书上传、ACME 账户、DNS 验证、自动续期、部署记录、到期告警。

### 6.11 监控统计
实时流量、带宽分析、请求分析、缓存命中率、回源分析、状态码、地域分析、运营商分析、节点性能、区域探测、访问日志、报表导出。

### 6.12 用户管理
用户列表、租户管理、团队成员、用户角色、权限管理、API Token、登录日志、操作审计。

### 6.13 产品计费
产品管理、套餐管理、定价规则、用户订购、流量计费、带宽计费、余额账户、充值订单、消费账单、代理商管理、支付设置。

### 6.14 工单通知
工单管理、站内消息、邮件通知、Telegram、Webhook、通知模板、告警规则、告警历史。

### 6.15 插件中心
插件列表、DNS 插件、云服务商插件、证书插件、存储插件、支付插件、通知插件、插件配置。

### 6.16 自动化运维
任务中心、配置版本、发布记录、节点升级、系统升级、备份恢复、巡检任务、故障诊断、审计日志。
升级与运维由 Tools、Node Agent 或受控任务服务执行。**不得开放任意 Shell 命令执行。**

### 6.17 系统设置
基本设置、品牌设置、登录设置、邮件设置、API 设置、管理员管理、权限角色、系统安全、数据保留、系统日志、版本信息。

---

## 第七章：用户控制台 Console

菜单：工作台 / CDN 加速 / 安全防护 / DNS 解析 / 四层代理 / SSL 证书 / 数据分析 / 财务中心 / 工单中心 / 账户设置。
用户必须能：创建站点、添加域名、配置源站、设置缓存、创建重定向规则、设置请求/响应头、自定义真实 IP 回源 Header、申请 SSL、开启 WAF 和 CC、查看自己的流量与账单、查询访问日志、管理账户与 API Token。
**用户不能查看或修改其他租户数据。** 自用模式可隐藏充值、套餐、工单等商业功能。

---

## 第八章：站点规则引擎

必须是**统一规则引擎**，而非每种规则独立数据结构。

### 8.1 规则类型
重定向、URL Rewrite、回源规则、请求头修改、回源请求头、响应头修改、缓存规则、访问控制、页面规则、自定义错误页面、防盗链、限速。

### 8.2 匹配条件
Host、Path、URI、Query、HTTP Method、Header、Cookie、User-Agent、Referer、客户端 IP、CIDR、ASN、国家、地区。
操作符：eq、ne、contains、starts_with、ends_with、regex、in、not_in、exists、ip_in_cidr。支持 AND / OR / NOT。

### 8.3 执行阶段
真实 IP 识别 → 访问控制 → WAF/CC → 重定向 → URL Rewrite → 缓存规则 → 缓存查询 → 回源规则 → 回源 Header → 源站响应 → 缓存写入 → 响应 Header → 返回客户端。
规则具备：优先级、启用状态、作用范围、配置版本、命中统计、发布记录、回滚能力。
**必须解决缓存规则与动态内容隔离问题，避免跨用户缓存泄漏。**

---

## 第九章：真实客户端 IP 传递（已确认强制需求）

### 9.1 定位
用户在 LingEdge 中自定义**向源站传递真实客户端 IP 的 Header 名称**。方向是 **LingEdge → 用户源站**，不是让用户定义 LingEdge 从上游读取哪个 Header。

### 9.2 后台位置
网站管理 → 选择站点 → 网络设置 → 真实 IP 传递。配置项：X-Forwarded-For 开关、X-Real-IP 开关、自定义 Header 开关、自定义 Header 名称（如 `Ling-Client-IP`）、XFF 传递策略、配置发布状态。

### 9.3 处理逻辑
已验证客户端 IP 为 1.2.3.4 时，向源站发送：
```http
X-Real-IP: 1.2.3.4
X-Forwarded-For: 1.2.3.4
Ling-Client-IP: 1.2.3.4
```
用户源站 Nginx：
```nginx
# 替换为实际可信 LingEdge 边缘出口 IP
set_real_ip_from 203.0.113.10/32;
real_ip_header Ling-Client-IP;
```

### 9.4 安全要求
Header 名称必须合法；禁止敏感保留 Header；自定义 Header 必须覆盖访客伪造值；IP 必须来自可信识别结果；站点独立配置；支持配置下发、版本回滚、调试。
另：LingEdge 自身**入站**真实 IP 识别必须有独立的可信代理 IP/CIDR 机制。

---

## 第十章：CDN Node

### 10.1 功能
HTTP/HTTPS、HTTP/2、WebSocket、SSE、反向代理、多源站、源站健康检查、静态缓存、Range、缓存刷新/预热、WAF、CC、请求/响应/回源 Header 修改、自定义端口、访问日志、性能统计。

### 10.2 Node Agent
注册节点、mTLS 身份认证、心跳、获取配置、配置验证、生成 OpenResty 配置、热重载、上报状态、缓存任务、软件升级、故障恢复。

### 10.3 配置下发
**不可变配置版本。** 流程：主控生成配置 → 校验 → 版本签名 → 节点拉取 → 签名验证 → 生成文件 → `nginx -t` → 灰度部署 → 健康检查 → 确认成功。失败恢复上一有效版本。必须记录每个节点实际生效的配置版本。

---

## 第十一章：缓存系统

默认策略、路径规则、文件类型、TTL、Query 参数策略、Cookie 策略、Cache Key、状态码缓存、缓存锁、Range 缓存、大文件缓存、URL 刷新、目录刷新、预热、磁盘限制、命中率、L2 中间缓存。
初期用成熟 OpenResty/Nginx 缓存。**目录刷新、通配符刷新、集群级失效必须设计可验证实现，不得仅靠不受控扫描/删除缓存目录。**

---

## 第十二章：DNS 与智能调度

### 12.1 模式
A. 第三方 DNS：Cloudflare、阿里云 DNS、DNSPod。B. 自建权威 DNS：LingEdge DNS 独立运行。

### 12.2 功能
Zone、A/AAAA/CNAME/TXT/MX/CAA/SRV/SOA/NS、GeoDNS、运营商线路、权重、健康检查、故障切换、查询统计、DNSSEC（高级阶段）。

### 12.3 调度
综合：节点健康、地域、运营商、带宽余量、延迟、错误率、权重、维护状态、容量。**必须有故障摘除与恢复防抖；不能仅依据单个探针一次失败摘除节点。**

### 12.4 Reporter
HTTP/HTTPS、TCP、TLS、DNS 探测；TTFB、连通性、延迟统计、失败原因、多区域采集。

---

## 第十三章：WAF 与 CC

### 13.1 WAF
SQL 注入、XSS、路径遍历、自定义规则、IP 黑白名单、ASN 封禁、国家地区限制、UA/Referer 限制、Bot 防护、安全事件、日志、误拦截分析。

### 13.2 CC
单 IP 限速、IP+URI 限速、API 路径限速、站点整体限速、临时封禁、JS Challenge、CAPTCHA、观察模式、规则例外。**边缘限流必须避免 Redis 成为每次请求的强制远程依赖。**

### 13.3 安全边界
必须说明：CDN WAF/CC 不等于具备独立大规模 L3/L4 DDoS 清洗能力；若提供高防，必须接入实际具备能力的网络或高防服务。

---

## 第十四章：SSL 证书

ACME、Let's Encrypt、HTTP-01、DNS-01、泛域名、多域名 SAN、手动上传、自动续期、到期告警、证书分发、部署记录、密钥加密存储。**所有证书配置按站点授权；用户不能访问其他租户的证书或私钥。**

---

## 第十五章：四层代理

TCP、UDP、端口监听、后端组、权重、健康检查、连接数限制、源 IP 访问控制、PROXY Protocol、连接日志、带宽流量统计。**TCP 与 UDP 须分别定义配置及健康检查语义；不得套用 HTTP 缓存与规则机制。**

---

## 第十六章：多租户与商业系统

用户注册、登录、TOTP、团队成员、RBAC、API Token、租户资源隔离、套餐、订阅、订单、充值、余额、账单、流量/带宽/请求数计费、代理商、工单。
自用模式关闭不需要的商业功能；商业模式启用完整体系。**计费原始记录必须幂等、可追溯、可对账；金额不得用浮点数。**

---

## 第十七章：监控日志与通知

### 17.1 监控
实时带宽、流量、RPS、请求数、状态码、缓存命中率、回源流量、回源延迟、CPU、内存、磁盘、活跃连接、节点状态、DNS 查询、WAF 事件。

### 17.2 日志
管理操作、登录、节点、访问、WAF、DNS、部署、升级、计费。**高流量访问日志必须采用独立可扩展的采集存储机制，不全部写入 MySQL。**

### 17.3 通知
第一阶段：Telegram Bot、SMTP Email、Webhook。事件：节点离线/恢复、源站不可用、DNS 故障切换、带宽异常、证书到期、WAF 异常、余额不足、部署失败、升级失败。**必须支持告警去重和恢复通知。**

---

## 第十八章：数据库设计

MySQL 8.0，必须使用 Migration。核心表：
```text
system_settings system_installations users roles permissions user_roles sessions api_tokens
tenants tenant_members tenant_quotas
sites site_domains site_groups site_settings
origin_groups origins origin_health
site_rules rule_templates rule_versions
cache_policies cache_tasks cache_task_nodes
nodes clusters cluster_nodes node_heartbeats
config_revisions deployments deployment_targets
dns_zones dns_records dns_providers dns_routes
scheduling_policies node_pools health_checks
certificates acme_accounts certificate_orders certificate_deployments
waf_rulesets waf_rules ip_lists security_events
l4_applications l4_listeners l4_backends
metric_rollups probe_results alert_rules alert_events
products plans subscriptions orders wallets wallet_entries usage_records
tickets ticket_messages
website_pages website_page_versions website_page_blocks website_navigation website_banners
website_products website_solutions website_articles website_categories website_faqs
website_media website_seo website_settings website_localizations
provider_credentials plugins plugin_configs audit_logs
```
必须产出：完整 ERD、表字段、索引、外键、约束、数据保留策略、Migration。**租户隔离必须由服务端强制执行。**

---

## 第十九章：API 规范

统一前缀 `/api/v1/`，模块：auth、admin、tenants、sites、nodes、clusters、dns、cache、waf、certificates、l4、metrics、billing、tasks、public、admin/cms。
每个接口必须：认证、鉴权、租户隔离、参数验证、错误码规范、Request ID、审计、分页、API 文档、测试。耗时任务用异步 Task ID。Node Agent 内部通信使用独立协议与权限体系。

---

## 第二十章：代码目录（必须创建）

```text
lingedge/
├── cmd/{lingedge,lingedge-server,lingedge-node,lingedge-dns,lingedge-scheduler,lingedge-reporter,lingedge-worker}/
├── internal/
│   ├── tools/{cli,tui,installer,manifest,resolver,downloader,registry,updater,rollback,backup,doctor,service,baota,security}/
│   └── {platform,auth,tenant,website,origin,cache,ruleengine,node,cluster,scheduler,dns,certificate,waf,security,l4proxy,monitoring,reporter,billing,notification,cms,seo,media,plugin}/
├── pkg/  proto/
├── frontend/{website,admin,console,shared}/
├── docs-site/  status-site/
├── edge/{templates,lua,tests}/
├── migrations/  manifests/  configs/
├── deploy/{baota,systemd,nginx,openresty,install}/
├── scripts/
├── tests/{integration,e2e,performance,security}/
├── docs/{prd,architecture,database,api,operations,security,bmad}/
├── Makefile  go.mod  README.md
```

---

## 第二十一章：安装与部署

- **21.1 主控**：`lingedge install server` 安装 Server、配置、systemd、数据目录、Web 安装向导。流程：环境检查 → MySQL 配置 → Redis 配置 → 数据库初始化 → 创建管理员 → 系统基础配置 → 内部身份与密钥初始化 → 完成 → `install.lock`。**不得使用公开固定管理员默认密码。**
- **21.2 节点**：`lingedge install node` 安装 Go Agent、OpenResty、Nginx 模板、缓存/日志目录、systemd；`lingedge node enroll` 完成注册。
- **21.3 DNS**：`lingedge install dns`，独立 DNS 服务器；考虑权威 DNS 冗余、UDP/TCP 53 端口、与现有 DNS 服务冲突。
- **21.4 官网**：`lingedge install website`，支持 Nuxt SSR 或预渲染静态站点；SSR 须明确 Node.js 运行依赖；**不得把 Nuxt SSR 当成可由纯 Go 二进制执行。**

---

## 第二十二章：Epic 划分

按 Epic → Story → Task 分解。

- **Epic 1 项目基础与 LingEdge Tools**：Go Monorepo、项目规范、数据库初始化、Migration、认证、RBAC、安装向导、Tools、Server 快速安装、基础升级机制、systemd。验收：全新 Linux 服务器可用 CLI 完成主控安装。
- **Epic 2 官方网站与 CMS**：Nuxt 网站、品牌设计、首页/产品/定价/解决方案、注册入口、CMS、SEO、多语言。验收：管理员可在后台修改官网首页并发布，用户可正常访问。
- **Epic 3 CDN 核心与 Node**：Node Agent、节点注册、集群、站点、源站、HTTPS、缓存、规则引擎、自定义真实 IP Header、配置下发与回滚。验收：真实网站可通过 LingEdge 节点访问并正常缓存。
- **Epic 4 DNS、调度与监控**：DNS Provider、自建权威 DNS、区域线路、权重、健康检查、故障切换、Reporter、流量统计、告警。验收：节点故障可识别，调度状态与切换结果可验证。
- **Epic 5 WAF 与四层代理**：WAF、CC、Bot、IP/ASN/Geo 限制、安全日志、TCP/UDP、四层健康检查。验收：规则真实生效，四层转发与安全模块经测试。
- **Epic 6 商业化与完整发布**：多租户、Console、套餐、计费、订单、支付接口、工单、插件体系、状态页、文档、Tools 全组件安装、升级回滚、备份、安全审计、性能测试、生产部署。验收：官网注册 → 创建站点 → CDN 接入 → 流量计费 → 账单 → 运维管理完整流程。
高级能力（L2 缓存、HTTP/3、DNSSEC、边缘计算）需独立技术验证与后续 Story，不得未经测试宣布完成。

---

## 第二十三章：测试与质量

必须执行：Go 单元测试、Go Race、数据库迁移测试、API 集成测试、前端组件测试、E2E、Node 配置下发测试、OpenResty 配置测试、DNS 查询测试、规则引擎测试、WAF 误拦截测试、证书续期测试、缓存失效测试、节点故障测试、多租户隔离测试、计费对账测试、CLI 安装与升级测试、备份恢复测试、性能测试、安全测试。
必须记录：测试环境、步骤、实际结果、性能基线、未通过项、已知问题。**禁止虚构测试结果。**

---

## 第二十四章：安全与可靠性

节点身份认证、mTLS、API Token 安全、租户隔离、SSRF、SQL 注入、XSS、CSRF、命令注入、Nginx 配置注入、Header 伪造、路径穿越、DNS 接口滥用、证书私钥保护、源站安全、安装包签名、软件供应链、配置版本与回滚、备份恢复、节点故障切换。
敏感凭据加密保存，日志脱敏。**不得将全部节点控制权限暴露给普通租户。**

---

## 第二十五章：Story 完成定义（DoD）

需求明确；代码完成；编译通过；数据库迁移完成；API 可调用；前端功能可用；权限正确；单元测试通过；集成测试通过；关键异常测试通过；文档更新；运行验证完成。
**测试环境缺失时必须标记「等待环境验证」，不得写成已完成。**

---

## 第二十六章：BMAD 工作要求

使用实际可用的 BMAD Agent/工作流，不假装调用未安装的 BMAD。首先检查：项目目录、Git、Go、Node.js、MySQL/Redis、BMAD 版本与工作流。
第一步产出（保存到 `docs/` 对应目录）：1 Product Brief　2 PRD　3 ADR　4 系统架构　5 数据库 ERD　6 OpenAPI 草案　7 Node gRPC 协议草案　8 Tools 技术设计　9 官网与 CMS 设计　10 Admin/Console 菜单与页面设计　11 Epic 与 Story　12 开发依赖关系　13 安全风险清单　14 测试与验收标准。
完成架构后初始化项目并执行 Epic 1。依赖明确、边界稳定时可并行开发官网与基础平台；**不得让并行开发产生不一致的 API、数据库结构或重复实现。**

---

## 第二十七章：最终验收闭环

- A CLI 安装：下载 Tools → 安装主控 → Web 初始化 → 安装 Console → 安装 Node → 注册节点 → 安装 DNS → 查询状态
- B 官网：访问 → 浏览产品 → 查看套餐 → 注册登录 → 进入控制台
- C CDN：创建站点 → 绑定域名 → 设置源站 → 绑定节点 → 申请证书 → DNS 接入 → 加速 → 缓存命中
- D 规则：创建 → 校验 → 发布 → 节点执行 → 查看命中 → 更新 → 回滚
- E 真实 IP：设置 `Ling-Client-IP` → 节点自动配置 → 源站 Nginx/WAF 读取 → 正确识别
- F 调度：节点在线 → 健康检查 → 故障 → 自动摘除 → 备用节点 → 恢复上线
- G 安全：WAF 规则 → 检测 → 阻断/放行 → 日志 → 调整 → 再验证
- H 商业：购买套餐 → 开通 → 使用 → 计量 → 账单 → 付款对账
- I CMS：修改首页 → 预览 → 发布 → 前台更新 → 版本回滚
- J 升级：Tools 检测版本 → 校验包 → 升级 → 健康检查 → 失败回滚 → 服务恢复

---

## 第二十八章：执行优先级

以本文为唯一需求基线。先完成需求分析、架构设计、数据库模型、API 规范、模块依赖、Epic/Story 拆解，再进入真实开发。
**优先交付：LingEdge Tools + LingEdge Server + 基础 Admin + 官网框架 + 可运行的 CDN Node。** 之后逐步实现 DNS、调度、WAF、商业计费、高级扩展。
严格遵守：独立研发；Go 主后端；MySQL 8.0；Redis 8；Vue 3 / Arco Design Pro；Nuxt 官网；OpenResty 边缘引擎；宝塔 + systemd；不使用 Docker；完整测试；可安装、可升级、可回滚。
所有品牌、程序、文档、界面统一为 **LingEdge（凌界云）**。
