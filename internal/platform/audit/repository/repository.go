// Package repository 是 audit_logs 的数据访问（Story 1.21）：**只有 INSERT 与 SELECT**——lingedge_app 账号对该表只有 SELECT/INSERT（迁移 000003），
// 表上还有 BEFORE UPDATE/DELETE 触发器；本包不提供任何 UPDATE/DELETE，也不接受 maint 池（分区维护走 schedrt/repository 的 maint 池）。
//
// Scope 规则（ADR-009 §1、make repo-check）：全部方法首参为 scope.Scope。读取方法据 Scope 限定可见范围：租户范围 Scope（含 acting-as 的目标租户）
// 强制追加 `tenant_id = Scope 的租户`、调用方无法省略；平台范围 Scope 才可跨租户（可再按 tenant_uid 过滤）。写入方法的 tenant_id 是「被操作对象所属租户」，
// 由审计记录器按事件明确给出（代租户操作时与操作者所属租户不同，平台级对象为 NULL），不受调用 Scope 的租户限定。
package repository

import (
	"context"
	"errors"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/scope"
)

// ErrWrongRole：只接受 app 连接池。
var ErrWrongRole = errors.New("audit/repository: 只接受 app 连接池")

// ErrNoScope：零值 Scope 一律拒绝（不会退化为无租户条件的查询）。
var ErrNoScope = errors.New("audit/repository: 缺少 Scope")

// Repo 是审计 Repository。
type Repo struct{ pool db.Pool }

// New 创建 Repository。
func New(pool db.Pool) (*Repo, error) {
	if pool == nil || pool.Role() != db.RoleApp {
		return nil, ErrWrongRole
	}
	return &Repo{pool: pool}, nil
}

// Row 是要写入的一行审计记录（字段与 audit_logs 一一对应；JSON 列传已序列化的文本，nil 表示 NULL）。
type Row struct {
	UID           string
	CreatedAt     time.Time
	ActorType     string
	ActorUserID   *uint64
	ActorTokenID  *uint64
	ActorLabel    string
	ActorTenantID *uint64
	TenantID      *uint64
	ActingAs      bool
	Action        string
	ResourceType  string
	ResourceUID   *string
	Result        string
	ChangesJSON   *string
	MetaJSON      *string
	RequestID     string
	IP            []byte
	UserAgent     *string
}

