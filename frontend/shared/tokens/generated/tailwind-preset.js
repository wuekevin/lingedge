/* 自动生成，请勿手改。来源：frontend/shared/tokens/tokens.json；生成器：frontend/scripts/gen-tokens.mjs（make frontend-check 校验无漂移）。 */
// Tailwind 3.x preset（官网 Nuxt/Tailwind 使用；Admin/Console 不使用 Tailwind 做主结构，DESIGN.md）。
// 颜色全部是 CSS 变量引用，亮/暗由 data-theme 切换，不在组件内写分支。
export default {
  "theme": {
    "extend": {
      "colors": {
        "le-brand-primary": "var(--le-color-brand-primary)",
        "le-brand-primary-hover": "var(--le-color-brand-primary-hover)",
        "le-brand-violet": "var(--le-color-brand-violet)",
        "le-on-brand": "var(--le-color-on-brand)",
        "le-bg-page": "var(--le-color-bg-page)",
        "le-bg-surface": "var(--le-color-bg-surface)",
        "le-bg-surface-raised": "var(--le-color-bg-surface-raised)",
        "le-bg-sunken": "var(--le-color-bg-sunken)",
        "le-border-default": "var(--le-color-border-default)",
        "le-border-strong": "var(--le-color-border-strong)",
        "le-text-primary": "var(--le-color-text-primary)",
        "le-text-secondary": "var(--le-color-text-secondary)",
        "le-text-disabled": "var(--le-color-text-disabled)",
        "le-success": "var(--le-color-success)",
        "le-success-bg": "var(--le-color-success-bg)",
        "le-warning": "var(--le-color-warning)",
        "le-warning-bg": "var(--le-color-warning-bg)",
        "le-danger": "var(--le-color-danger)",
        "le-danger-bg": "var(--le-color-danger-bg)",
        "le-info": "var(--le-color-info)",
        "le-info-bg": "var(--le-color-info-bg)",
        "le-chart-1": "var(--le-color-chart-1)",
        "le-chart-2": "var(--le-color-chart-2)",
        "le-chart-3": "var(--le-color-chart-3)",
        "le-chart-4": "var(--le-color-chart-4)",
        "le-chart-5": "var(--le-color-chart-5)",
        "le-chart-6": "var(--le-color-chart-6)",
        "le-chart-7": "var(--le-color-chart-7)",
        "le-chart-8": "var(--le-color-chart-8)"
      },
      "fontFamily": {
        "le-sans": [
          "var(--le-font-sans)"
        ],
        "le-mono": [
          "var(--le-font-mono)"
        ]
      },
      "fontSize": {
        "le-display": [
          "var(--le-text-display-size)",
          {
            "lineHeight": "var(--le-text-display-line-height)",
            "fontWeight": "var(--le-text-display-weight)"
          }
        ],
        "le-heading-1": [
          "var(--le-text-heading-1-size)",
          {
            "lineHeight": "var(--le-text-heading-1-line-height)",
            "fontWeight": "var(--le-text-heading-1-weight)"
          }
        ],
        "le-heading-2": [
          "var(--le-text-heading-2-size)",
          {
            "lineHeight": "var(--le-text-heading-2-line-height)",
            "fontWeight": "var(--le-text-heading-2-weight)"
          }
        ],
        "le-heading-3": [
          "var(--le-text-heading-3-size)",
          {
            "lineHeight": "var(--le-text-heading-3-line-height)",
            "fontWeight": "var(--le-text-heading-3-weight)"
          }
        ],
        "le-page-title": [
          "var(--le-text-page-title-size)",
          {
            "lineHeight": "var(--le-text-page-title-line-height)",
            "fontWeight": "var(--le-text-page-title-weight)"
          }
        ],
        "le-body": [
          "var(--le-text-body-size)",
          {
            "lineHeight": "var(--le-text-body-line-height)",
            "fontWeight": "var(--le-text-body-weight)"
          }
        ],
        "le-body-lg": [
          "var(--le-text-body-lg-size)",
          {
            "lineHeight": "var(--le-text-body-lg-line-height)",
            "fontWeight": "var(--le-text-body-lg-weight)"
          }
        ],
        "le-caption": [
          "var(--le-text-caption-size)",
          {
            "lineHeight": "var(--le-text-caption-line-height)",
            "fontWeight": "var(--le-text-caption-weight)"
          }
        ],
        "le-label": [
          "var(--le-text-label-size)",
          {
            "lineHeight": "var(--le-text-label-line-height)",
            "fontWeight": "var(--le-text-label-weight)"
          }
        ],
        "le-metric": [
          "var(--le-text-metric-size)",
          {
            "lineHeight": "var(--le-text-metric-line-height)",
            "fontWeight": "var(--le-text-metric-weight)"
          }
        ],
        "le-mono": [
          "var(--le-text-mono-size)",
          {
            "lineHeight": "var(--le-text-mono-line-height)",
            "fontWeight": "var(--le-text-mono-weight)"
          }
        ]
      },
      "borderRadius": {
        "le-sm": "var(--le-radius-sm)",
        "le-md": "var(--le-radius-md)",
        "le-lg": "var(--le-radius-lg)",
        "le-xl": "var(--le-radius-xl)",
        "le-full": "var(--le-radius-full)"
      },
      "spacing": {
        "le-1": "var(--le-space-1)",
        "le-2": "var(--le-space-2)",
        "le-3": "var(--le-space-3)",
        "le-4": "var(--le-space-4)",
        "le-5": "var(--le-space-5)",
        "le-6": "var(--le-space-6)",
        "le-8": "var(--le-space-8)",
        "le-12": "var(--le-space-12)",
        "le-16": "var(--le-space-16)"
      },
      "boxShadow": {
        "le-overlay": "var(--le-shadow-overlay)"
      },
      "backgroundImage": {
        "le-brand": "var(--le-gradient-brand)"
      },
      "screens": {
        "sm": "576px",
        "md": "768px",
        "lg": "992px",
        "xl": "1200px",
        "xxl": "1600px"
      },
      "maxWidth": {
        "le-admin": "var(--le-layout-content-max-admin)",
        "le-website": "var(--le-layout-content-max-website)"
      }
    }
  }
};
