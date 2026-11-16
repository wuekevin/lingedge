---
title: "数据库：官网 CMS（website_*）"
status: draft
created: 2026-11-16
updated: 2026-11-16
---

# 官网 CMS 表（P2）

> 返回 [README](README.md) · [architecture.md](../architecture/architecture.md) · 设计见 [website-cms-design.md](../architecture/website-cms-design.md)。全部为**平台级**（无 `tenant_id`；官网为平台所有）。M1 官网为纯静态 SSG，不依赖本组表。通用列简写 C1 / SD 见 README §2.1。

## 0. 设计原则

1. **无第二套价格**（FR-45、R-12）：`website_*` 表**不含任何价格/金额列**（命名含 `price`/`amount`/`currency` 的列禁止；CI 对 `information_schema.columns` 做检查）。套餐展示仅存 `plans.uid`（`website_page_blocks.props_json.plan_refs`）或产品引用（`website_products.ref_product_id`），渲染时由 `GET /public/plans` 读取正式 `plans`。
2. **多语言**：非本地化属性在实体表；本地化文本统一放 `website_localizations(entity_type, entity_id, locale)`，`fields_json` 存各字段（title/summary/body/alt/label…）。语言集合由 `website_settings.locales` 配置，默认 `zh-CN`、`en`。
3. **内容版本/发布**：页面使用专用 `website_page_versions`（整页快照：块、本地化、SEO）；其他实体（文章、FAQ、产品、解决方案、横幅）用补充表 `website_content_versions`。发布态以 `published_version_id` 指针为准；草稿为工作态（实体表 + 本地化表的当前内容）。
4. **预览**：预览令牌为**不透明随机令牌**，库内仅存 SHA-256，存于独立表 `website_preview_links`（§6.2）：一个版本可有多个并存、可单独撤销的链接；版本行**不再**含预览列（M17）。预览只读版本快照。
5. **定时发布**：`scheduled_version_id` + `scheduled_publish_at`（+ 失败时的 `schedule_error`），由 Worker 任务 `cms.publish_due`（幂等）到点把该版本置为已发布并触发 SSR 缓存失效/可选静态重新生成。
7. **MySQL 约束与迁移顺序**（2026-11-16 实测，8.0.46）：① CHECK 只可引用 FK 动作为 RESTRICT/NO ACTION/`ON DELETE CASCADE` 的列，**`ON DELETE SET NULL` 的 FK 列不得被 CHECK 引用**（ERROR 3823）——本文件所有 CHECK 仅引用无 FK 的列，已核对；② 唯一性生成列默认 **VIRTUAL**（STORED 生成列的基列不可有 CASCADE/SET NULL 的 FK，ERROR 1215）；③ **循环外键** `website_pages.current_version_id/published_version_id ↔ website_page_versions.page_id`、`website_products/solutions/articles/faqs.published_version_id/scheduled_version_id ↔ website_content_versions`：迁移先建表（版本列为无 FK 的普通列），再以 `ALTER TABLE … ADD CONSTRAINT` 追加 FK（与 README §2.4 一致，expand 迁移）；④ 全部表无分区、无外键指向分区表。
6. **真实数据原则**（FR-41）：数据驱动区块以 `data_source` 标注（`public_stats`/`plans`），无数据时前端隐藏或标注“示意”。

## 1. 页面

