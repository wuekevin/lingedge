package db

import (
	"errors"

	"github.com/go-sql-driver/mysql"
)

// ErrNotFound 表示按条件未找到行（Get 的 sql.ErrNoRows、UPDATE/DELETE 的 0 行受影响经 tenantdb 转换而来）。
// 越权访问表现为同一错误，调用方一律映射为 404 resource.not_found（ADR-009 §4）。
var ErrNotFound = errors.New("db: 未找到匹配的行")

// MySQL 错误号（仅登记本层与测试需要的）。
const (
	MySQLErrDupEntry          uint16 = 1062 // ER_DUP_ENTRY
	MySQLErrTableAccessDenied uint16 = 1142 // ER_TABLEACCESS_DENIED_ERROR
	MySQLErrNoReferencedRow   uint16 = 1452 // ER_NO_REFERENCED_ROW_2（外键拒绝，复合 FK 的租户纵深防御）
	MySQLErrTriggerNoSuper    uint16 = 1419 // ER_BINLOG_CREATE_ROUTINE_NEED_SUPER
)

// MySQLErrorNumber 提取错误链中的 MySQL 服务端错误号；不是 MySQL 服务端错误时 ok=false。
// Repository 外的代码不得 import 驱动，需要按错误号分支时用它。
func MySQLErrorNumber(err error) (num uint16, ok bool) {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return me.Number, true
	}
	return 0, false
}

// IsDuplicate 报告错误是否为唯一键冲突（1062）。
func IsDuplicate(err error) bool {
	n, ok := MySQLErrorNumber(err)
	return ok && n == MySQLErrDupEntry
}
