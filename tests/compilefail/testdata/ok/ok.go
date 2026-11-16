// Package ok 是正向对照：合法写法必须编译通过。
package ok

import "github.com/wuekevin/lingedge/internal/platform/db"

const base = "SELECT id FROM sites "

// Const 常量与常量拼接都是合法的 SQL 文本。
func Const() (db.Query, db.Query) {
	return db.Q("SELECT 1"), db.Q(base + "WHERE tenant_id = ? AND uid = ?")
}

func useApp(p *db.AppPool) db.Executor { return nil }

// Roles 各自的池只能用于各自的位置。
func Roles(a *db.AppPool) db.Executor { return useApp(a) }
