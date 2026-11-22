---
name: LingEdge 凌界云
description: 企业级 CDN 边缘云平台的视觉身份。深蓝科技风 + 蓝紫渐变；Admin/Console 基于 Arco Design Pro 仅做主题层定制，官网（Tailwind）与后台共用同一套设计令牌。
status: draft
updated: 2026-11-16
sources:
  - docs/prd/LINGEDGE_V2_REQUIREMENTS.md   # §2.2 §2.3 §5.1（唯一权威）
  - docs/prd/prd.md                        # FR-30 FR-40 FR-41 NFR-29
colors:
  # ---------- 亮色主题（语义令牌，{colors.*}）----------
  brand-primary: '#3B4FD8'          # 品牌蓝：主按钮、链接、选中态；白字对比度 6.37
  brand-primary-hover: '#2F42C4'    # 悬停/按下；白字对比度 7.79
  brand-violet: '#6D4FE0'           # 渐变终点、强调图形；白字对比度 5.46
  on-brand: '#FFFFFF'
  bg-page: '#F5F7FB'                # 页面底
  bg-surface: '#FFFFFF'             # 卡片/表格/抽屉
  bg-surface-raised: '#FFFFFF'      # 浮层（配合阴影）
  bg-sunken: '#EDF0F7'              # 代码块、输入底、表头
  border-default: '#DCE1EC'
  border-strong: '#B7C0D4'
  text-primary: '#1D2129'           # 对比度 16.13
  text-secondary: '#5A6478'         # 对比度 5.95（正文次级的下限，禁止再浅）
  text-disabled: '#8A93A6'          # 仅用于禁用态，不承载必读信息（对比度 3.09，已知例外）
  success: '#067647'
  success-bg: '#E7F6EC'
  warning: '#B54708'
  warning-bg: '#FEF0C7'
  danger: '#B42318'
  danger-bg: '#FEE4E2'
  info: '#0B5CAD'
  info-bg: '#E1EFFE'
  # ---------- 暗色主题（同名 + -dark 后缀；运行时由 data-theme 切换，不在组件内写分支）----------
  brand-primary-dark: '#7C93FF'     # 在 {colors.bg-page-dark} 上对比度 6.62
  brand-primary-hover-dark: '#9AAEFF'
  brand-violet-dark: '#A78BFA'
  on-brand-dark: '#0B1226'
  bg-page-dark: '#0B1226'           # 深海军蓝，非纯黑
  bg-surface-dark: '#111A33'
  bg-surface-raised-dark: '#182244'
  bg-sunken-dark: '#0D152B'
  border-default-dark: '#263256'
  border-strong-dark: '#3A4A7A'
  text-primary-dark: '#E6EBFA'      # 对比度 15.62
  text-secondary-dark: '#A9B4D0'    # 对比度 8.30
  text-disabled-dark: '#6B7799'
  success-dark: '#47CD89'
  success-bg-dark: '#0F2E22'
  warning-dark: '#FDB022'
  warning-bg-dark: '#33260A'
  danger-dark: '#F97066'
  danger-bg-dark: '#3A1512'
  info-dark: '#5CA8FF'
  info-bg-dark: '#0E2542'
  # ---------- 数据可视化（ECharts）8 色序列，亮/暗各一套，色盲友好顺序，禁止仅靠颜色区分系列 ----------
  chart-1: '#3B4FD8'
  chart-2: '#0E9384'
  chart-3: '#D17A1F'                # 在白底对比度 3.22（≥3:1 的非文本要求；Story 1.37b 由 #D9822B 的 2.93 调整，仍待 UD-14 确认）
  chart-4: '#6D4FE0'
  chart-5: '#C11574'
  chart-6: '#0B5CAD'
  chart-7: '#667085'
  chart-8: '#7A5AF8'
  chart-1-dark: '#7C93FF'
  chart-2-dark: '#2ED3B7'
  chart-3-dark: '#FDB022'
  chart-4-dark: '#A78BFA'
  chart-5-dark: '#F670C7'
  chart-6-dark: '#5CA8FF'
  chart-7-dark: '#98A2B3'
  chart-8-dark: '#BDB4FE'
