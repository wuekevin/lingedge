// Package scope 提供 Repository 签名中使用的 Scope 类型名（Story 1.5 AC2：`scope.Scope`）。
//
// 类型本体与全部构造入口在 internal/platform/tenantscope：只有同包才能让 Scope 的字段对包外保持不可写，
// 因此这里只是类型别名（别名不改变字段的可见性），不提供任何构造能力。
package scope

import "github.com/wuekevin/lingedge/internal/platform/tenantscope"

// Scope 即 tenantscope.Scope。
type Scope = tenantscope.Scope
