// Package secrets 把密文列仓储（secrets/repository）适配为 pkg/secretbox 的 RecordStore（Story 1.11 遗留项）。
// 本包不含 SQL：全部数据访问在 repository 子包，并受 repo-scope/平台范围规则约束。
package secrets

import (
	"context"
	"errors"

	"github.com/wuekevin/lingedge/internal/platform/scope"
	"github.com/wuekevin/lingedge/internal/platform/secrets/repository"
	"github.com/wuekevin/lingedge/pkg/secretbox"
)

// ErrNeedPlatformScope：重包装是跨租户批处理，须平台范围 Scope（tenantscope.PlatformForJob）。
var ErrNeedPlatformScope = repository.ErrNeedPlatformScope

type recordStore struct {
	repo *repository.Repo
	sc   scope.Scope
}

var _ secretbox.RecordStore = recordStore{}

// NewRecordStore 返回绑定了平台范围 Scope 的 secretbox.RecordStore（真实 MySQL 实现；经 repo.New(appPool) 构造 repo）。
// sc 不是平台范围（租户范围或零值）时返回 ErrNeedPlatformScope。
func NewRecordStore(repo *repository.Repo, sc scope.Scope) (secretbox.RecordStore, error) {
	if repo == nil {
		return nil, errors.New("secrets: repo 不能为空")
	}
	if !sc.IsPlatform() {
		return nil, ErrNeedPlatformScope
	}
	return recordStore{repo: repo, sc: sc}, nil
}

func (r recordStore) ListByKEK(ctx context.Context, kekID string, after secretbox.Record, limit int) ([]secretbox.Record, error) {
	return r.repo.ListByKEKAcrossTenants(ctx, r.sc, kekID, after, limit)
}

func (r recordStore) Replace(ctx context.Context, old secretbox.Record, newEnc, newKEK string) (bool, error) {
	return r.repo.ReplaceAcrossTenants(ctx, r.sc, old, newEnc, newKEK)
}

func (r recordStore) CountByKEK(ctx context.Context, kekID string) (int64, error) {
	return r.repo.CountByKEKAcrossTenants(ctx, r.sc, kekID)
}
