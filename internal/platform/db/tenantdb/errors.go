package tenantdb

import "errors"

// 构建器与执行期的失败原因。其中与租户隔离有关的（ErrNoScope、ErrPlatformScope、ErrNotPlatform）是调用方 bug，
// 一律在发出任何 SQL 之前失败（fail closed）。
var (
	// ErrNoScope：传入了零值 Scope（没有 Scope）。
	ErrNoScope = errors.New("tenantdb: Scope 为空，拒绝执行")
	// ErrPlatformScope：平台范围 Scope 用在普通方法上；跨租户访问必须调用名称含 AcrossTenants 的方法（ADR-009 §2）。
	ErrPlatformScope = errors.New("tenantdb: 平台范围 Scope 只能用于名称含 AcrossTenants 的方法")
	// ErrNotPlatform：AcrossTenants 方法收到非平台范围的 Scope。
	ErrNotPlatform = errors.New("tenantdb: AcrossTenants 方法要求平台范围 Scope")
	// ErrNoCondition：UPDATE/DELETE 没有任何 Where 条件（防止整租户误改/误删）。
	ErrNoCondition = errors.New("tenantdb: UPDATE/DELETE 必须至少有一个 Where 条件")
	// ErrEmptyCondition：And/Or 没有子条件。
	ErrEmptyCondition = errors.New("tenantdb: And/Or 至少需要一个子条件")
	// ErrNilValue：比较条件的值为 nil（`= NULL` 永不成立；请用 IsNull/IsNotNull）。
	ErrNilValue = errors.New("tenantdb: 比较值为 nil，请改用 IsNull/IsNotNull")
	// ErrReservedColumn：条件或 SET 引用了 tenant_id（该列只由构建器管理）。
	ErrReservedColumn = errors.New("tenantdb: tenant_id 只由构建器管理，不能出现在条件或 SET 中")
	// ErrUnknownColumn：列不在表声明的白名单内（防标识符注入与拼写错误）。
	ErrUnknownColumn = errors.New("tenantdb: 列不在表声明的列清单内")
	// ErrInvalidIdentifier：表名/列名不是合法的小写标识符。
	ErrInvalidIdentifier = errors.New("tenantdb: 非法标识符")
	// ErrNoTenant：INSERT 需要租户范围 Scope（TenantID>0）。
	ErrNoTenant = errors.New("tenantdb: INSERT 需要租户范围 Scope")
	// ErrNoValues：INSERT/UPDATE 没有可写入的列。
	ErrNoValues = errors.New("tenantdb: 没有可写入的列")
	// ErrTxRequired：FOR UPDATE 必须在事务内执行（自动提交下锁立即释放，SKIP LOCKED 领取会重复）。
	ErrTxRequired = errors.New("tenantdb: FOR UPDATE 必须在事务内执行（db.WithTx）")
	// ErrInvalidArgument：LIMIT/OFFSET 等参数非法。
	ErrInvalidArgument = errors.New("tenantdb: 参数非法")
)
