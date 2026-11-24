package installer

import (
	"context"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/backup"
	"github.com/wuekevin/lingedge/internal/tools/dbmigrate"
	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/probe"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/resolver"
	"github.com/wuekevin/lingedge/internal/tools/rollback"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

// 升级事务的步骤号（journal 的 step 即 tools-design §17.3 的 13 步）。
const (
	UpStepLock        = 1  // 取锁、检查 journal（锁由 cli 在入口取得；journal 在此开启并记录请求）
	UpStepManifest    = 2  // 获取并验证签名清单
	UpStepCompat      = 3  // 兼容性检查（min_upgrade_from / requires / conflicts / 系统）
	UpStepPlan        = 4  // 解析目标版本并展示 Plan
	UpStepDownload    = 5  // 下载并验证包到 staging，并落地为 releases/<新版本>（不切换 current）
	UpStepBackup      = 6  // 备份当前版本（server：含数据库）
	UpStepMigratePlan = 7  // migrate plan（决策表；不可逆需确认）
	UpStepStop        = 8  // 停止服务
	UpStepMigrateUp   = 9  // migrate up
	UpStepSwitch      = 10 // 原子切换 current
	UpStepStart       = 11 // 启动服务
	UpStepHealth      = 12 // 健康检查
	UpStepCommit      = 13 // 确认：写 state.json（提交点）、清理旧 release
	UpTotalSteps      = 13
)

var updateStepNames = [UpTotalSteps + 1]string{"", "lock_journal", "verify_manifest", "compat_check", "plan", "download_verify_extract", "backup", "migrate_plan",
	"stop_service", "migrate_up", "switch_current", "start_service", "health_check", "confirm_commit"}

var updateStepTitles = [UpTotalSteps + 1]string{"", "取锁并检查未完成事务", "获取并验证签名清单", "兼容性检查", "解析目标版本与升级计划", "下载并验证包、落地新 release",
	"备份当前版本", "检查数据库迁移（migrate plan）", "停止服务", "执行数据库迁移（migrate up）", "原子切换 current", "启动服务", "健康检查", "确认并保存状态（提交点）"}

// UpdateStepName 返回升级事务某一步的稳定名称。
func UpdateStepName(n int) string {
	if n < 1 || n > UpTotalSteps {
		return ""
	}
	return updateStepNames[n]
}

// UpdateStepTitle 返回升级事务某一步的中文标题。
func UpdateStepTitle(n int) string {
	if n < 1 || n > UpTotalSteps {
		return ""
	}
	return updateStepTitles[n]
}

// 升级/回滚 journal 的撤销动作（installer 自己的；一步内按记录顺序执行）。
const (
	ActUpdateRequest       = "update_request"        // 请求记录（撤销为空操作；Extra 携带事实，recover 据此识别这是一条升级事务并复核回滚结果）
	ActUpdateNoteBackup    = "update_note_backup"    // 升级前备份文件（只是记录，撤销为空操作；备份永远保留）
	ActUpdateStartUnits    = "update_start_units"    // 撤销「停止服务」：若升级前是 active 则重新启动（旧版本、旧 current）
	ActUpdateDBRevert      = "update_db_revert"      // 撤销「migrate up」：按迁移级别回退数据库（rollback.Executor.RevertDB）
	ActUpdateSwitchCurrent = "update_switch_current" // 撤销「切换 current」：切回旧版本
	ActRollbackRequest     = "rollback_request"      // 手动回滚事务的请求记录（撤销为空操作）
)

// UpdateEnv 是升级/回滚的外部依赖装配（Deps.Update；nil 表示该部署不支持升级——生产由 cli 装配）。
// 业务逻辑（13 步、journal、撤销、决策表、迁移、回滚路径）永远是生产代码；这里只是「世界」的接缝。
type UpdateEnv struct {
	// Backup 创建/校验备份；Rollback 执行回滚路径（它引用同一个 Backup 与 Mig）；Mig 调用 lingedge-server migrate。
	Backup   *backup.Service
	Rollback *rollback.Executor
	Mig      *dbmigrate.Migrator
	// DB 取得迁移账号连接（AC9：环境变量 ＞ 口令文件 ＞ migrate.yaml ＞ 交互输入）；interactive=false 时不得提示，无凭据返回 backup.ErrNoCredentials。
	// 调用方（installer）用后 Close。
	DB func(ctx context.Context, interactive bool) (*backup.DBConn, error)
	// BackupPassphrase 返回升级前备份的口令（nil = 不加密）。口令只存在于内存，不进 journal。
	BackupPassphrase func(ctx context.Context) ([]byte, error)
	// BackupDir 是备份输出目录（主机路径；空取 backup 服务的缺省目录）。
	BackupDir string
	// TypedConfirm 向用户要求键入 phrase 才算确认（交互；nil = 不支持交互确认，只认 UpdateRequest.AcceptIrreversible）。
	TypedConfirm func(message string, items []string, phrase string) (bool, error)
	// PostChecks 在回滚/升级之后运行 doctor 子集（只读诊断，结果只作为报告，不改变成败）；nil = 不运行。
	PostChecks func(ctx context.Context, component string) PostCheckReport
	// ToolsVersion 是当前 Tools 版本（update self 比较用；空 = 取 pkg/version）。
	ToolsVersion string
}

// PostCheckReport 是 doctor 子集的摘要。
type PostCheckReport struct {
	Pass     int      `json:"pass"`
	Warn     int      `json:"warn"`
	Fail     int      `json:"fail"`
	Skip     int      `json:"skip"`
	Problems []string `json:"problems,omitempty"`
}

// UpdateRequest 是一次升级请求。
type UpdateRequest struct {
	Component    string
	Version      string
	Channel      string
	Offline      string
	AllowExpired bool
	// AcceptIrreversible 对应 --accept-irreversible（非交互时含不可逆迁移必须显式给出）。
	AcceptIrreversible bool
	NoBackup           bool
	NoRollback         bool
	DryRun             bool
	NonInteractive     bool
	Yes                bool
}

// MigrationPlan 是升级的迁移计划与决策。
type MigrationPlan struct {
	// Source：manifest（清单预告）| migrate_plan（新二进制对真实数据库的计划）| none（组件没有数据库）。
	Source  string           `json:"source"`
	Classes []string         `json:"classes"`
	Items   []dbmigrate.Item `json:"items,omitempty"`
	// SchemaFrom/SchemaTo：升级前/后的数据库迁移版本（仅有数据库且已连库时）。
	SchemaFrom      int64 `json:"schema_from,omitempty"`
	SchemaTo        int64 `json:"schema_to,omitempty"`
	HasIrreversible bool  `json:"has_irreversible"`
	// DBInitialized：server 的安装向导已完成（install.lock 存在）。false = setup mode，没有数据库可备份/迁移。
	DBInitialized bool `json:"db_initialized"`
}

// UpdatePlan 是升级计划（--dry-run/--plan-only、update check 的升级项、TUI 确认页同源）。
type UpdatePlan struct {
	Component string `json:"component"`
	From      string `json:"from"`
	To        string `json:"to"`
	Variant   string `json:"variant,omitempty"`
	Channel   string `json:"channel"`
	// Action：upgrade | keep（已是目标版本）。
	Action     string            `json:"action"`
	Migrations MigrationPlan     `json:"migrations"`
	Decision   rollback.Decision `json:"decision"`
	// Backup：将创建的备份（server 含数据库）；不做备份时 Planned=false 并说明 Reason。
	Backup UpdateBackupPlan `json:"backup"`
	// Units 是将停止/启动的服务单元；Steps 是 13 步。
	Units    []string         `json:"units"`
	Steps    []UpdateStepInfo `json:"steps"`
	Warnings []Warning        `json:"warnings"`
	// Credentials：迁移凭据可用性（not_required | available | missing_non_interactive | interactive）。不含任何口令。
	Credentials string `json:"db_login"`
	// KeepReleases 是确认成功后保留的 release 数。
	KeepReleases int `json:"keep_releases"`
	// Resolver 的 Plan（端口/资源/文件…）。
	Resolved *resolver.Plan `json:"resolved,omitempty"`

	req   UpdateRequest
	pc    resolver.PlanComponent
	rec   registry.Component
	mf    *manifest.Result
	spec  ComponentSpec
	units []service.UnitName
	probe probe.Result
}

// UpdateBackupPlan 描述备份。
type UpdateBackupPlan struct {
	Planned   bool   `json:"planned"`
	Reason    string `json:"reason,omitempty"`
	Dir       string `json:"dir,omitempty"`
	Encrypted bool   `json:"encrypted"`
	Database  bool   `json:"database"`
	Verify    bool   `json:"verify"`
}

// UpdateStepInfo 是计划里的一步。
type UpdateStepInfo struct {
	N     int    `json:"n"`
	Name  string `json:"name"`
	Title string `json:"title"`
}

// 升级结果取值。
const (
	OutcomeUpdated        = "updated"
	OutcomeAlreadyCurrent = "already_up_to_date"
)

// UpdateResult 是升级的结果。
type UpdateResult struct {
	Component string `json:"component"`
	From      string `json:"from"`
	To        string `json:"to"`
	Outcome   string `json:"outcome"`
	Tx        string `json:"tx,omitempty"`
	// Path 是本次升级若失败/需回滚所走的路径（registry.Path*）。
	Path     string            `json:"path"`
	Decision rollback.Decision `json:"decision"`
	// Backup 是升级前备份文件（server 含数据库）；BackupVerified 表示写完后又做了完整可读校验。
	Backup         string `json:"backup,omitempty"`
	BackupVerified bool   `json:"backup_verified"`
	SchemaFrom     int64  `json:"schema_from,omitempty"`
	SchemaTo       int64  `json:"schema_to,omitempty"`
	Health         string `json:"health,omitempty"`
	// Pruned 是确认成功之后清理掉的旧 release。
	Pruned   []string         `json:"pruned_releases"`
	Checks   *PostCheckReport `json:"post_checks,omitempty"`
	Warnings []Warning        `json:"warnings"`
	Plan     *UpdatePlan      `json:"-"`
}

func csv(ss []string) string { return strings.Join(ss, ",") }
