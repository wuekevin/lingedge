// Package bad_sprintf：用格式化拼 SQL（NFR-9 禁止）必须编译失败。
package bad_sprintf

import (
	"fmt"

	"github.com/wuekevin/lingedge/internal/platform/db"
)

func Bad(uid string) db.Query {
	return db.Q(fmt.Sprintf("SELECT id FROM sites WHERE uid = '%s'", uid))
}