### 1.1 `website_pages`　**P2**
用途：页面（首页、产品页、关于、自定义落地页…）。通用列：C1 + SD。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| page_key | VARCHAR(96) | N | — | 稳定键：`home/about/pricing/product-cdn/…`，唯一 |
| page_type | VARCHAR(16) | N | 'standard' | `home/standard/landing/system` |
| template | VARCHAR(64) | N | — | 前端模板标识 |
| status | VARCHAR(12) | N | 'draft' | `draft/scheduled/published/archived` |
| current_version_id | BIGINT UNSIGNED | Y | — | 最新草稿版本；FK → website_page_versions(id) SET NULL |
| published_version_id | BIGINT UNSIGNED | Y | — | 线上版本；FK → website_page_versions(id) SET NULL |
| scheduled_publish_at | DATETIME(3) | Y | — | |
| published_at | DATETIME(3) | Y | — | |
| visibility_mode | VARCHAR(16) | N | 'always' | `always/commercial_only/self_hosted_only`（D-3：自用模式隐藏商业页） |
| sort_order | INT | N | 0 | |
| seo_id | BIGINT UNSIGNED | Y | — | FK → website_seo(id) SET NULL |
| active_key | VARCHAR(96) | Y | — | 生成列 `IF(deleted_at IS NULL, page_key, NULL)`，**VIRTUAL** |
| scheduled_version_id | BIGINT UNSIGNED | Y | — | 待到点发布的版本；FK → website_page_versions(id) SET NULL（循环外键，ALTER 追加）；`status='scheduled'` 时必填（Service 保证，不入 CHECK 以免引用 SET NULL 列） |
| schedule_error | VARCHAR(255) | Y | — | `cms.publish_due` 引用校验失败的原因码（如 `media_missing`、`plan_unpublished`、`link_unpublished`）；成功发布/改期/取消时清空 |

- 唯一：`uk_website_pages_key(active_key)`。索引：`idx_website_pages_status(status, scheduled_publish_at)`。CHECK：枚举；`status<>'scheduled' OR scheduled_publish_at IS NOT NULL`。

### 1.2 `website_page_versions`　**P2**
用途：页面不可变内容快照（回滚=以旧快照生成新版本）。C1（无 `updated_at` 之外的可变列：仅 `status/published_at` 可更新）。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| page_id | BIGINT UNSIGNED | N | — | FK → website_pages(id) RESTRICT |
| version_no | INT UNSIGNED | N | — | 页面内递增 |
| status | VARCHAR(12) | N | 'draft' | `draft/published/superseded` |
| snapshot_json | JSON | N | — | 整页快照：`blocks[]`、`localizations{locale:fields}`、`seo{}` |
| snapshot_sha256 | CHAR(64) | N | — | |
| change_note | VARCHAR(512) | Y | — | |
| rollback_of_version_id | BIGINT UNSIGNED | Y | — | 自引用 FK SET NULL |
| created_by | BIGINT UNSIGNED | Y | — | FK → users(id) SET NULL |
| published_at | DATETIME(3) | Y | — | |

- 唯一：`uk_website_page_versions(page_id, version_no)`（预览链接见 §6.2，不再在版本行存令牌哈希）。保留：每页最近 50 个及已发布版本 `[ASSUMPTION]`。`snapshot_*`、`version_no` 不可变（触发器，同 `rule_versions` 模式）。

### 1.3 `website_page_blocks`　**P2**
用途：页面工作态的模块（区块）与排序（“模块排序”）。通用列：C1。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| page_id | BIGINT UNSIGNED | N | — | FK → website_pages(id) CASCADE |
| block_type | VARCHAR(48) | N | — | 取值**以 `pkg/cmsblocks/schemas/*.json` 的文件名为准**（website-cms-design §7：`hero`、`feature_grid`、`product_cards`、`node_map`、`stat_group`、`plan_table`、`faq_list`、`solutions_grid`、`advantages`、`cta`、`rich_text`、`image`、`security_boundary_notice` 等）；**数据库不设 CHECK**（新增区块类型无需迁移），由 Service 按 Schema 注册表校验 |
| sort_order | INT | N | 0 | |
| enabled | TINYINT(1) | N | 1 | |
| props_json | JSON | N | — | 非本地化属性（图片引用 media uid、链接、`plan_refs`） |
| data_source | VARCHAR(16) | Y | — | `public_stats/plans`（真实数据来源标注） |

- 索引：`idx_wpb_page(page_id, sort_order)`。本地化文本在 `website_localizations(entity_type='page_block')`。

## 2. 导航、横幅

