package migrationstest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"go.yaml.in/yaml/v3"

	"github.com/wuekevin/lingedge/internal/platform/migrate"
	"github.com/wuekevin/lingedge/migrations"
	"github.com/wuekevin/lingedge/tests/mysqltest"
)

// Story 1.7：M1 迁移 A（000001–000005，真实迁移，来自 migrations.FS）的测试支撑。
// 与 skeleton_test.go（骨架替身）不同，这里验证的是仓库里真正会发行的迁移文件。

// m1aTables 是 000001–000005 创建的 21 张表（epics Story 1.7 AC1）。
var m1aTables = []string{
	// 000001 identity_core（12）
	"users", "tenants", "user_recovery_codes", "tenant_members", "tenant_quotas", "permissions", "roles", "role_permissions",
	"user_roles", "sessions", "api_tokens", "login_logs",
	// 000002 platform_core（7）
	"system_settings", "system_installations", "tasks", "task_events", "job_locks", "idempotency_keys", "event_outbox",
	// 000003 audit_logs（1）、000005 keys_credentials（1）
	"audit_logs", "signing_keys",
}

// m1aFiles 是 000001–000005 的文件名（序号连续、名称与 database/README §8.1 一致）。
var m1aFiles = []string{
	"000001_identity_core.sql", "000002_platform_core.sql", "000003_audit_logs.sql", "000004_seed_builtin.sql", "000005_keys_credentials.sql",
}

// m1aUp 在新建空库上经框架执行全部真实迁移（必须 0 错误）。
func m1aUp(t testing.TB) (*env, *migrate.Runner) {
	t.Helper()
	e := newEnv(t)
	r := e.runner(t, migrations.FS)
	if f := r.Findings(); len(f) != 0 {
		t.Fatalf("真实迁移必须通过 lint: %v", f)
	}
	res, err := r.Up(ctxT(t), migrate.UpOptions{})
	if err != nil {
		t.Fatalf("空库全量 up 必须 0 错误: %v", err)
	}
	if len(res.Applied) != len(m1aFiles) || res.ToVersion != int64(len(m1aFiles)) {
		t.Fatalf("up 结果: %+v", res)
	}
	return e, r
}

