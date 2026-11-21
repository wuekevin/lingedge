package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/migrate"
	"github.com/wuekevin/lingedge/pkg/secretbox"
)

// 无头路径 `lingedge-server setup --from-file <YAML>`（ADR-026 §7、tools-design §13）。
//
// 文件必须是调用者属主、权限恰为 0600 的普通文件（O_NOFOLLOW + fstat），否则拒绝（退出码 3）。**首次成功解析后**进程立即对内存缓冲清零，
// 并用 0 覆盖后 unlink 该文件（尽力而为：日志型/COW 文件系统与备份上的残留风险见 docs/operations/secrets.md）；删除失败醒目警告，结果带 from_file_residual=true。
// 因此续做必须重新提供文件。口令字段也可写成 *_file（间接指向另一个 0600/0400 的文件）。自动生成的迁移口令只在结果里输出一次，不落盘。

// ErrFromFilePermission：文件权限/属主/类型不满足要求（CLI 退出码 3）。
var ErrFromFilePermission = errors.New("from-file 文件必须是调用者属主、权限恰为 0600 的普通文件")

type secretField struct {
	Value string `yaml:"password"`
	File  string `yaml:"password_file"`
}

type fileAccount struct {
	User        string `yaml:"user"`
	secretField `yaml:",inline"`
}

// FromFile 是 --from-file 的 YAML 结构（KnownFields：未知键拒绝）。
type FromFile struct {
	Database struct {
		Host    string      `yaml:"host"`
		Port    int         `yaml:"port"`
		Name    string      `yaml:"name"`
		TLS     bool        `yaml:"tls"`
		Admin   fileAccount `yaml:"admin"`
		Migrate fileAccount `yaml:"migrate"`
		App     fileAccount `yaml:"app"`
		Maint   fileAccount `yaml:"maint"`
	} `yaml:"database"`
	Redis struct {
		Addr        string `yaml:"addr"`
		DB          int    `yaml:"db"`
		TLS         bool   `yaml:"tls"`
		secretField `yaml:",inline"`
	} `yaml:"redis"`
	Admin struct {
		Email       string `yaml:"email"`
		DisplayName string `yaml:"display_name"`
		Locale      string `yaml:"locale"`
		secretField `yaml:",inline"`
	} `yaml:"admin"`
	System struct {
		RunMode       string `yaml:"run_mode"`
		BaseURL       string `yaml:"base_url"`
		SiteName      string `yaml:"site_name"`
		SiteNameEN    string `yaml:"site_name_en"`
		Timezone      string `yaml:"timezone"`
		DefaultLocale string `yaml:"default_locale"`
	} `yaml:"system"`
}

// LoadFromFile 校验、读取并解析 path，成功后清零并删除该文件。residual 为真表示删除失败（文件仍在磁盘上）。
// 解析失败时**不**删除文件（让操作者修正后重试），但缓冲仍会清零。
func LoadFromFile(path string) (ff *FromFile, residual bool, err error) {
	f, err := openFromFile(path)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, 256<<10+1))
	if err != nil || len(raw) > 256<<10 {
		secretbox.Zero(raw)
		return nil, false, errors.New("from-file 读取失败或过大")
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var out FromFile
	if derr := dec.Decode(&out); derr != nil {
		secretbox.Zero(raw)
		return nil, false, errors.New("from-file 解析失败（YAML 语法错误或含未知键）")
	}
	secretbox.Zero(raw)
	if err := out.resolveSecretFiles(); err != nil {
		return nil, false, err
	}
	// 首次成功解析：用 0 覆盖后删除。
	if err := overwriteAndRemove(f, path); err != nil {
		return &out, true, nil
	}
	return &out, false, nil
}

func (f *FromFile) resolveSecretFiles() error {
	for name, sf := range map[string]*secretField{
		"database.admin": &f.Database.Admin.secretField, "database.migrate": &f.Database.Migrate.secretField,
		"database.app": &f.Database.App.secretField, "database.maint": &f.Database.Maint.secretField,
		"redis": &f.Redis.secretField, "admin": &f.Admin.secretField,
	} {
		if sf.Value != "" && sf.File != "" {
			return fmt.Errorf("from-file: %s 的 password 与 password_file 只能设置其一", name)
		}
		if sf.File != "" {
			s, err := config.ReadSecretFile(sf.File)
			if err != nil {
				return fmt.Errorf("from-file: %s.password_file: %w", name, err)
			}
			sf.Value, sf.File = s.Reveal(), ""
		}
	}
	return nil
}

func overwriteAndRemove(f *os.File, path string) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() > 0 {
		zeros := make([]byte, st.Size())
		if _, err := f.WriteAt(zeros, 0); err == nil {
			_ = f.Sync()
		}
	}
	return os.Remove(path)
}

// HeadlessStep 是无头结果里的一步。
type HeadlessStep struct {
	Step    string `json:"step"`
	Message string `json:"message"`
}

