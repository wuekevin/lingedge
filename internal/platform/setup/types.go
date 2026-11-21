package setup

import (
	"context"
)

// 向导步骤键（openapi SetupStatus.steps[].key，对应基线 §21.1）。
const (
	StepEnvironment = "environment"
	StepDatabase    = "database"
	StepRedis       = "redis"
	StepInitDB      = "init_db"
	StepAdmin       = "admin"
	StepSystem      = "system"
	StepKeys        = "keys"
	StepComplete    = "complete"
)

var stepOrder = []string{StepEnvironment, StepDatabase, StepRedis, StepInitDB, StepAdmin, StepSystem, StepKeys, StepComplete}

// 步骤状态（openapi SetupStatus.steps[].status）。
const (
	StatusPending = "pending"
	StatusDone    = "done"
	StatusFailed  = "failed"
	StatusRunning = "running"
)

// StepStatus 是 SetupStatus.steps 的一项。
type StepStatus struct {
	Key     string  `json:"key"`
	Status  string  `json:"status"`
	Message *string `json:"message,omitempty"`
}

// Status 是 openapi SetupStatus。
type Status struct {
	Installed     bool         `json:"installed"`
	Steps         []StepStatus `json:"steps"`
	ServerVersion string       `json:"server_version"`
}

// CheckItem / CheckResult 是 openapi SetupCheckResult。
type CheckItem struct {
	Key      string `json:"key"`
	OK       bool   `json:"ok"`
	Severity string `json:"severity,omitempty"`
	Message  string `json:"message"`
}

// CheckResult 是环境检查结果。
type CheckResult struct {
	Items []CheckItem `json:"items"`
}

// OneTimeSecrets 是 SetupStepResult.one_time_secrets（只读、仅返回一次；响应 Cache-Control: no-store；不入日志/审计；响应后服务端清零）。
// migrate_password 仅 complete 在迁移口令由服务端自动生成时返回（ADR-026 §5）；totp_* 与 recovery_codes 仅 admin 步骤的 TOTP 绑定返回（CC-12，加法）。
type OneTimeSecrets struct {
	MigratePassword string   `json:"migrate_password,omitempty"`
	TOTPSecret      string   `json:"totp_secret,omitempty"`
	TOTPOtpauthURI  string   `json:"totp_otpauth_uri,omitempty"`
	RecoveryCodes   []string `json:"recovery_codes,omitempty"`
}

// StepResult 是 openapi SetupStepResult。
type StepResult struct {
	Step    string          `json:"step"`
	OK      bool            `json:"ok"`
	Message string          `json:"message,omitempty"`
	Next    *string         `json:"next"`
	Secrets *OneTimeSecrets `json:"one_time_secrets,omitempty"`
}

// DatabaseInput 是 openapi SetupDbConfig（含 CC-11 的加法字段）。
type DatabaseInput struct {
	Host            string `json:"host"`
	Port            int    `json:"port"`
	Database        string `json:"database"`
	Username        string `json:"username"`
	Password        string `json:"password"`
	TLS             bool   `json:"tls"`
	CreateIfMissing bool   `json:"create_if_missing"`
	AdminUsername   string `json:"admin_username"`
	AdminPassword   string `json:"admin_password"`
	MigrateUsername string `json:"migrate_username"`
	MigratePassword string `json:"migrate_password"`
	MaintUsername   string `json:"maint_username"`
	MaintPassword   string `json:"maint_password"`
}

// RedisInput 是 openapi SetupRedisConfig。
type RedisInput struct {
	Address  string `json:"address"`
	Password string `json:"password"`
	DB       int32  `json:"db"`
	TLS      bool   `json:"tls"`
}

// AdminInput 是 openapi SetupAdminCreate（CC-12 加法：locale、enable_totp、totp_code）。
type AdminInput struct {
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Password    string `json:"password"`
	Locale      string `json:"locale"`
	EnableTOTP  bool   `json:"enable_totp"`
	TOTPCode    string `json:"totp_code"`
}

// SystemInput 是 openapi SetupSystemConfig（CC-12 加法：timezone）。brand 只取 name/name_en/support_email（向导阶段没有媒体库，logo/favicon 不接受）。
type SystemInput struct {
	RunMode       string      `json:"run_mode"`
	Brand         *BrandInput `json:"brand"`
	BaseURL       string      `json:"base_url"`
	DefaultLocale string      `json:"default_locale"`
	Timezone      string      `json:"timezone"`
}

// BrandInput 是 BrandSettings 在向导中接受的子集。
type BrandInput struct {
	Name         string `json:"name"`
	NameEN       string `json:"name_en"`
	SupportEmail string `json:"support_email"`
}

// AdminService 是向导创建管理员所需的认证能力（由装配层用 auth.Module.Bootstrap 适配：platform 层不得 import 业务模块 internal/auth）。
type AdminService interface {
	CreateAdmin(ctx context.Context, in AdminCreate) (AdminCreated, error)
	BeginTOTP(ctx context.Context, userUID string) (TOTPEnrollment, error)
	ConfirmTOTP(ctx context.Context, userUID, code string) (recoveryCodes []string, err error)
	HasAdmin(ctx context.Context) (bool, error)
}

// AdminCreate / AdminCreated / TOTPEnrollment 是 AdminService 的输入输出。
type (
	AdminCreate struct {
		Email, DisplayName, Password, Locale string
	}
	AdminCreated struct {
		UserUID     string
		Created     bool
		TOTPEnabled bool
	}
	TOTPEnrollment struct{ Secret, OTPAuthURI string }
)
