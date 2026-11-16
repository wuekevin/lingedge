package tenantdb

import (
	"fmt"
	"reflect"
	"strings"
)

// MaxInValues 是 In 条件的值个数上限（MySQL 预处理语句最多 65535 个占位符，且租户条件与其他条件也占用）。
const MaxInValues = 10000

// isNil 识别 nil 以及带类型的 nil（nil 指针/切片/map/接口），它们作为参数都会变成 `= NULL`。
func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface, reflect.Func, reflect.Chan:
		return rv.IsNil()
	}
	return false
}

type condKind int

const (
	kEq condKind = iota
	kNe
	kGt
	kGte
	kLt
	kLte
	kLike
	kIn
	kIsNull
	kNotNull
	kAnd
	kOr
	kNot
)

// Cond 是 Where 条件。只能用本包的构造函数创建（字段未导出）；列名在构建时按表白名单校验，值一律作为 ? 参数。
type Cond struct {
	kind condKind
	col  string
	val  any
	vals []any
	subs []Cond
}

// Eq：col = ?（val 为 nil 时构建失败，请用 IsNull）。
func Eq(col string, val any) Cond { return Cond{kind: kEq, col: col, val: val} }

// Ne：col <> ?。注意 SQL 三值逻辑：col 为 NULL 的行不会被选中（Not 同理），需要包含 NULL 行时写 Or(IsNull(col), Ne(col, v))。
func Ne(col string, val any) Cond { return Cond{kind: kNe, col: col, val: val} }

// Gt：col > ?。
func Gt(col string, val any) Cond { return Cond{kind: kGt, col: col, val: val} }

// Gte：col >= ?。
func Gte(col string, val any) Cond { return Cond{kind: kGte, col: col, val: val} }

// Lt：col < ?。
func Lt(col string, val any) Cond { return Cond{kind: kLt, col: col, val: val} }

// Lte：col <= ?。
func Lte(col string, val any) Cond { return Cond{kind: kLte, col: col, val: val} }

// Like：col LIKE ?（通配符由调用方在值里给出；用户输入须先转义 % 与 _）。
func Like(col, pattern string) Cond { return Cond{kind: kLike, col: col, val: pattern} }

// In：col IN (?, ?, …)；空列表恒为假（`1 = 0`），不会退化为无条件。
func In(col string, vals ...any) Cond { return Cond{kind: kIn, col: col, vals: vals} }

// IsNull：col IS NULL。
func IsNull(col string) Cond { return Cond{kind: kIsNull, col: col} }

// IsNotNull：col IS NOT NULL。
func IsNotNull(col string) Cond { return Cond{kind: kNotNull, col: col} }

// And：全部成立（至少一个子条件）。
func And(subs ...Cond) Cond { return Cond{kind: kAnd, subs: subs} }

// Or：任一成立（至少一个子条件）。顶层 Where 与 tenant_id 条件以 AND 连接，Or 总被括号包裹，无法逃出租户条件。
func Or(subs ...Cond) Cond { return Cond{kind: kOr, subs: subs} }

// Not：取反。
func Not(sub Cond) Cond { return Cond{kind: kNot, subs: []Cond{sub}} }

var cmpOps = map[condKind]string{kEq: "=", kNe: "<>", kGt: ">", kGte: ">=", kLt: "<", kLte: "<=", kLike: "LIKE"}

func (c Cond) build(t Table, b *strings.Builder, args *[]any) error {
	column := func() error {
		switch {
		case c.col == TenantColumn:
			return fmt.Errorf("%w: 条件列 %s", ErrReservedColumn, c.col)
		case !t.known(c.col):
			return fmt.Errorf("%w: 表 %s 的列 %q", ErrUnknownColumn, t.name, c.col)
		}
		return nil
	}
	switch c.kind {
	case kEq, kNe, kGt, kGte, kLt, kLte, kLike:
		if err := column(); err != nil {
			return err
		}
		if isNil(c.val) {
			return fmt.Errorf("%w: 列 %s", ErrNilValue, c.col)
		}
		b.WriteString(quote(c.col) + " " + cmpOps[c.kind] + " ?")
		*args = append(*args, c.val)
	case kIn:
		if err := column(); err != nil {
			return err
		}
		if len(c.vals) == 0 {
			b.WriteString("1 = 0")
			return nil
		}
		if len(c.vals) > MaxInValues {
			return fmt.Errorf("%w: 列 %s 的 IN 列表超过 %d 个值", ErrInvalidArgument, c.col, MaxInValues)
		}
		for _, v := range c.vals {
			if isNil(v) {
				return fmt.Errorf("%w: 列 %s 的 IN 列表含 nil", ErrNilValue, c.col)
			}
		}
		b.WriteString(quote(c.col) + " IN (" + strings.TrimSuffix(strings.Repeat("?, ", len(c.vals)), ", ") + ")")
		*args = append(*args, c.vals...)
	case kIsNull, kNotNull:
		if err := column(); err != nil {
			return err
		}
		if c.kind == kIsNull {
			b.WriteString(quote(c.col) + " IS NULL")
		} else {
			b.WriteString(quote(c.col) + " IS NOT NULL")
		}
	case kAnd, kOr:
		if len(c.subs) == 0 {
			return ErrEmptyCondition
		}
		sep := " AND "
		if c.kind == kOr {
			sep = " OR "
		}
		b.WriteString("(")
		for i, s := range c.subs {
			if i > 0 {
				b.WriteString(sep)
			}
			if err := s.build(t, b, args); err != nil {
				return err
			}
		}
		b.WriteString(")")
	case kNot:
		b.WriteString("(NOT ")
		if err := c.subs[0].build(t, b, args); err != nil {
			return err
		}
		b.WriteString(")")
	default:
		return fmt.Errorf("%w: 未知条件类型", ErrInvalidArgument)
	}
	return nil
}

// buildWhere 生成 ` WHERE ...`：tenant 为 nil 表示 AcrossTenants（不加租户条件）。租户条件总是第一项，并与其余条件以 AND 连接。
func buildWhere(t Table, tenant *uint64, conds []Cond, args *[]any) (string, error) {
	var b strings.Builder
	n := 0
	if tenant != nil {
		b.WriteString(quote(TenantColumn) + " = ?")
		*args = append(*args, *tenant)
		n++
	}
	for _, c := range conds {
		if n > 0 {
			b.WriteString(" AND ")
		}
		if err := c.build(t, &b, args); err != nil {
			return "", err
		}
		n++
	}
	if n == 0 {
		return "", nil
	}
	return " WHERE " + b.String(), nil
}
