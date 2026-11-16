// Package ok 是「正向对照」：包外代码合法地使用 Scope（零值、读取访问器、经 FromPrincipal 构造）。
package ok

import (
	"github.com/wuekevin/lingedge/internal/platform/scope"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
)

type principal struct{}

func (principal) ActorID() uint64           { return 1 }
func (principal) IsPlatform() bool          { return false }
func (principal) TenantID() uint64          { return 7 }
func (principal) HasPermission(string) bool { return false }

// Make 经唯一入口构造 Scope。
func Make() (scope.Scope, error) {
	return tenantscope.FromPrincipal(principal{}, tenantscope.RouteTenant, tenantscope.NoTarget())
}

// Zero 零值 Scope 在包外是合法的（表示「没有 Scope」，会被构建器拒绝）。
func Zero() (scope.Scope, uint64) {
	var s scope.Scope
	return s, s.TenantID()
}
