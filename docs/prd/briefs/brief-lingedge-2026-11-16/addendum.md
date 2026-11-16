# Addendum: LingEdge Product Brief 补充材料

> 来源：docs/prd/LINGEDGE_V2_REQUIREMENTS.md。此处保存对下游文档（PRD、架构、Epic）有用但不适合放进 1–2 页 Brief 的深度内容。

## A. 模块清单与建议交付批次（批次为本 Brief 的推断，待 PRD 确认）

| 模块 | 形态 | 建议批次 |
|---|---|---|
| Tools（lingedge） | Go 二进制 + TUI | 1 |
| Server | Go 模块化单体（内置 Scheduler/Worker） | 1 |
| Admin | Vue3 + Arco | 1（基础）→ 逐步 |
| Node（Agent + OpenResty） | Go + OpenResty | 1–2 |
| Website | Nuxt 3 | 1（框架）→ 2（CMS） |
| DNS / Reporter / Scheduler 独立化 | Go | 3 |
| WAF/CC / L4 | OpenResty/Lua / Nginx Stream | 4 |
| Console / 计费 / 插件 / Docs / Status | Vue3 / 其他 | 5 |

## B. 基线"硬约束"风险表（需在架构/ADR/测试中逐项落地）

| 约束 | 风险 | 对应验证方式 |
|---|---|---|
| 主控离线节点继续服务 | 配置缓存/证书到期、Agent 依赖主控 | 故障注入测试 |
| 配置不可变+签名+灰度+回滚 | 回滚遇不可逆 Migration | 升级回滚测试，Migration 分级 |
| 不得拼接不可信输入生成 Nginx 指令 | 配置注入 | 模板白名单 + 安全测试 |
| 缓存规则与动态内容隔离 | 跨用户缓存泄漏 | Cache Key/Cookie/Vary 策略测试 |
| 目录/通配符刷新可验证 | 扫描删除不可控 | 方案 PoC，禁止盲删缓存目录 |
| 边缘限流不强依赖 Redis | 远程依赖拖垮延迟/可用性 | 本地限流 + 异步汇总设计 |
| 调度摘除防抖 | 单探针误报 | 多探针/多次采样 + 恢复阈值 |
| 计费幂等可对账 | 重复计量、浮点误差 | 定点金额/整数最小单位，对账测试 |
| 租户隔离服务端强制 | 越权 | 多租户隔离测试 |
| Tools 供应链安全 | 路径穿越、未验签包 | 签名清单、SHA256、安全解压测试 |
| DDoS 边界 | 对外夸大能力 | 文案与产品说明明确边界 |
| HTTP/3 | 仅握手成功即宣称完成 | 与代理/安全模块兼容性验证 |

## C. 被明确排除/拒绝的方向（来自基线）

- Docker/Compose 作必选组件；Kubernetes 依赖。
- 独立 lingedge-updater（并入 Tools）。
- 第一阶段拆微服务。
- 官网套餐独立维护第二套结算价格（必须读取正式产品套餐）。
- Nuxt SSR 当作纯 Go 二进制执行。
- 任意 Shell 命令执行入口。

## D. 参考对象（仅公开功能与理念，不作对比结论）

CDN1（组件化部署、节点集中管理）、Cloudflare（站点规则）、腾讯云 EdgeOne（网站配置体验）、阿里云 ESA（缓存与安全）、Certimate（证书 Provider 扩展）。

## E. 交给 PRD 的建议问题

1. 自用模式与商业模式的功能开关粒度与默认值。
2. 首批验收环境清单（OS、架构、宝塔版本、MySQL/Redis 部署方式）。
3. 性能基线的测量方法与"可接受"门槛由谁定。
4. 访问日志存储选型候选与评估标准。
5. 第一批 Story 中"可验证"的最小真实流量场景。
