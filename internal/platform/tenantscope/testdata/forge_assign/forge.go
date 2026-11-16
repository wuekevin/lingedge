// Package forge_assign 尝试对零值 Scope 的字段赋值：必须编译失败。
package forge_assign

import "github.com/wuekevin/lingedge/internal/platform/tenantscope"

func Forge() tenantscope.Scope {
	var s tenantscope.Scope
	s.tenantID = 1
	return s
}