### 2.1 `website_navigation`　**P2**
通用列：C1。列：`area VARCHAR(8) N`（`header/footer`）、`parent_id`（自引用 FK CASCADE）、`link_type VARCHAR(12) N`（`page/url/product/solution/article/none`）、`target_page_id`（FK→website_pages SET NULL）、`url VARCHAR(512) Y`（仅 `link_type='url'`，须经 URL 白名单校验，禁止 `javascript:`）、`open_in_new TINYINT(1) N 0`、`visibility_mode VARCHAR(16) N 'always'`、`sort_order INT N 0`、`enabled TINYINT(1) N 1`。标签本地化。索引 `idx_wnav(area, parent_id, sort_order)`。

### 2.2 `website_banners`　**P2**
通用列：C1。列：`placement VARCHAR(32) N`、`image_media_id`（FK→website_media SET NULL）、`link_url VARCHAR(512) Y`、`start_at/end_at DATETIME(3) Y`、`sort_order INT N 0`、`enabled TINYINT(1) N 1`。文案本地化。

## 3. 产品、解决方案

### 3.1 `website_products`　**P2**
用途：官网产品介绍（CDN、WAF、智能 DNS、四层、SSL、数据分析）。**仅引用正式产品，不含价格**。通用列：C1。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| product_key | VARCHAR(64) | N | — | 唯一，如 `cdn`、`waf` |
| slug | VARCHAR(96) | N | — | URL 片段 |
| ref_product_id | BIGINT UNSIGNED | Y | — | 可选关联正式产品 `products(id)`，FK SET NULL（P5 追加）；价格展示一律经 `plans` |
| icon_media_id | BIGINT UNSIGNED | Y | — | FK → website_media(id) SET NULL |
| page_id | BIGINT UNSIGNED | Y | — | 详情页，FK → website_pages(id) SET NULL |
| status | VARCHAR(12) | N | 'draft' | `draft/scheduled/published/archived` |
| scheduled_publish_at / published_at | DATETIME(3) | Y | — | |
| published_version_id | BIGINT UNSIGNED | Y | — | FK → website_content_versions(id) SET NULL（循环外键，ALTER 追加） |
| scheduled_version_id | BIGINT UNSIGNED | Y | — | 待到点发布的内容版本；FK → website_content_versions(id) SET NULL |
| schedule_error | VARCHAR(255) | Y | — | 同 `website_pages.schedule_error` |
| sort_order | INT | N | 0 | |

- 唯一：`uk_website_products_key(product_key)`、`uk_website_products_slug(slug)`。

### 3.2 `website_solutions`　**P2**
同 `website_products` 结构（无 `ref_product_id`）：`solution_key`（唯一）、`slug`、`icon_media_id`、`page_id`、`status`、`scheduled_publish_at`、`scheduled_version_id`、`schedule_error`、`published_at`、`published_version_id`、`sort_order`。

## 4. 文章、分类、FAQ

### 4.1 `website_categories`　**P2**
通用列：C1。列：`category_type VARCHAR(12) N`（`article/faq/help`）、`slug VARCHAR(96) N`、`parent_id`（自引用 FK SET NULL）、`sort_order INT N 0`。唯一 `uk_wcat(category_type, slug)`。名称本地化。

### 4.2 `website_articles`　**P2**
用途：新闻公告、博客、更新日志、帮助文章。通用列：C1。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| article_type | VARCHAR(12) | N | — | `news/announcement/blog/changelog/help` |
| slug | VARCHAR(160) | N | — | |
| category_id | BIGINT UNSIGNED | Y | — | FK → website_categories(id) SET NULL |
| author_user_id | BIGINT UNSIGNED | Y | — | FK → users(id) SET NULL |
| cover_media_id | BIGINT UNSIGNED | Y | — | FK → website_media(id) SET NULL |
| status | VARCHAR(12) | N | 'draft' | `draft/scheduled/published/archived` |
| pinned | TINYINT(1) | N | 0 | |
| scheduled_publish_at / published_at | DATETIME(3) | Y | — | |
| published_version_id | BIGINT UNSIGNED | Y | — | FK → website_content_versions(id) SET NULL |
| scheduled_version_id | BIGINT UNSIGNED | Y | — | FK → website_content_versions(id) SET NULL；`schedule_error VARCHAR(255) NULL` 同 pages |
| seo_id | BIGINT UNSIGNED | Y | — | FK → website_seo(id) SET NULL |

