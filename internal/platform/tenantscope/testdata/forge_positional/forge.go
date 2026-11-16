// Package forge_positional 尝试用位置式复合字面量伪造 Scope：必须编译失败。
package forge_positional

import "github.com/wuekevin/lingedge/internal/platform/tenantscope"

var Forged = tenantscope.Scope{1, false, 2, false}
