package handler

import (
	"context"
	"errors"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/tenant/internal/repository"
	"github.com/wuekevin/lingedge/internal/tenant/internal/service"
)

// Options 是 NewModule 的参数（根包 tenant.Options 是它的别名）。
type Options struct {
	// Pool 必须是 app 连接池。
	Pool  db.Pool
	Clock clock.Clock
	Log   *zap.Logger
	// ManifestOnly 仅用于导出路由清单（lingedge-server routes）：不需要数据库，处理函数不可被调用。
	ManifestOnly bool
}

// Module 是租户模块。
type Module struct {
	svc      *service.Service
	handlers *Handlers
}

// NewModule 创建租户模块。
func NewModule(_ context.Context, o Options) (*Module, error) {
	if o.ManifestOnly {
		return &Module{handlers: New(nil)}, nil
	}
	repo, err := repository.New(o.Pool)
	if err != nil {
		return nil, err
	}
	svc, err := service.New(service.Options{Repo: repo, WithTx: o.Pool.WithTx, Clock: o.Clock})
	if err != nil {
		return nil, err
	}
	return &Module{svc: svc, handlers: New(svc)}, nil
}

// Register 注册全部 /tenants* 路由。
func (m *Module) Register(r *httpx.Router) error {
	if r == nil {
		return errors.New("tenant: Router 为空")
	}
	return m.handlers.Register(r)
}

// Resolver 返回租户目录；ManifestOnly 时为 nil（acting-as 因此保持安全拒绝）。
func (m *Module) Resolver() httpx.TenantResolver {
	if m.svc == nil {
		return nil
	}
	return m.svc
}

// AuditedOperation 报告 operationID 是否是本模块要求审计的写 operation。
func (m *Module) AuditedOperation(operationID string) bool {
	for _, meta := range Metas() {
		if meta.OperationID == operationID {
			return meta.Audit
		}
	}
	return false
}