- 唯一：`uk_website_articles_slug(article_type, slug)`。索引：`idx_website_articles_list(status, article_type, published_at)`、`idx_website_articles_due(status, scheduled_publish_at)`。标题/摘要/正文（Markdown→净化 HTML，防 XSS）在 `website_localizations.fields_json`。

### 4.3 `website_faqs`　**P2**
通用列：C1。列：`category_id`（FK→website_categories SET NULL）、`sort_order INT N 0`、`status VARCHAR(12) N 'draft'`、`scope VARCHAR(16) N 'general'`（`general/product/pricing`）、`published_version_id`（FK→website_content_versions SET NULL）、`scheduled_publish_at DATETIME(3) Y`、`scheduled_version_id`（FK→website_content_versions SET NULL）、`schedule_error VARCHAR(255) Y`。问答本地化。

## 5. 媒体、SEO、设置、本地化

### 5.1 `website_media`　**P2**
通用列：C1 + SD。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| file_name | VARCHAR(255) | N | — | 原始名（展示用，不参与路径） |
| storage_driver | VARCHAR(16) | N | 'local' | `local/<plugin_key>` |
| storage_key | VARCHAR(255) | N | — | 受控存储键，**内容寻址**：服务端生成 `<yyyy>/<mm>/<sha256>.<ext>`（与 website-cms-design §12.1 一致），根目录为公开媒体目录 `/var/lib/lingedge/media/`；**禁止采用用户提供的路径** |
| mime_type | VARCHAR(100) | N | — | 白名单：png/jpeg/webp/svg(净化)/gif/avif/mp4(视情)… |
| size_bytes | BIGINT UNSIGNED | N | — | |
| width / height | INT UNSIGNED | Y | — | |
| sha256 | CHAR(64) | N | — | 去重；与 `storage_key` 一一对应 |
| active_sha256 | CHAR(64) | Y | — | 生成列 `IF(deleted_at IS NULL, sha256, NULL)`，**VIRTUAL** |
| uploaded_by | BIGINT UNSIGNED | Y | — | FK → users(id) SET NULL |

- 唯一：`uk_website_media_active_sha(active_sha256)`——相同内容只有一条活动媒体行、一个文件（重复上传复用该行；软删除后可重新上传同内容）。`storage_key` 因含 sha256 而天然唯一，另设普通索引 `idx_website_media_key(storage_key)`（不设唯一，避免软删除行与新行冲突）。替代文本本地化。

### 5.2 `website_seo`　**P2**
用途：非本地化 SEO 属性（1:1 于实体）；标题/描述/OG 文案在本地化表。通用列：C1。列：`entity_type VARCHAR(32) N`、`entity_id BIGINT UNSIGNED N`（多态，无 FK）、`robots VARCHAR(32) N 'index,follow'`、`canonical_override VARCHAR(512) Y`、`og_image_media_id`（FK→website_media SET NULL）、`sitemap_priority SMALLINT UNSIGNED N 5`（0–10，整数，十分之一为单位）、`changefreq VARCHAR(10) N 'weekly'`、`in_sitemap TINYINT(1) N 1`。唯一 `uk_website_seo(entity_type, entity_id)`。

### 5.3 `website_settings`　**P2**
用途：站点级设置键值（品牌 Logo、默认语言、启用语言、主题、页脚版权、统计脚本 ID、备案号文本等）。通用列：C1。列：`setting_key VARCHAR(96) N`（唯一）、`group_name VARCHAR(32) N`、`value_json JSON N`。品牌名等与 `system_settings(brand.*)` 同源时读系统设置，避免两处维护。