// conn 返回以 root 连接且已 USE 本库的专用连接（测试结束关闭）。
func (e *env) conn(t testing.TB) *sql.Conn {
	t.Helper()
	c, err := e.admin(t).Conn(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.ExecContext(ctxT(t), "USE `"+e.dbName+"`"); err != nil {
		t.Fatal(err)
	}
	return c
}

// sx 是 *sql.Conn 与 *sql.Tx 共有的方法子集。
type sx interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

var seq atomic.Int64

// next 返回进程内唯一的序号（构造不冲突的邮箱/slug/uid）。
func next() int64 { return seq.Add(1) }

func uid26() string { return fmt.Sprintf("T%025d", next()) }

// cx 是一次「事务内」约束测试的上下文：用例结束回滚，彼此隔离。
type cx struct {
	t  *testing.T
	x  sx
	pw string
}

// inTx 在 root 连接上开事务运行 fn，结束一律回滚。
func inTx(t *testing.T, e *env, c *sql.Conn, fn func(c *cx)) {
	t.Helper()
	tx, err := c.BeginTx(ctxT(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	fn(&cx{t: t, x: tx, pw: mysqltest.RandomSecret(t)})
}

func (c *cx) exec(q string, args ...any) (sql.Result, error) {
	c.t.Helper()
	return c.x.ExecContext(ctxT(c.t), q, args...)
}

// ok 执行必须成功的语句，返回 LAST_INSERT_ID。
func (c *cx) ok(q string, args ...any) int64 {
	c.t.Helper()
	res, err := c.exec(q, args...)
	if err != nil {
		c.t.Fatalf("语句应成功: %v\n%s", err, q)
	}
	id, _ := res.LastInsertId()
	return id
}

// bad 执行必须以指定 MySQL 错误号失败的语句。
func (c *cx) bad(want uint16, q string, args ...any) {
	c.t.Helper()
	_, err := c.exec(q, args...)
	if err == nil {
		c.t.Fatalf("语句应以 ERROR %d 失败，实际成功:\n%s", want, q)
	}
	if n, ok := mysqlErr(err); !ok || n != want {
		c.t.Fatalf("期望 ERROR %d，实际: %v\n%s", want, err, q)
	}
}

func (c *cx) count(q string, args ...any) int64 {
	c.t.Helper()
	var n int64
	if err := c.x.QueryRowContext(ctxT(c.t), q, args...).Scan(&n); err != nil {
		c.t.Fatalf("%v\n%s", err, q)
	}
	return n
}

func (c *cx) isNull(q string, args ...any) bool {
	c.t.Helper()
	var v sql.NullInt64
	if err := c.x.QueryRowContext(ctxT(c.t), q, args...).Scan(&v); err != nil {
		c.t.Fatalf("%v\n%s", err, q)
	}
	return !v.Valid
}

// 常见 MySQL 错误号。
const (
	errDup           = 1062
	errNoDefault     = 1364
	errRowIsRef      = 1451 // 父行被引用，RESTRICT 拒绝删除
	errNoRefRow      = 1452 // 引用的父行不存在
	errCheckViolated = 3819
	errSignal        = 1644 // 触发器 SIGNAL SQLSTATE '45000'
	errOutOfRange    = 1526 // 分区表无 pmax 时越界写入
)

// ---- 测试数据构造 ----

func (c *cx) user(email string) int64 {
	c.t.Helper()
	return c.ok("INSERT INTO users (uid, user_type, email, display_name, password_hash) VALUES (?, 'tenant', ?, 'u', ?)", uid26(), email, c.pw)
}

func (c *cx) platformUser() int64 {
	c.t.Helper()
	return c.ok("INSERT INTO users (uid, user_type, email, display_name, password_hash) VALUES (?, 'platform', ?, 'p', ?)", uid26(), fmt.Sprintf("p%d@example.test", next()), c.pw)
}

func (c *cx) tenant() int64 {
	c.t.Helper()
	return c.ok("INSERT INTO tenants (uid, name, slug) VALUES (?, 't', ?)", uid26(), fmt.Sprintf("t-%d", next()))
}

func (c *cx) role(scope string, tenantID any, code string) int64 {
	c.t.Helper()
	return c.ok("INSERT INTO roles (uid, tenant_id, scope, code, name) VALUES (?, ?, ?, ?, 'r')", uid26(), tenantID, scope, code)
}

func (c *cx) task(typ string) int64 {
	c.t.Helper()
	return c.ok("INSERT INTO tasks (uid, type, payload_json, request_id) VALUES (?, ?, JSON_OBJECT(), ?)", uid26(), typ, uid26())
}

// ---- OpenAPI（权限字典与 TaskType 的事实源）----

type oaPerm struct {
	scopes map[string]bool // 使用该权限的 operation 的 x-tenant-scope 集合
	phases map[string]bool
}

type openapiFacts struct {
	perms    map[string]*oaPerm
	taskType []string
}

var (
	oaOnce  atomic.Pointer[openapiFacts]
	quoteRe = regexp.MustCompile(`\\?'([^'\\]+)\\?'`) // information_schema 里的 CHECK 子句把引号转义为 \\'
)

// loadOpenAPI 解析 docs/api/openapi.yaml：全部 operation 的 x-permissions / x-tenant-scope，以及 TaskType 枚举。
func loadOpenAPI(t testing.TB) *openapiFacts {
	t.Helper()
	if f := oaOnce.Load(); f != nil {
		return f
	}
	raw, err := os.ReadFile("../../docs/api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths      map[string]map[string]any `yaml:"paths"`
		Components struct {
			Schemas map[string]struct {
				Enum []string `yaml:"enum"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	f := &openapiFacts{perms: map[string]*oaPerm{}, taskType: doc.Components.Schemas["TaskType"].Enum}
	for _, item := range doc.Paths {
		for _, v := range item {
			op, ok := v.(map[string]any)
			if !ok {
				continue
			}
			ps, _ := op["x-permissions"].([]any)
			scope, _ := op["x-tenant-scope"].(string)
			phase, _ := op["x-phase"].(string)
			for _, p := range ps {
				code, _ := p.(string)
				if code == "" {
					continue
				}
				e := f.perms[code]
				if e == nil {
					e = &oaPerm{scopes: map[string]bool{}, phases: map[string]bool{}}
					f.perms[code] = e
				}
				e.scopes[scope] = true
				e.phases[phase] = true
			}
		}
	}
	if len(f.perms) == 0 || len(f.taskType) == 0 {
		t.Fatalf("openapi.yaml 解析结果为空: perms=%d taskType=%d", len(f.perms), len(f.taskType))
	}
	oaOnce.Store(f)
	return f
}

// derivedScope 按种子迁移的口径由 operation 的 x-tenant-scope 推导权限的 scope：
// 仅 platform 路由使用 = platform；仅 tenant 路由使用 = tenant；被 either 路由或两类路由共用 = both。
func (p *oaPerm) derivedScope() string {
	switch {
	case len(p.scopes) == 1 && p.scopes["platform"]:
		return "platform"
	case len(p.scopes) == 1 && p.scopes["tenant"]:
		return "tenant"
	}
	return "both"
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// rows 执行查询并把第一列收集为字符串切片。
func (e *env) column(t testing.TB, q string, args ...any) []string {
	t.Helper()
	conn := e.conn(t) // 已 USE 本库：查询里可以不带库名
	defer func() { _ = conn.Close() }()
	rows, err := conn.QueryContext(ctxT(t), q, args...)
	if err != nil {
		t.Fatalf("%v\n%s", err, q)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s sql.NullString
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s.String)
	}
	return out
}

func join(ss []string) string { return strings.Join(ss, ",") }

// mapFile / toMapFS：内存迁移集合（变异测试用）。
type mapFile = fstest.MapFile

func toMapFS(m map[string]*mapFile) fstest.MapFS { return fstest.MapFS(m) }
