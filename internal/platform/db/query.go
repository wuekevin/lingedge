package db

import "github.com/wuekevin/lingedge/internal/platform/db/internal/token"

// sqlConst 是只接受「编译期常量」的字符串类型：未导出，因此包外调用方只能传字符串字面量/无类型常量
// （及其常量拼接），任何变量、fmt.Sprintf 结果、运行期拼接都无法通过编译（NFR-9：SQL 一律参数化、禁止拼接）。
type sqlConst string

// Query 是可被执行的 SQL 文本。只有两个来源：
//   - Q：调用方写下的编译期常量（值一律用 ? 占位符传参）；
//   - FromBuilder：tenantdb 构建器在校验过标识符白名单后生成的文本。
type Query struct{ text string }

// Q 把 SQL 常量包装为 Query。参数类型为未导出的 sqlConst，因此运行期构造的字符串会编译失败：
//
//	db.Q("SELECT id FROM sites WHERE tenant_id = ? AND uid = ?") // 合法
//	db.Q("SELECT id FROM sites WHERE uid = '" + uid + "'")        // 编译失败（uid 不是常量）
func Q(s sqlConst) Query { return Query{text: string(s)} }

// FromBuilder 由受信任的 SQL 构建器（internal/platform/db/tenantdb）使用。凭据类型来自 db 的 internal 包，
// 包外代码无法合法取得；零值凭据会 panic。
func FromBuilder(t token.Token, text string) Query {
	if !t.Valid() {
		panic("db: FromBuilder 只能由受信任的 SQL 构建器调用")
	}
	return Query{text: text}
}

// String 返回 SQL 文本（不含任何参数值，可安全写入日志）。
func (q Query) String() string { return q.text }
