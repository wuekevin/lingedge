// Package bad_query_literal：用复合字面量直接构造 db.Query 必须编译失败。
package bad_query_literal

import "github.com/wuekevin/lingedge/internal/platform/db"

var Bad = db.Query{"DROP TABLE sites"}
