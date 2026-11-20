// Package lineage 实现部署谱系（`system_settings.deploy.lineage`）的读取与提升（Story 1.13 AC2，ADR-002 §12、tools-design §17.5）。
//
// 背景：节点按 (lineage, seq) 字典序「严格大于」接受修订。数据库恢复到较早备份后，clusters.current_seq、cache_epoch 等会低于节点
// 已应用的值；恢复之后、Server 启动之前执行 `lingedge-server lineage bump`，把 lineage 提升为 max(当前值+1, 当前 UNIX 秒)，
// 使恢复后产生的新修订在节点侧仍然「更大」。取 UNIX 秒作下界，保证「多次恢复 / 恢复到更早备份」仍单调、不重用。
//
// 约束：
//   - 单事务：`SELECT … FOR UPDATE` 锁住设置行 → 计算 → UPDATE → 写审计（audit_logs，同事务；任一失败整体回滚）。并发 bump 串行，
//     结果严格单调递增；
//   - 设置项不存在（安装尚未完成，deploy.lineage 由 Story 1.23 的初始化写入）一律拒绝，不隐式创建；
//   - 只用 app 连接池（lingedge_app 对 system_settings 有 SELECT/UPDATE、对 audit_logs 有 INSERT）；不需要也不接受更高权限账号。
package lineage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/requestid"
)

// SettingKey 是 system_settings 里的设置键（跨文档契约，database/platform-identity.md §1）。
const SettingKey = "deploy.lineage"

// 错误。
var (
	// ErrNotInstalled：设置项不存在（安装未完成）。
	ErrNotInstalled = errors.New("lineage: system_settings 中没有 deploy.lineage（安装尚未完成：该值由安装向导的「内部身份与密钥初始化」步骤写入，初值 1）；拒绝提升")
	// ErrWrongRole：只接受 app 连接池。
	ErrWrongRole = errors.New("lineage: 只接受 app 连接池")
	// ErrInvalidValue：库内的值不是正整数。
	ErrInvalidValue = errors.New("lineage: system_settings.deploy.lineage 不是正整数，拒绝提升（请人工核对）")
)

// Service 读取与提升 lineage。
type Service struct {
	pool db.Pool
	clk  clock.Clock
	ids  requestid.Generator
}

// New 创建 Service。ids 用于审计记录的 uid 与 request_id（CLI 没有入站请求，每次 bump 生成新的）。
func New(pool db.Pool, clk clock.Clock, ids requestid.Generator) (*Service, error) {
	if pool == nil || pool.Role() != db.RoleApp {
		return nil, ErrWrongRole
	}
	if clk == nil {
		clk = clock.System()
	}
	if ids == nil {
		return nil, errors.New("lineage: 缺少 ID 生成器")
	}
	return &Service{pool: pool, clk: clk, ids: ids}, nil
}

type settingRow struct {
	ID    uint64 `db:"id"`
	Value []byte `db:"value_json"`
}

func parseValue(raw []byte) (int64, error) {
	if len(raw) == 0 {
		return 0, ErrInvalidValue
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var n json.Number
	if err := dec.Decode(&n); err != nil {
		return 0, ErrInvalidValue
	}
	v, err := strconv.ParseInt(n.String(), 10, 64)
	if err != nil || v < 1 {
		return 0, ErrInvalidValue
	}
	return v, nil
}

// Show 返回当前 lineage。设置项不存在返回 ErrNotInstalled。
func (s *Service) Show(ctx context.Context) (int64, error) {
	var r settingRow
	err := s.pool.Executor(ctx).Get(ctx, &r, db.Q("SELECT id, value_json FROM system_settings WHERE setting_key = ?"), SettingKey)
	if errors.Is(err, db.ErrNotFound) {
		return 0, ErrNotInstalled
	}
	if err != nil {
		return 0, fmt.Errorf("lineage: 读取: %w", err)
	}
	return parseValue(r.Value)
}

// BumpResult 是一次提升的结果。
type BumpResult struct {
	Before, After int64
	RequestID     string
}

// Bump 在单事务内把 lineage 提升为 max(当前值+1, 当前 UNIX 秒) 并写审计。重复调用严格单调递增。
func (s *Service) Bump(ctx context.Context) (BumpResult, error) {
	reqID, err := s.ids.New()
	if err != nil {
		return BumpResult{}, fmt.Errorf("lineage: 生成 request_id: %w", err)
	}
	auditUID, err := s.ids.New()
	if err != nil {
		return BumpResult{}, fmt.Errorf("lineage: 生成审计 uid: %w", err)
	}
	res := BumpResult{RequestID: reqID}
	err = s.pool.WithTx(ctx, func(ctx context.Context) error {
		ex := s.pool.Executor(ctx)
		var r settingRow
		if err := ex.Get(ctx, &r, db.Q("SELECT id, value_json FROM system_settings WHERE setting_key = ? FOR UPDATE"), SettingKey); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return ErrNotInstalled
			}
			return fmt.Errorf("lineage: 读取并锁定: %w", err)
		}
		cur, err := parseValue(r.Value)
		if err != nil {
			return err
		}
		now := s.clk.Now()
		next := cur + 1
		if u := now.Unix(); u > next {
			next = u
		}
		if _, err := ex.Exec(ctx, db.Q("UPDATE system_settings SET value_json = ? WHERE id = ?"), strconv.FormatInt(next, 10), r.ID); err != nil {
			return fmt.Errorf("lineage: 更新: %w", err)
		}
		changes, _ := json.Marshal(map[string]int64{"before": cur, "after": next})
		meta, _ := json.Marshal(map[string]string{"source": "cli", "command": "lineage bump"})
		if _, err := ex.Exec(ctx, db.Q("INSERT INTO audit_logs (uid, created_at, actor_type, actor_label, acting_as, action, resource_type, resource_uid, result, changes_json, meta_json, request_id) "+
			"VALUES (?, ?, 'system', 'cli:lingedge-server', 0, 'system.lineage.bump', 'system_setting', ?, 'success', ?, ?, ?)"),
			auditUID, now, SettingKey, string(changes), string(meta), reqID); err != nil {
			return fmt.Errorf("lineage: 写审计: %w", err)
		}
		res.Before, res.After = cur, next
		return nil
	})
	if err != nil {
		return BumpResult{}, err
	}
	return res, nil
}
