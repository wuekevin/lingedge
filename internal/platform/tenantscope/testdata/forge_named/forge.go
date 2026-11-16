// Package forge_named 尝试用具名字段的复合字面量伪造 Scope：必须编译失败。
package forge_named

import "github.com/wuekevin/lingedge/internal/platform/tenantscope"

var Forged = tenantscope.Scope{tenantID: 1}
