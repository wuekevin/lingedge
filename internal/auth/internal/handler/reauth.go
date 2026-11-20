package handler

import (
	"net/http"

	"github.com/wuekevin/lingedge/internal/auth/internal/domain"
	"github.com/wuekevin/lingedge/internal/auth/internal/service"
	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
)

// reauthChecker 实现 httpx.ReauthChecker（L4 再认证检查，ADR-018 §12）。
type reauthChecker struct{ svc *service.Service }

var _ httpx.ReauthChecker = reauthChecker{}

// CheckReauth 实现 httpx.ReauthChecker：不是本模块产出的主体（无法核对会话）一律视为未再认证（fail-closed）。
func (c reauthChecker) CheckReauth(r *http.Request, meta httpx.RouteMeta, p tenantscope.Principal) error {
	if !meta.Reauth {
		return nil
	}
	dp, ok := p.(*domain.Principal)
	if !ok || dp == nil {
		return apierr.New(apierr.AuthReauthRequired, "reauthentication required")
	}
	return c.svc.CheckReauth(r.Context(), dp)
}