typography:
  display:        { fontFamily: 'Inter, PingFang SC, Microsoft YaHei, Noto Sans SC, sans-serif', fontSize: 48px, fontWeight: '700', lineHeight: '1.15', letterSpacing: -0.02em }   # 仅官网 Hero
  heading-1:      { fontFamily: 'Inter, PingFang SC, Microsoft YaHei, Noto Sans SC, sans-serif', fontSize: 32px, fontWeight: '700', lineHeight: '1.25' }
  heading-2:      { fontFamily: 'Inter, PingFang SC, Microsoft YaHei, Noto Sans SC, sans-serif', fontSize: 24px, fontWeight: '600', lineHeight: '1.3' }
  heading-3:      { fontFamily: 'Inter, PingFang SC, Microsoft YaHei, Noto Sans SC, sans-serif', fontSize: 18px, fontWeight: '600', lineHeight: '1.4' }
  page-title:     { fontFamily: 'Inter, PingFang SC, Microsoft YaHei, Noto Sans SC, sans-serif', fontSize: 20px, fontWeight: '600', lineHeight: '1.4' }   # 后台页标题
  body:           { fontFamily: 'Inter, PingFang SC, Microsoft YaHei, Noto Sans SC, sans-serif', fontSize: 14px, fontWeight: '400', lineHeight: '1.6' }   # 后台正文
  body-lg:        { fontFamily: 'Inter, PingFang SC, Microsoft YaHei, Noto Sans SC, sans-serif', fontSize: 16px, fontWeight: '400', lineHeight: '1.7' }   # 官网正文
  caption:        { fontFamily: 'Inter, PingFang SC, Microsoft YaHei, Noto Sans SC, sans-serif', fontSize: 12px, fontWeight: '400', lineHeight: '1.5' }
  label:          { fontFamily: 'Inter, PingFang SC, Microsoft YaHei, Noto Sans SC, sans-serif', fontSize: 13px, fontWeight: '500', lineHeight: '1.4' }
  metric:         { fontFamily: 'Inter, PingFang SC, Microsoft YaHei, Noto Sans SC, sans-serif', fontSize: 28px, fontWeight: '600', lineHeight: '1.2', fontFeatureSettings: '"tnum"' }   # 数字卡片，等宽数字
  mono:           { fontFamily: 'JetBrains Mono, SF Mono, Menlo, Consolas, monospace', fontSize: 13px, fontWeight: '400', lineHeight: '1.6' }   # Header 名、Nginx 片段、命令、配置版本号
rounded:
  sm: 4px      # 标签、输入内部元素
  md: 6px      # 按钮、输入框、表格内控件
  lg: 10px     # 卡片、抽屉
  xl: 16px     # 官网大卡片、Hero 面板
  full: 9999px # 状态胶囊、头像
spacing:
  '1': 4px
  '2': 8px
  '3': 12px
  '4': 16px
  '5': 20px
  '6': 24px
  '8': 32px
  '12': 48px
  '16': 64px
  gutter-mobile: 16px
  gutter-desktop: 24px
  content-max-admin: 1440px
  content-max-website: 1200px
  sidebar-width: 220px
  sidebar-width-collapsed: 48px
components:
  button-primary:    { background: '{colors.brand-primary}', foreground: '{colors.on-brand}', radius: '{rounded.md}', height: 32px }
  button-primary-hero: { background: 'linear-gradient(135deg, {colors.brand-primary} 0%, {colors.brand-violet} 100%)', foreground: '{colors.on-brand}', radius: '{rounded.md}', height: 44px }
  button-danger:     { background: '{colors.danger}', foreground: '{colors.on-brand}', radius: '{rounded.md}' }
  status-pill:       { radius: '{rounded.full}', typography: '{typography.caption}', paddingX: '{spacing.2}' }
  card:              { background: '{colors.bg-surface}', border: '1px solid {colors.border-default}', radius: '{rounded.lg}' }
  code-block:        { background: '{colors.bg-sunken}', foreground: '{colors.text-primary}', typography: '{typography.mono}', radius: '{rounded.md}' }
  publish-bar:       { background: '{colors.warning-bg}', foreground: '{colors.warning}', border: '1px solid {colors.warning}', radius: '{rounded.md}' }
  acting-as-banner:  { background: '{colors.info-bg}', foreground: '{colors.info}', height: 32px }
  illustrative-badge: { background: '{colors.warning-bg}', foreground: '{colors.warning}', radius: '{rounded.sm}', typography: '{typography.caption}' }
