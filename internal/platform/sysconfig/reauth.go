package sysconfig

import (
	"context"
	"net/http"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
)

// 安全敏感设置键的分级再认证（Story 1.22b；键清单与理由见 registry.go 的 reauthRequiredKeys）。
//
// 为什么不用 OpenAPI 的 x-reauth：x-reauth 是 operation 级的，且其集合由 CI 固定（scripts/contract/data/x-reauth.txt）；patchSettings 同时承载
// 「改品牌」与「关掉强制 TOTP」这类强度不同的变更，把整个 operation 标成 x-reauth 会让改品牌也要再认证并改动冻结契约。因此在设置服务内按键分级：
// 变更触及任一 ReauthRequired 键时，在写事务之前对当前主体运行与 x-reauth 路由相同的检查（httpx.ReauthChecker：会话的 reauth_at 在
// auth.reauth_window_s 内；API Token 没有会话，永远不满足 → 令牌不能写这些键），不满足返回 403 auth.reauth_required。
// 顺序：先 RBAC（授权钩子：setting:write）→ 再校验取值 → 再本检查 → 才写入；失败不产生任何写入与审计。

type reauthHolder struct{ c httpx.ReauthChecker }

// SetReauthChecker 设置再认证检查组件（见 Module.SetReauthChecker）。
func (s *Service) SetReauthChecker(c httpx.ReauthChecker) {
	if c == nil {
		s.reauth.Store(nil)
		return
	}
	s.reauth.Store(&reauthHolder{c: c})
}

// patchSettingsMeta 是通用 PATCH 的路由元数据副本，仅用来驱动 ReauthChecker（它只看 meta.Reauth 与主体）。
var patchSettingsMeta = httpx.RouteMeta{OperationID: "patchSettings", Method: http.MethodPatch, Path: "/admin/settings", Reauth: true}

// requireReauthFor 在变更触及安全敏感键时要求近期再认证。
func (s *Service) requireReauthFor(ctx context.Context, changes []change) error {
	need := false
	for _, c := range changes {
		if c.def.ReauthRequired {
			need = true
			break
		}
	}
	if !need {
		return nil
	}
	h := s.reauth.Load()
	p, ok := httpx.PrincipalFrom(ctx)
	if h == nil || !ok {
		return apierr.New(apierr.AuthForbidden, "forbidden") // 未装配检查组件或没有主体：fail-closed
	}
	r, err := http.NewRequestWithContext(ctx, patchSettingsMeta.Method, patchSettingsMeta.Path, nil)
	if err != nil {
		return apierr.Internal(err)
	}
	return h.c.CheckReauth(r, patchSettingsMeta, p)
}
