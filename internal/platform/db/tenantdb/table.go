// Package tenantdb 是租户范围表的查询构建器（Story 1.5 AC3、ADR-009 §2）：
//
//   - SELECT/UPDATE/DELETE 自动追加 `tenant_id = ?`（取自 Scope），调用方无法省略也无法覆盖；
//   - INSERT 强制写入 scope.TenantID，请求体里的 tenant_id 被忽略；
//   - 平台范围（跨租户）只能经名称含 AcrossTenants 的方法；零值 Scope 一律失败；
//   - 标识符（表名、列名）来自 NewTable 声明的白名单，值一律用 ? 占位符，生成的 SQL 文本不含任何调用方提供的值；
//   - UPDATE/DELETE 必须带至少一个 Where 条件；越权的 UPDATE/DELETE 表现为 0 行（ExecOne 转为 db.ErrNotFound）。
//
// 构建器只生成语句并经 db.Executor 执行；事务由 db.WithTx 经 context 提供（Repository 用 pool.Executor(ctx) 取得执行器）。
package tenantdb

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/db/internal/token"
	"github.com/wuekevin/lingedge/internal/platform/scope"
)

// TenantColumn 是租户列名，所有租户范围表都有（database/README §2.1 的 T）。
const TenantColumn = "tenant_id"

var reIdent = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Table 描述一张租户范围表：表名与列白名单（不含 tenant_id，它由构建器管理且总是存在）。
type Table struct {
	name string
	cols []string
	set  map[string]struct{}
}

// NewTable 声明租户范围表。name 与每个列名必须是小写标识符；columns 至少一列，且不得包含 tenant_id。
func NewTable(name string, columns ...string) (Table, error) {
	if !reIdent.MatchString(name) {
		return Table{}, fmt.Errorf("%w: 表名 %q", ErrInvalidIdentifier, name)
	}
	if len(columns) == 0 {
		return Table{}, fmt.Errorf("%w: 表 %s 未声明任何列", ErrNoValues, name)
	}
	t := Table{name: name, set: make(map[string]struct{}, len(columns))}
	for _, c := range columns {
		switch {
		case !reIdent.MatchString(c):
			return Table{}, fmt.Errorf("%w: 列名 %q", ErrInvalidIdentifier, c)
		case c == TenantColumn:
			return Table{}, fmt.Errorf("%w: 表 %s 不得声明 %s", ErrReservedColumn, name, TenantColumn)
		}
		if _, dup := t.set[c]; dup {
			return Table{}, fmt.Errorf("%w: 表 %s 重复声明列 %s", ErrInvalidIdentifier, name, c)
		}
		t.set[c] = struct{}{}
		t.cols = append(t.cols, c)
	}
	return t, nil
}

// MustTable 是 NewTable 的包级变量形式；声明有误属于编程错误，直接 panic（包初始化时即暴露）。
func MustTable(name string, columns ...string) Table {
	t, err := NewTable(name, columns...)
	if err != nil {
		panic(err)
	}
	return t
}

// Name 返回表名。
func (t Table) Name() string { return t.name }

func (t Table) known(col string) bool { _, ok := t.set[col]; return ok }

func quote(ident string) string { return "`" + ident + "`" }

// mint 把构建器生成的文本包装为 db.Query（凭据只在本包内可取得）。
func mint(text string) db.Query { return db.FromBuilder(token.New(), text) }

// tenantOf 取得普通（租户限定）方法所需的租户 id；零值与平台范围 Scope 在此被拒绝。
func tenantOf(sc scope.Scope) (uint64, error) {
	switch {
	case sc.IsZero():
		return 0, ErrNoScope
	case sc.IsPlatform():
		return 0, ErrPlatformScope
	case sc.TenantID() == 0:
		return 0, ErrNoScope
	}
	return sc.TenantID(), nil
}

// requirePlatform 校验 AcrossTenants 方法的 Scope。
func requirePlatform(sc scope.Scope) error {
	switch {
	case sc.IsZero():
		return ErrNoScope
	case !sc.IsPlatform():
		return ErrNotPlatform
	}
	return nil
}

func joinQuoted(cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = quote(c)
	}
	return strings.Join(q, ", ")
}
