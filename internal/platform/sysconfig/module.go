package sysconfig

import (
	"context"
	"errors"

	"github.com/wuekevin/lingedge/internal/platform/httpx"
)

// Module 是系统设置模块：路由 + 功能开关 + /auth/me 的 features 提供者。生产装配（cmd/lingedge-server）与 tests/apistack 共用 New。
type Module struct{ svc *Service }

// New 创建模块。ManifestOnly 时不需要任何依赖（处理函数不可被调用）。
func New(o Options) (*Module, error) {
	s, err := newService(o)
	if err != nil {
		return nil, err
	}
	if o.ManifestOnly {
		return &Module{}, nil
	}
	return &Module{svc: s}, nil
}

// Service 返回类型化设置服务（ManifestOnly 时为 nil）。
func (m *Module) Service() *Service { return m.svc }

// Features 实现 auth 模块使用的能力提供者：运行模式 + 已交付模块 + 能力状态。
func (m *Module) Features(ctx context.Context) (FeaturesView, error) {
	if m.svc == nil {
		return FeaturesView{}, errors.New("sysconfig: 模块未装配")
	}
	return m.svc.Features(ctx)
}

// Register 注册本模块的全部路由。
func (m *Module) Register(r *httpx.Router) error {
	if r == nil {
		return errors.New("sysconfig: Router 为空")
	}
	return registerRoutes(r, m.svc)
}
