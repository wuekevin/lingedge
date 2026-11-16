// Package bad_import_token：包外代码不得取得 SQL 构建器凭据（internal 包不可导入）。
package bad_import_token

import (
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/db/internal/token"
)

func Bad(s string) db.Query { return db.FromBuilder(token.New(), s) }
