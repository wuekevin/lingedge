package migrate

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// 退出码（lingedge-server migrate）。Tools 以退出码区分失败原因；9 与 ADR-013 §2 的「不可逆未确认」一致。
const (
	ExitOK           = 0
	ExitFailure      = 1
	ExitUsage        = 2
	ExitLint         = 3
	ExitDirty        = 4
	ExitChecksum     = 5
	ExitVersion      = 6
	ExitLocked       = 7
	ExitPreflight    = 8
	ExitIrreversible = 9
	// ExitOutOfSync：数据库与二进制的迁移集合不一致（二进制比数据库旧、乱序、版本表与运行记录矛盾）。
	// Tools 据此区分「该回滚/换二进制」与普通失败（ADR-013 §3）。
	ExitOutOfSync = 10
)

// ErrLint 表示迁移集合未通过 lint（fail-closed：带缺陷的迁移不会被执行）。
type ErrLint struct{ Findings []Finding }

func (e *ErrLint) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "迁移 lint 未通过（%d 项），拒绝执行：", len(e.Findings))
	for i, f := range e.Findings {
		if i >= 10 {
			fmt.Fprintf(&b, "\n  ……另有 %d 项", len(e.Findings)-i)
			break
		}
		fmt.Fprintf(&b, "\n  %s", f)
	}
	return b.String()
}

// ErrUnsupportedVersion 表示 MySQL 版本不满足最低要求（ADR-016 §6：CHECK 约束自 8.0.16 起才强制执行）。
type ErrUnsupportedVersion struct{ Version, Reason string }

func (e *ErrUnsupportedVersion) Error() string {
	return fmt.Sprintf("不支持的数据库版本 %q：%s；LingEdge 要求 MySQL >= 8.0.16（CHECK 约束自该版本才强制执行，ADR-016 §6）", e.Version, e.Reason)
}

// ErrDirty 表示迁移处于 dirty 状态：上一次执行中断或失败，MySQL DDL 非事务，数据库可能已部分变更（ADR-013 §5）。
type ErrDirty struct {
	Version   int64
	Name      string
	Direction string // up | down
	Status    string // running | failed
	Cause     string
}

func (e *ErrDirty) Error() string {
	state := "执行被中断（进程退出或连接断开，无失败原因记录）"
	if e.Status == "failed" {
		state = "执行失败"
	}
	if e.Cause != "" {
		state += "：" + e.Cause
	}
	return fmt.Sprintf("迁移处于 dirty 状态，拒绝继续：%06d_%s（%s）%s。\n"+
		"MySQL 的 DDL 会隐式提交、不能随失败回滚，数据库可能已有该迁移的部分变更。恢复步骤：\n"+
		"  1. 检查库中该迁移创建/修改的对象（`lingedge-server migrate status` 查看详情），必要时从升级前备份恢复（ADR-013 §3）；\n"+
		"  2. 排除失败原因（权限、空间、锁、被篡改的文件……）；\n"+
		"  3. 执行 `lingedge-server migrate recover --version %d` 清除 dirty 标记（仅当确认可重跑）；\n"+
		"  4. 重新执行 `lingedge-server migrate %s`：迁移带幂等守卫（IF NOT EXISTS 等）时，重跑与一次性执行等价；\n"+
		"     无原生守卫的单语句 DDL（如 ADD COLUMN）若已经生效，重跑会报「已存在」——此时先确认对象已按迁移内容就位，再决定从备份恢复，或修正后重跑。",
		e.Version, e.Name, e.Direction, state, e.Version, e.Direction)
}

// ErrChecksumMismatch 表示已应用迁移的文件内容与应用时记录的校验和不一致（文件被改动）。
type ErrChecksumMismatch struct {
	Version  int64
	File     string
	Recorded string // 空表示没有应用记录（不是经本框架应用的）
	Actual   string
}