---

# LingEdge（凌界云）视觉身份 · DESIGN.md

> 本文件拥有「长什么样」。「怎么工作」见 `EXPERIENCE.md`。二者与任何线框/Mock 冲突时，以这两份为准。
> 基线依据：§5.1（深蓝科技风、蓝紫渐变、企业级布局、响应式、深/浅主题、中英双语）、§2.2（Arco Design Pro）、§2.3（Tailwind）。**色值、字体、圆角属于 UX 提议**（基线只给了风格关键词），用户可整体推翻，推翻只影响本文件 frontmatter 与 `frontend/shared/tokens`。

## Brand & Style

LingEdge 的气质是「**可信赖的基础设施**」：冷静、精确、有技术底气，但不炫技。它面对两类人：在终端里装机的运营者，和每天在后台盯着发布状态的站点所有者。因此：

- **深蓝**承担「稳」：暗色主题以深海军蓝（`{colors.bg-page-dark}`）而非纯黑为底，长时间盯屏不刺眼；亮色主题以冷灰蓝白（`{colors.bg-page}`）为底。
- **蓝紫渐变**承担「智能 / 边缘 / 凌界」的品牌记忆点，只用在**品牌时刻**：官网 Hero、官网 CTA 按钮、登录页侧栏、安装向导欢迎页、CLI 的 Logo 横幅。**后台的数据区、表格、表单不使用渐变**——数据界面要求色彩有语义。
- 视觉密度分两档：**官网**（宽松、大留白、大标题、插画/网络拓扑图形）与**后台**（紧凑、14px 正文、32px 控件高度、信息密度优先）。两档共享颜色/字体/圆角令牌，不共享间距密度。
- 品牌文字：中文界面写「LingEdge（凌界云）」首次出现、之后「凌界云」或「LingEdge」；英文界面只写「LingEdge」。禁止出现其他产品名残留（NFR-29）。
- 不使用写实 3D 地球或夸张光效作为**数据**载体；节点地图是真实数据的容器，不是装饰（见 EXPERIENCE.md 的官网数据真实性规范）。

## Colors

**品牌色**
- `{colors.brand-primary}`（亮）/`{colors.brand-primary-dark}`（暗）：主按钮、链接、选中菜单、焦点环、进度。一屏内主色实心按钮至多一个。
- `{colors.brand-violet}`：只作为渐变终点与少量强调图形（官网插画、图表第 4 序列），不作文字色。
- 渐变 `--le-gradient-brand: linear-gradient(135deg, {colors.brand-primary} 0%, {colors.brand-violet} 100%)`（暗色主题端点换用 `-dark` 令牌并降低 10% 不透明度叠加在 `{colors.bg-page-dark}` 上，避免发光过强）。**渐变上的白字必须在两端色上对比度 ≥ 4.5**（已验证：6.37 / 5.46）。

**语义色**（后台状态的唯一色彩来源，亮/暗各一套，文字与背景成对使用）
| 语义 | 前景 | 背景 | 用在 |
|---|---|---|---|
| 成功 | `{colors.success}` | `{colors.success-bg}` | 已全部生效、节点在线、健康检查通过 |
| 警告 | `{colors.warning}` | `{colors.warning-bg}` | **已保存但未发布**、部分节点未生效、证书即将到期、示意数据角标 |
| 危险 | `{colors.danger}` | `{colors.danger-bg}` | 发布失败、节点离线、校验错误、不可逆操作 |
| 信息 | `{colors.info}` | `{colors.info-bg}` | 发布中、代管模式横幅、提示 |
| 中性 | `{colors.text-secondary}` | `{colors.bg-sunken}` | 草稿、已停用、未启用 |

