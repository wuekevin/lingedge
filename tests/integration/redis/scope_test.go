package redisit

import (
	"github.com/wuekevin/lingedge/internal/platform/scope"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
)

func fromTask(tenantID uint64) (scope.Scope, error) { return tenantscope.FromTask(tenantID, 0) }