func (e *ErrChecksumMismatch) Error() string {
	if e.Recorded == "" {
		return fmt.Sprintf("已应用的迁移 %s 没有校验和记录（不是经 lingedge-server migrate 应用的），拒绝继续", e.File)
	}
	return fmt.Sprintf("迁移文件被改动，拒绝继续：%s 应用时的 SHA-256 为 %s…，当前为 %s…。已应用的迁移不得修改，修正须新增后续迁移（ADR-016、Story 1.3 AC4）",
		e.File, short(e.Recorded), short(e.Actual))
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// ErrUnknownApplied 表示数据库记录了当前二进制不认识的迁移（二进制比数据库旧，或迁移文件丢失）。
type ErrUnknownApplied struct{ Versions []int64 }

func (e *ErrUnknownApplied) Error() string {
	vs := make([]string, 0, len(e.Versions))
	for _, v := range e.Versions {
		vs = append(vs, fmt.Sprintf("%06d", v))
	}
	sort.Strings(vs)
	return fmt.Sprintf("数据库已应用的迁移 %s 在当前二进制中不存在（二进制版本低于数据库，或迁移文件被删除），拒绝继续；请使用不低于数据库版本的 lingedge-server，或按 ADR-013 §3 回滚流程恢复", strings.Join(vs, ", "))
}

// ErrOutOfOrder 表示存在序号低于已应用最大版本、却尚未应用的迁移（乱序）。
type ErrOutOfOrder struct {
	Missing []int64
	Max     int64
}

func (e *ErrOutOfOrder) Error() string {
	return fmt.Sprintf("检测到乱序迁移：%v 尚未应用，但数据库已应用到 %06d；迁移必须按序号单调追加，拒绝继续", e.Missing, e.Max)
}

// ErrIrreversibleNotAccepted 表示待执行迁移含 irreversible，但未带 --accept-irreversible（ADR-013 §2）。
type ErrIrreversibleNotAccepted struct{ Files []string }

func (e *ErrIrreversibleNotAccepted) Error() string {
	return fmt.Sprintf("待执行迁移含不可逆（irreversible）迁移 %s：回滚路径只有「恢复升级前备份 + 旧二进制」，备份点之后的数据会丢失。确认已完成备份后，加 --accept-irreversible 重试（ADR-013 §2）",
		strings.Join(e.Files, ", "))
}

// ErrDownNotAllowed 表示请求回退的迁移不是 reversible（ADR-013 §3）。
type ErrDownNotAllowed struct {
	File  string
	Class Class
}

func (e *ErrDownNotAllowed) Error() string {
	switch e.Class {
	case ClassExpand:
		return fmt.Sprintf("拒绝回退 %s：分类为 expand（只增不破），不提供数据库回退；升级失败的即时回滚只需切回旧二进制，旧二进制与 expand 后的库结构兼容（ADR-013 §1）", e.File)
	case ClassIrreversible:
		return fmt.Sprintf("拒绝回退 %s：分类为 irreversible，没有回退路径；回滚 = 恢复升级前备份 + 旧二进制（ADR-013 §3）", e.File)
	}
	return fmt.Sprintf("拒绝回退 %s：只有 reversible 迁移可以回退", e.File)
}

// ErrMigrationRunning 表示另一个 migrate 正在运行（互斥被持有且有迁移处于 running）：此时计划与完整性都不稳定，
// 不应把它当作 dirty，也不应给出计划；等其结束后再查看。
type ErrMigrationRunning struct{ Version int64 }

func (e *ErrMigrationRunning) Error() string {
	return fmt.Sprintf("另一个 lingedge-server migrate 正在运行（迁移互斥被持有，000%03d 等迁移执行中）；等它结束后再查看计划", e.Version)
}

// ErrInconsistent 表示迁移版本表与运行记录互相矛盾（如运行记录称已应用、版本表却没有），不是本框架正常流程能产生的状态。
type ErrInconsistent struct {
	Version int64
	Detail  string
}

func (e *ErrInconsistent) Error() string {
	return fmt.Sprintf("迁移状态不一致：%06d %s。这不是正常流程能产生的状态（通常是手工改动了 %s 或 %s），请人工核对两张表与库结构后再继续（必要时从备份恢复）", e.Version, e.Detail, VersionTable, RunsTable)
}

// ErrLockTimeout 表示等待迁移互斥超时（另一个 migrate 正在运行）。
type ErrLockTimeout struct{ Waited string }

func (e *ErrLockTimeout) Error() string {
	return fmt.Sprintf("另一个 lingedge-server migrate 正在运行（等待迁移互斥 %s 后超时）；确认没有迁移进程后重试", e.Waited)
}

// ErrPreflight 表示迁移账号的前置权限/形态检查失败（如 binlog=ON 时缺少 SET_USER_ID，ERROR 1419）。
type ErrPreflight struct {
	Reason string
	Err    error
}

func (e *ErrPreflight) Error() string { return "迁移预检失败：" + e.Reason }
func (e *ErrPreflight) Unwrap() error { return e.Err }

// ExitCode 把迁移错误映射为退出码。
func ExitCode(err error) int {
	var (
		lint *ErrLint
		dirt *ErrDirty
		sum  *ErrChecksumMismatch
		ver  *ErrUnsupportedVersion
		lock *ErrLockTimeout
		run  *ErrMigrationRunning
		pre  *ErrPreflight
		irr  *ErrIrreversibleNotAccepted
		unk  *ErrUnknownApplied
		ooo  *ErrOutOfOrder
		inc  *ErrInconsistent
	)
	switch {
	case err == nil:
		return ExitOK
	case errors.As(err, &lint):
		return ExitLint
	case errors.As(err, &dirt):
		return ExitDirty
	case errors.As(err, &sum):
		return ExitChecksum
	case errors.As(err, &ver):
		return ExitVersion
	case errors.As(err, &lock), errors.As(err, &run):
		return ExitLocked
	case errors.As(err, &pre):
		return ExitPreflight
	case errors.As(err, &irr):
		return ExitIrreversible
	case errors.As(err, &unk), errors.As(err, &ooo), errors.As(err, &inc):
		return ExitOutOfSync
	}
	return ExitFailure
}