**铁律**：任何状态**不得仅靠颜色传达**——必须同时有图标与文字（状态胶囊 = 图标 + 文案）。**警告色 = 「还有一步没做完」**，因此「已保存未发布」始终是琥珀色，从不用绿色。

**不要做的**：不用纯黑 `#000` 与纯白大面积对撞；不用紫色表示错误；不用红色表示「普通停用」；不在图表里仅用红绿区分两个系列。

## Typography

- 字体栈为**系统字体栈 + Inter（可选）**，中文回退 PingFang SC / Microsoft YaHei / Noto Sans SC。**不依赖外部 CDN 字体**：LingEdge 面向可私有化部署甚至离线部署的运营者（基线 §4.8 支持离线安装），字体文件（Inter 子集、JetBrains Mono 子集）随前端构建产物自托管。
- 数字一律用**等宽数字**（`font-feature-settings: "tnum"`），表格与指标卡的数值右对齐。
- `{typography.mono}` 用于一切「机器可读、需逐字符核对」的内容：自定义 Header 名、Nginx 片段、CLI 命令、配置版本号、IP/CIDR、Request ID。
- 中英文混排：中文正文不使用斜体；英文缩写（CDN、WAF、DNS、CIDR）保持原样，不强行汉化；中英文之间不手工加空格，由 `lang` 与字体回退处理。
- 官网 `{typography.display}` 仅用于 Hero，移动端降为 32px；后台最大标题为 `{typography.page-title}`。

## Layout & Spacing

- 间距基准 4px，常用 8/12/16/24。后台页面内边距 `{spacing.6}`；卡片间距 `{spacing.4}`；表单项纵向间距 `{spacing.5}`。
- **断点（沿用 Arco 栅格，避免自定义）**：xs `<576` · sm `≥576` · md `≥768` · lg `≥992` · xl `≥1200` · xxl `≥1600`。
- 后台外壳：顶栏 48px + 侧栏 `{spacing.sidebar-width}`（可折叠为 `{spacing.sidebar-width-collapsed}`）+ 多标签栏 36px + 内容区；内容区最大宽 `{spacing.content-max-admin}`，超宽屏居中而非拉伸表格。
- 官网：内容最大宽 `{spacing.content-max-website}`，12 栏网格；区块纵向节奏 `{spacing.16}`（桌面）/ `{spacing.12}`（移动）。
- 详细的各断点行为（侧栏、表格、编辑器如何降级）见 `EXPERIENCE.md` · 响应式。

## Elevation & Depth

- 亮色：用 1px `{colors.border-default}` 描边为主，阴影仅用于浮层（下拉、对话框、抽屉）：`0 8px 24px rgba(16,24,40,.12)`。
- 暗色：**用表面明度分层，不靠阴影**——`bg-page-dark` < `bg-surface-dark` < `bg-surface-raised-dark`，浮层加 1px `border-default-dark`。
- 层级只有四级：页面底、卡片/表格、浮层（下拉/Popover）、模态（对话框/抽屉）。不发明第五级。

## Shapes

圆角克制：控件 `{rounded.md}`（6px）、卡片 `{rounded.lg}`（10px）、官网大面板 `{rounded.xl}`、状态胶囊 `{rounded.full}`。表格单元格与代码块边角不做大圆角。图标采用 Arco 图标库线性风格，描边 1.5–2px，不混用填充与线性两套。

## Components

> 仅规定「视觉」；行为规则见 `EXPERIENCE.md` · 组件模式。凡 Arco 已有组件，**沿用 Arco 默认结构**，只通过主题令牌改色/圆角（见下「Arco 定制边界」）。