// Insert 写入一行审计记录。必须在事务内调用（由审计记录器保证：与业务变更同一事务，写入失败即业务整体回滚）。
func (r *Repo) Insert(ctx context.Context, sc scope.Scope, row Row) error {
	_ = sc // 写入不按调用 Scope 限定租户：tenant_id 是被操作对象所属租户，见包注释
	var ip any
	if len(row.IP) > 0 {
		ip = row.IP
	}
	_, err := r.pool.Executor(ctx).Exec(ctx, db.Q(`INSERT INTO audit_logs (uid, created_at, actor_type, actor_user_id, actor_token_id, actor_label, actor_tenant_id,
		tenant_id, acting_as, action, resource_type, resource_uid, result, changes_json, meta_json, request_id, ip, user_agent)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		row.UID, row.CreatedAt, row.ActorType, row.ActorUserID, row.ActorTokenID, row.ActorLabel, row.ActorTenantID,
		row.TenantID, row.ActingAs, row.Action, row.ResourceType, row.ResourceUID, row.Result, row.ChangesJSON, row.MetaJSON, row.RequestID, ip, row.UserAgent)
	if err != nil {
		return err
	}
	return nil
}

// LogRow 是读取到的审计记录（含联结出的操作者 uid 与租户 uid）。
type LogRow struct {
	ID           uint64    `db:"id"`
	UID          string    `db:"uid"`
	CreatedAt    time.Time `db:"created_at"`
	ActorType    string    `db:"actor_type"`
	ActorUID     *string   `db:"actor_uid"`
	ActorLabel   string    `db:"actor_label"`
	TenantUID    *string   `db:"tenant_uid"`
	ActingAs     bool      `db:"acting_as"`
	Action       string    `db:"action"`
	ResourceType string    `db:"resource_type"`
	ResourceUID  *string   `db:"resource_uid"`
	Result       string    `db:"result"`
	ChangesJSON  []byte    `db:"changes_json"`
	RequestID    string    `db:"request_id"`
	IP           []byte    `db:"ip"`
	UserAgent    *string   `db:"user_agent"`
}

// Filter 是列表过滤条件（空串/nil = 不过滤）。
type Filter struct {
	Action       string
	ResourceType string
	ResourceUID  string
	ActorUID     string
	TenantUID    string
	Result       string
	RequestID    string
	From, To     *time.Time // created_at ∈ [From, To]
	BeforeID     uint64     // 游标：只取 id 更小的
	Limit        int
}

const selectCols = `a.id, a.uid, a.created_at, a.actor_type, u.uid AS actor_uid, a.actor_label, t.uid AS tenant_uid, a.acting_as, a.action, a.resource_type,
	a.resource_uid, a.result, a.changes_json, a.request_id, a.ip, a.user_agent`
const fromJoin = ` FROM audit_logs a LEFT JOIN users u ON u.id = a.actor_user_id LEFT JOIN tenants t ON t.id = a.tenant_id`

// 可见范围：平台范围（visPlatform=1）不限租户；否则只取 tenant_id = 租户范围 Scope 的租户。
const visible = `(? = 1 OR a.tenant_id = ?)`

func visibility(sc scope.Scope) (platform int, tenantID uint64, err error) {
	switch {
	case sc.IsZero():
		return 0, 0, ErrNoScope
	case sc.IsPlatform():
		return 1, 0, nil
	case sc.TenantID() == 0:
		return 0, 0, ErrNoScope
	}
	return 0, sc.TenantID(), nil
}

// List 按 id 倒序列出审计记录（游标分页）：租户范围 Scope 只返回其租户的记录；平台范围可跨租户。返回至多 f.Limit 行（调用方传 limit+1 判断是否有下一页）。
func (r *Repo) List(ctx context.Context, sc scope.Scope, f Filter) ([]LogRow, error) {
	plat, tid, err := visibility(sc)
	if err != nil {
		return nil, err
	}
	var rows []LogRow
	err = r.pool.Executor(ctx).Select(ctx, &rows, db.Q(`SELECT `+selectCols+fromJoin+` WHERE `+visible+`
		AND (? = '' OR a.action = ?) AND (? = '' OR a.resource_type = ?) AND (? = '' OR a.resource_uid = ?)
		AND (? = '' OR u.uid = ?) AND (? = '' OR t.uid = ?) AND (? = '' OR a.result = ?) AND (? = '' OR a.request_id = ?)
		AND (? IS NULL OR a.created_at >= ?) AND (? IS NULL OR a.created_at <= ?) AND (? = 0 OR a.id < ?)
		ORDER BY a.id DESC LIMIT ?`),
		plat, tid,
		f.Action, f.Action, f.ResourceType, f.ResourceType, f.ResourceUID, f.ResourceUID,
		f.ActorUID, f.ActorUID, f.TenantUID, f.TenantUID, f.Result, f.Result, f.RequestID, f.RequestID,
		f.From, f.From, f.To, f.To, f.BeforeID, f.BeforeID, f.Limit)
	return rows, err
}

// Get 按 uid 取一条审计记录；不存在或不在 Scope 可见范围内一律返回 db.ErrNotFound（不泄漏存在性）。
func (r *Repo) Get(ctx context.Context, sc scope.Scope, uid string) (LogRow, error) {
	plat, tid, err := visibility(sc)
	if err != nil {
		return LogRow{}, err
	}
	var row LogRow
	err = r.pool.Executor(ctx).Get(ctx, &row, db.Q(`SELECT `+selectCols+fromJoin+` WHERE a.uid = ? AND `+visible+` LIMIT 1`), uid, plat, tid)
	return row, err
}
