// Package token 只能被 internal/platform/db 及其子包导入（Go internal 规则），用来证明「调用方是受信任的 SQL 构建器」。
//
// 零值 Token 无效：包外代码即便借助泛型推断拿到该类型的零值，db.FromBuilder 也会因 Valid()==false 而 panic。
package token

// Token 是构建器凭据。
type Token struct{ ok bool }

// New 签发有效凭据。
func New() Token { return Token{ok: true} }

// Valid 报告凭据是否由 New 签发。
func (t Token) Valid() bool { return t.ok }