- **主按钮 `button-primary`**：实心 `{colors.brand-primary}`，高度 32px（后台）；官网 CTA 用 `button-primary-hero`（渐变、44px）。危险按钮 `button-danger` 仅用于二次确认对话框的最终确认键。
- **状态胶囊 `status-pill`**：图标 + 文案，语义色前景/背景成对。标准文案集合与含义是**产品语言的一部分**，在 `EXPERIENCE.md` · 配置状态表达 中固定，不得由页面自行发明。
- **发布条 `publish-bar`**：站点页顶部粘性条，琥珀色，仅在存在「已保存但未发布」更改时出现；发布完成后收敛为一行中性状态。
- **代管横幅 `acting-as-banner`**：管理员代租户操作时固定在顶栏下方，信息蓝，不可关闭。
- **代码块 `code-block`**：`{colors.bg-sunken}` 底、`{typography.mono}`、右上角「复制」按钮；Nginx 片段中**需用户替换的占位值**用 `{colors.warning}` 前景 + 下划线虚线标出，复制出的文本保留占位符原样。
- **示意角标 `illustrative-badge`**：官网任何非真实数据的数值/地图/曲线的右上角，琥珀色小标「示意」，不可由编辑者去除（见官网数据真实性规范）。
- **数据表格**：表头 `{colors.bg-sunken}`；行高 40px（紧凑 32px）；操作列固定在右；数值列右对齐 + 等宽数字；斑马纹关闭，悬停行高亮。
- **图表（ECharts）**：序列色取 `{colors.chart-1}`…`{colors.chart-8}`；网格线 `{colors.border-default}` 且低对比；每张图有标题、单位、时间范围与「更新于 hh:mm:ss」；提供「查看数据表」切换（可访问性）。
- **节点/拓扑示意图（官网、真实 IP 页指示图）**：线性图形 + 品牌渐变连线；指示图里「LingEdge → 源站」方向的箭头用品牌蓝实线，「访客 → LingEdge」用中性虚线，以视觉区分两个方向（对应基线 §9.1 的方向性）。

### Arco Design Pro 的使用与定制边界

| 允许（主题层） | 禁止 |
|---|---|
| 通过 CSS 变量/主题令牌覆盖主色板（`--primary-1…10`）、语义色、圆角、字体、阴影；亮暗两套用 Arco 的 `arco-theme` 属性切换 | Fork/打补丁修改 Arco 源码；用 `!important` 或深层选择器改组件内部结构 |
| 暗色主题下，对 Arco 写死 `#fff` 文字色的实心控件（主按钮/状态按钮、选中格、徽标、步骤图标）做**仅 `color`** 的窄覆盖，取 `{colors.on-brand-dark}`（对比度≥4.5:1）；选择器清单在 `tokens.json` 的 `arco`，由 `make frontend-check` 对照 Arco 真实 CSS 校验（Story 1.37b，提议，待 UD-14 确认） | 用该机制改 `color` 以外的属性，或覆盖清单之外的组件 |
| 在 `frontend/shared` 封装**业务组件**（统一表格、状态胶囊、发布条、代码块、危险确认对话框、权限按钮）并在 Admin/Console 间复用 | 页面内直接拼装「一次性」自定义版本的同类组件 |
| 使用 Arco Pro 的布局外壳（侧栏/顶栏/多标签）并替换品牌与菜单数据源 | 引入第二套 UI 组件库 |
| 官网使用 Tailwind，**读取同一份令牌**（`tokens.css` → Tailwind theme extend），不使用 Arco | 官网引入 Arco 或 Admin 引入 Tailwind 原子类做主结构（可用于局部布局辅助，不得覆盖语义色） |

令牌单一来源：`frontend/shared/tokens/`（CSS 变量 `--le-*`），由构建同时生成 Arco 主题变量映射与 Tailwind theme。**色值只允许在此处出现**，组件与页面里出现十六进制色值视为缺陷（lint 检查）。

## Do's and Don'ts

**Do**
- 把渐变留给品牌时刻；数据区用语义色。
- 状态 = 图标 + 文字 + 颜色。
- 配置/Header/命令一律等宽字体并提供复制。
- 暗色主题用明度分层；所有语义色成对给出暗色值。
- 示意内容必须有 `illustrative-badge`。

**Don't**
- 不在后台表格、表单、图表上使用渐变。
- 不用纯颜色区分状态或图表系列。
- 不把 `text-disabled` 用于必读信息。
- 不在页面里写死十六进制色值或 px 字号。
- 不用霓虹/发光/3D 地球冒充「全球网络规模」。
