package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/schedrt/builtin"
	"github.com/wuekevin/lingedge/internal/platform/scope"
)

// ErrScope：这些作业只接受平台 Scope（Scheduler 是跨租户的平台进程）。
var ErrScope = errors.New("schedrt/repository: 需要平台 Scope")

func requirePlatform(sc scope.Scope) error {
	if sc.IsZero() || !sc.IsPlatform() {
		return ErrScope
	}
	return nil
}

// Partitions 实现 builtin.PartitionStore：只经 maint 连接池（lingedge_maint，分区表表级 SELECT/ALTER/DROP，README §5.1）。
type Partitions struct{ pool *db.MaintPool }

var _ builtin.PartitionStore = (*Partitions)(nil)

// NewPartitions 创建分区维护存储。
func NewPartitions(pool *db.MaintPool) (*Partitions, error) {
	if pool == nil {
		return nil, errors.New("schedrt/repository: 缺少 maint 连接池")
	}
	return &Partitions{pool: pool}, nil
}

// ListPartitioned 见 builtin.PartitionStore。information_schema 只返回账号有权限的表，因此只会列出 maint 被授权的分区表。
func (p *Partitions) ListPartitioned(ctx context.Context, sc scope.Scope) ([]builtin.PartitionedTable, error) {
	if err := requirePlatform(sc); err != nil {
		return nil, err
	}
	var rows []struct {
		Table  string  `db:"tbl"`
		Part   string  `db:"part"`
		Descr  *string `db:"descr"`
		Method *string `db:"method"`
	}
	if err := p.pool.Executor(ctx).Select(ctx, &rows, db.Q("SELECT TABLE_NAME AS tbl, PARTITION_NAME AS part, PARTITION_DESCRIPTION AS descr, PARTITION_METHOD AS method "+
		"FROM information_schema.PARTITIONS WHERE TABLE_SCHEMA = DATABASE() AND PARTITION_NAME IS NOT NULL ORDER BY TABLE_NAME, PARTITION_ORDINAL_POSITION")); err != nil {
		return nil, fmt.Errorf("schedrt/repository: 查询分区表: %w", err)
	}
	var out []builtin.PartitionedTable
	for _, r := range rows {
		if len(out) == 0 || out[len(out)-1].Name != r.Table {
			t := builtin.PartitionedTable{Name: r.Table}
			if r.Method != nil {
				t.Method = *r.Method
			}
			out = append(out, t)
		}
		part := builtin.Partition{Name: r.Part}
		if r.Descr != nil {
			part.Description = *r.Descr
		}
		out[len(out)-1].Partitions = append(out[len(out)-1].Partitions, part)
	}
	return out, nil
}

// ReorganizePmax 见 builtin.PartitionStore。
func (p *Partitions) ReorganizePmax(ctx context.Context, sc scope.Scope, table string, add []db.PartitionSpec) error {
	if err := requirePlatform(sc); err != nil {
		return err
	}
	return p.pool.ReorganizePmax(ctx, table, add)
}

// Sessions 实现 builtin.SessionStore（app 账号；sessions 表对 lingedge_app 有 DELETE 授权）。
type Sessions struct{ pool db.Pool }

var _ builtin.SessionStore = (*Sessions)(nil)

// NewSessions 创建会话清理存储。
func NewSessions(pool db.Pool) (*Sessions, error) {
	if pool == nil || pool.Role() != db.RoleApp {
		return nil, ErrWrongRole
	}
	return &Sessions{pool: pool}, nil
}

// DeleteEnded 见 builtin.SessionStore：会话的结束时刻 = min(expires_at, absolute_expires_at, revoked_at)；结束满 retention 才删除。
// 条件写成三个 OR 的比较（任一时刻早于截止点即结束得足够早），截止点由数据库 NOW(3) 计算。
func (s *Sessions) DeleteEnded(ctx context.Context, sc scope.Scope, retention time.Duration, limit int) (int64, error) {
	if err := requirePlatform(sc); err != nil {
		return 0, err
	}
	if retention <= 0 || limit <= 0 {
		return 0, errors.New("schedrt/repository: DeleteEnded 参数非法")
	}
	sec := int64(retention / time.Second)
	res, err := s.pool.Executor(ctx).Exec(ctx, db.Q("DELETE FROM sessions WHERE expires_at < NOW(3) - INTERVAL ? SECOND OR absolute_expires_at < NOW(3) - INTERVAL ? SECOND "+
		"OR revoked_at < NOW(3) - INTERVAL ? SECOND LIMIT ?"), sec, sec, sec, limit)
	if err != nil {
		return 0, fmt.Errorf("schedrt/repository: 清理会话: %w", err)
	}
	return res.RowsAffected()
}
