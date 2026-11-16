// Package bad_concat：把变量拼进 SQL 必须编译失败。
package bad_concat

import "github.com/wuekevin/lingedge/internal/platform/db"

func Bad(uid string) db.Query {
	return db.Q("SELECT id FROM sites WHERE uid = '" + uid + "'")
}