### 5.4 `website_localizations`　**P2**
用途：所有 CMS 实体的多语言字段。多态，无外键（由 Service 校验实体存在）。C1。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| entity_type | VARCHAR(32) | N | — | `page/page_block/product/solution/article/category/faq/banner/navigation/media/seo/settings` |
| entity_id | BIGINT UNSIGNED | N | — | |
| locale | VARCHAR(8) | N | — | `zh-CN/en`（受 `website_settings.locales` 约束） |
| fields_json | JSON | N | — | `{title,summary,body,label,alt,question,answer,seo_title,seo_description,og_title…}`；`body` 为受限 Markdown 源文本，渲染时净化 |
| status | VARCHAR(12) | N | 'draft' | `draft/translated/reviewed` |
| updated_by | BIGINT UNSIGNED | Y | — | FK → users(id) SET NULL |

- 唯一：`uk_website_loc(entity_type, entity_id, locale)`；索引：`idx_website_loc_entity(entity_type, entity_id)`。

## 6. 补充：通用内容版本

### 6.1 `website_content_versions`　**补充 · P2**
用途：非页面实体（文章/FAQ/产品/解决方案/横幅）的不可变内容快照与预览/回滚。C1（可变：`status/published_at`）。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| entity_type | VARCHAR(32) | N | — | 多态，无 FK |
| entity_id | BIGINT UNSIGNED | N | — | |
| version_no | INT UNSIGNED | N | — | |
| status | VARCHAR(12) | N | 'draft' | `draft/published/superseded` |
| snapshot_json | JSON | N | — | 实体属性 + 各语言字段 + SEO |
| snapshot_sha256 | CHAR(64) | N | — | |
| change_note | VARCHAR(512) | Y | — | |
| rollback_of_version_id | BIGINT UNSIGNED | Y | — | 自引用 FK SET NULL |
| created_by | BIGINT UNSIGNED | Y | — | FK → users(id) SET NULL |
| published_at | DATETIME(3) | Y | — | |

- 唯一：`uk_wcv(entity_type, entity_id, version_no)`。预览链接见 §6.2。保留：每实体最近 50 个及已发布版本 `[ASSUMPTION]`。

### 6.2 `website_preview_links`　**补充 · P2**

用途：CMS 预览链接（页面与通用内容实体共用）。**不透明随机令牌**（≥32B CSPRNG，与会话/API Token 同模式，ADR-018）：库内仅存 SHA-256，明文只在创建时返回一次；撤销＝置 `revoked_at`；一个版本可并存多个链接（取代原先“版本行 1 个令牌哈希 + `jti` 黑名单”的设计，M17）。通用列：C1。平台级。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| entity_type | VARCHAR(32) | N | — | `page/product/solution/article/faq/banner/…`（多态，无 FK） |
| entity_id | BIGINT UNSIGNED | N | — | |
| version_id | BIGINT UNSIGNED | N | — | 预览的草稿版本；页面指 `website_page_versions.id`，其它实体指 `website_content_versions.id`（多态，由 `entity_type` 决定；无 FK，Service 校验） |
| locale | VARCHAR(8) | N | — | |
| token_hash | CHAR(64) | N | — | SHA-256(令牌) |
| expires_at | DATETIME(3) | N | — | 默认创建后 ≤30 分钟（官网设置可调） |
| revoked_at | DATETIME(3) | Y | — | |
| created_by | BIGINT UNSIGNED | Y | — | FK → users(id) SET NULL |

- 唯一：`uk_wpl_token(token_hash)`；索引：`idx_wpl_entity(entity_type, entity_id, expires_at)`。CHECK：无。保留：过期后 7 天批量删除。令牌交换与 cookie 流程见 website-cms-design §4.4。

## 7. 修订记录 2026-11-16

- M17：新增 `website_preview_links`，版本表删除 `preview_token_hash/preview_expires_at` 与 `uk_wpv_preview/uk_wcv_preview`；页面/产品/解决方案/文章/FAQ 增 `scheduled_version_id`、`schedule_error`；`block_type` 取值改以 `pkg/cmsblocks/schemas` 为准且不设 CHECK；媒体 `storage_key` 改内容寻址、活动行 `sha256` 唯一（`active_sha256` VIRTUAL）。
- BLK-01/H1 相关：补 §0.7 的 MySQL 约束与循环外键迁移顺序；`active_key` 改 VIRTUAL；核对全部 CHECK 均未引用 SET NULL 的 FK 列（通过）。
