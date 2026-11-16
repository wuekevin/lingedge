// Package forge_alias 经 scope.Scope 别名伪造：别名不改变字段可见性，必须编译失败。
package forge_alias

import "github.com/wuekevin/lingedge/internal/platform/scope"

var Forged = scope.Scope{tenantID: 1, platform: true}