// HeadlessResult 是 setup --from-file 的结果（--json 时即 stdout 的 JSON）。口令只有 one_time_secrets 里自动生成的迁移口令，且只输出一次。
type HeadlessResult struct {
	OK               bool            `json:"ok"`
	Steps            []HeadlessStep  `json:"steps"`
	Failed           string          `json:"failed_step,omitempty"`
	Error            string          `json:"error,omitempty"`
	OneTimeSecrets   *OneTimeSecrets `json:"one_time_secrets,omitempty"`
	FromFileResidual bool            `json:"from_file_residual"`
	Warnings         []string        `json:"warnings,omitempty"`
}

func genOr(v string) (string, error) {
	if v != "" {
		return v, nil
	}
	return migrate.GeneratePassword()
}

// RunHeadless 依次执行全部步骤（每步幂等，失败即停并报告失败步骤；整个文件已被清理，续做须重新提供）。
func (s *Service) RunHeadless(ctx context.Context, ff *FromFile, residual bool) HeadlessResult {
	res := HeadlessResult{FromFileResidual: residual}
	if residual {
		res.Warnings = append(res.Warnings, "警告：from-file 文件未能删除，仍留在磁盘上（含口令）；请立即手工安全删除它")
	}
	fail := func(step string, err error) HeadlessResult {
		res.Failed = step
		res.Error = errText(err)
		return res
	}
	step := func(name string, r StepResult, err error) error {
		if err != nil {
			return err
		}
		res.Steps = append(res.Steps, HeadlessStep{Step: name, Message: r.Message})
		if r.Secrets != nil && r.Secrets.MigratePassword != "" {
			res.OneTimeSecrets = &OneTimeSecrets{MigratePassword: r.Secrets.MigratePassword}
		}
		return nil
	}
	if cr, err := s.Checks(ctx); err != nil {
		return fail(StepEnvironment, err)
	} else {
		for _, it := range cr.Items {
			if !it.OK && it.Severity == "error" {
				return fail(StepEnvironment, errors.New(it.Message))
			}
		}
		res.Steps = append(res.Steps, HeadlessStep{Step: StepEnvironment, Message: "环境检查通过"})
	}
	d := ff.Database
	in := DatabaseInput{Host: d.Host, Port: d.Port, Database: d.Name, Username: firstNonEmpty(d.App.User, "lingedge_app"), TLS: d.TLS,
		MigrateUsername: d.Migrate.User, MaintUsername: firstNonEmpty(d.Maint.User, ""), CreateIfMissing: d.Admin.User != "", AdminUsername: d.Admin.User, AdminPassword: d.Admin.Value}
	var err error
	if in.Password, err = genOr(d.App.Value); err != nil && in.CreateIfMissing {
		return fail(StepDatabase, err)
	}
	in.MigratePassword, in.MaintPassword = d.Migrate.Value, d.Maint.Value // 缺省：建账号模式由服务端生成
	if r, err := s.ConfigureDatabase(ctx, in); step(StepDatabase, r, err) != nil {
		return fail(StepDatabase, err)
	}
	rd := ff.Redis
	if r, err := s.ConfigureRedis(ctx, RedisInput{Address: rd.Addr, Password: rd.Value, DB: int32(rd.DB), TLS: rd.TLS}); step(StepRedis, r, err) != nil {
		return fail(StepRedis, err)
	}
	if r, err := s.InitializeDB(ctx); step(StepInitDB, r, err) != nil {
		return fail(StepInitDB, err)
	}
	a := ff.Admin
	if r, err := s.CreateAdmin(ctx, AdminInput{Email: a.Email, DisplayName: a.DisplayName, Password: a.Value, Locale: a.Locale}); step(StepAdmin, r, err) != nil {
		return fail(StepAdmin, err)
	}
	sy := ff.System
	in2 := SystemInput{RunMode: firstNonEmpty(sy.RunMode, "self_hosted"), BaseURL: sy.BaseURL, DefaultLocale: sy.DefaultLocale, Timezone: sy.Timezone}
	if sy.SiteName != "" || sy.SiteNameEN != "" {
		in2.Brand = &BrandInput{Name: sy.SiteName, NameEN: sy.SiteNameEN}
	}
	if r, err := s.ConfigureSystem(ctx, in2); step(StepSystem, r, err) != nil {
		return fail(StepSystem, err)
	}
	if r, err := s.InitKeys(ctx); step(StepKeys, r, err) != nil {
		return fail(StepKeys, err)
	}
	r, err := s.Complete(ctx)
	if err := step(StepComplete, r, err); err != nil {
		return fail(StepComplete, err)
	}
	s.AfterComplete()
	res.OK = true
	return res
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func errText(err error) string {
	var sb strings.Builder
	sb.WriteString(err.Error())
	return sb.String()
}

// JSON 返回结果的 JSON 文本。
func (r HeadlessResult) JSON() []byte {
	b, _ := json.MarshalIndent(r, "", "  ")
	return b
}
