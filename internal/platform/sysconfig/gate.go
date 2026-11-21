package sysconfig

import "github.com/wuekevin/lingedge/internal/platform/httpx"

// Gate 返回功能开关钩子（httpx.Hooks.Features）：声明了 x-feature 的路由据运行模式放行或 403 feature.disabled。
// ManifestOnly 的 Module 没有设置服务，返回 nil（Hooks 的默认 DenyFeatures 生效）。
func (m *Module) Gate() httpx.FeatureGate {
	if m.svc == nil {
		return nil
	}
	return m.svc
}
