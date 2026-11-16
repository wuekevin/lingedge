// Package bad_pool_mixup：把 maint 池传给需要 app 池的位置必须编译失败（三类账号类型互不兼容，AC6）。
package bad_pool_mixup

import "github.com/wuekevin/lingedge/internal/platform/db"

type siteRepo struct{ pool *db.AppPool }

func NewSiteRepo(p *db.AppPool) siteRepo { return siteRepo{p} }

func Bad(m *db.MaintPool) siteRepo { return NewSiteRepo(m) }
