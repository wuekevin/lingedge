package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/dbcred"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/migrate"
	"github.com/wuekevin/lingedge/internal/platform/signingkeys"
	"github.com/wuekevin/lingedge/internal/platform/sysconfig"
	"github.com/wuekevin/lingedge/pkg/version"
)

// ---- 步骤 4：数据库初始化 ----

// InitializeDB 是步骤 4：进程内调用与 `lingedge-server migrate up` 同一个 runner（ADR-026 §2），使用内存中的 migrate 凭据，逐个报告迁移分类；
// 结束即关闭 migrate 池（自动生成的口令保留到 complete 响应写出，其余清零）。
func (s *Service) InitializeDB(req context.Context) (StepResult, error) {
	ctx, cancel := s.opCtx(req, 30*time.Minute)
	defer cancel()
	return s.run(StepInitDB, func() (StepResult, error) { return s.initializeDB(ctx, req) })
}

func (s *Service) initializeDB(ctx, req context.Context) (StepResult, error) {
	if err := s.require(ctx, StepDatabase, StepRedis); err != nil {
		return StepResult{}, err
	}
	f, err := dbcred.Load(s.credPath())
	if err != nil || f.Database == nil {
		return StepResult{}, apierr.New(apierr.ResourceConflict, "database step not completed", apierr.Detail{Field: "step", Reason: "complete step database first"})
	}
	s.mu.Lock()
	user, pw := s.held.migrateUser, string(s.held.migrate)
	s.mu.Unlock()
	if pw == "" {
		// ADR-026 §6：口令不落盘 ⇒ 续做时重新提供，绝不「悄悄取回」。
		return StepResult{}, apierr.New(apierr.ResourceConflict, "migrate credentials are not available in memory",
			apierr.Detail{Field: "migrate_password", Reason: "the wizard process restarted since step database: resubmit PUT /public/setup/database with migrate_password, or with create_if_missing and the temporary administrator credentials to reset the migrate account"})
	}
	cfg := db.Config{Host: f.Database.Host, Port: f.Database.Port, Socket: f.Database.Socket, Name: f.Database.Name, User: user, Password: config.NewSecret(pw), TLS: f.Database.TLS}
	r, closeFn, err := s.window.OpenRunner(ctx, cfg, s.o.Migrations)
	if err != nil {
		return StepResult{}, apierr.New(apierr.SetupPrecheckFailed, "cannot connect with the migrate account", apierr.Detail{Reason: strings.ReplaceAll(err.Error(), pw, "***")})
	}
	defer closeFn()
	if _, err := r.CheckVersion(ctx); err != nil {
		return StepResult{}, precheck("database precheck failed", err.Error())
	}
	if err := r.Preflight(ctx); err != nil {
		return StepResult{}, precheck("database precheck failed", strings.ReplaceAll(err.Error(), pw, "***"))
	}
	res, err := r.Up(ctx, migrate.UpOptions{AcceptIrreversible: true}) // 全新空库：没有可丢失的数据，不可逆迁移无需额外确认
	if err != nil {
		var ae *apierr.Error
		if errors.As(err, &ae) {
			return StepResult{}, err
		}
		msg := strings.ReplaceAll(err.Error(), pw, "***")
		s.log.Error("向导：数据库迁移失败", zap.String("error", msg))
		return StepResult{}, apierr.New(apierr.ResourceConflict, "database migration failed", apierr.Detail{Reason: msg + "（可重试 initialize；若迁移被标记为 dirty，请先在 root 侧用 lingedge-server migrate recover 处理，ADR-016）"})
	}
	closeFn()
	// 迁移池用毕：口令不再需要。自动生成的保留到 complete 响应写出。
	s.mu.Lock()
	if !s.held.generated {
		s.held.zeroMigrate()
	}
	s.mu.Unlock()
	p, err := s.ensurePost(ctx)
	if err != nil {
		return StepResult{}, apierr.Internal(fmt.Errorf("迁移后无法以 app 账号连接: %w", err))
	}
	ms, perr := httpx.MigrationProbeFromApp(p.app, s.migrationSet()).MigrationState(ctx)
	if perr != nil || ms.Problem != "" || ms.Pending != 0 {
		return StepResult{}, apierr.Internal(fmt.Errorf("迁移后校验未通过: err=%v problem=%s pending=%d", perr, ms.Problem, ms.Pending))
	}
	var lines []string
	applied := make([]map[string]any, 0, len(res.Applied))
	for _, a := range res.Applied {
		lines = append(lines, fmt.Sprintf("%06d_%s（%s，%d ms）", a.Version, a.Name, a.Class, a.Millis))
		applied = append(applied, map[string]any{"version": a.Version, "name": a.Name, "class": string(a.Class)})
	}
	msg := fmt.Sprintf("迁移完成：%06d → %06d，本次应用 %d 个", res.FromVersion, res.ToVersion, len(res.Applied))
	if len(lines) > 0 {
		msg += "：" + strings.Join(lines, "；")
	} else {
		msg = fmt.Sprintf("数据库已是最新版本 %06d（无待执行迁移）", res.ToVersion)
	}
	ip := clientIP(req)
	ev := map[string]any{"from_version": res.FromVersion, "to_version": res.ToVersion, "applied": applied}
	if err := s.mutateMeta(ctx, p, func(m *setupMeta) {
		m.Steps[StepInitDB] = stepMeta{Done: true, At: s.clk.Now().UTC().Format(time.RFC3339), Detail: fmt.Sprintf("迁移已应用到 %06d", res.ToVersion)}
		m.MigrateGenerated = s.heldGenerated()
	}, func(ctx context.Context) error {
		if err := s.flushPending(ctx, p); err != nil { // 迁移之前缓冲的 checks/database/redis 审计事件，与本步骤同一事务补写
			return err
		}
		return recordAudit(ctx, p.audit, StepInitDB, auditEvent(StepInitDB, ev, ip))
	}); err != nil {
		return StepResult{}, apierr.Internal(err)
	}
	s.clearPending()
	s.log.Info("向导：数据库迁移完成", zap.Int64("to_version", res.ToVersion), zap.Int("applied", len(res.Applied)))
	out := s.result(StepInitDB, msg)
	infos := make([]AppliedMigrationInfo, 0, len(res.Applied))
	for _, a := range res.Applied {
		infos = append(infos, AppliedMigrationInfo{Version: a.Version, Name: a.Name, Category: string(a.Class), Millis: a.Millis})
	}
	out.Migrations = &infos
	return out, nil
}

func (s *Service) heldGenerated() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held.generated
}

// ---- 步骤 5：创建管理员 ----

// CreateAdmin 是步骤 5：口令由安装者当场设定（强度校验，无任何默认口令），授予超级管理员角色；可选绑定 TOTP（enable_totp=true 先取得密钥，带 totp_code 再提交确认）。
func (s *Service) CreateAdmin(req context.Context, in AdminInput) (StepResult, error) {
	ctx, cancel := s.opCtx(req, 5*time.Minute)
	defer cancel()
	res, err := s.run(StepAdmin, func() (StepResult, error) { return s.createAdmin(ctx, req, in) })
	in.Password = ""
	return res, err
}

func (s *Service) createAdmin(ctx, req context.Context, in AdminInput) (StepResult, error) {
	var ds []apierr.Detail
	if strings.TrimSpace(in.Email) == "" {
		ds = append(ds, invalidField("email", "required"))
	}
	if strings.TrimSpace(in.DisplayName) == "" {
		ds = append(ds, invalidField("display_name", "required"))
	}
	if in.Password == "" {
		ds = append(ds, invalidField("password", "required"))
	}
	if in.TOTPCode != "" && !in.EnableTOTP {
		ds = append(ds, invalidField("enable_totp", "must be true when totp_code is provided"))
	}
	if len(ds) > 0 {
		return StepResult{}, apierr.Validation(ds...)
	}
	if err := s.require(ctx, StepInitDB); err != nil {
		return StepResult{}, err
	}
	p, err := s.ensurePost(ctx)
	if err != nil {
		return StepResult{}, apierr.Internal(err)
	}
	if p.admin == nil {
		return StepResult{}, apierr.Internal(errors.New("setup: 管理员服务未装配"))
	}
	created, err := p.admin.CreateAdmin(ctx, AdminCreate{Email: in.Email, DisplayName: in.DisplayName, Password: in.Password, Locale: in.Locale})
	if err != nil {
		return StepResult{}, err
	}
	res := s.result(StepAdmin, "")
	switch {
	case in.EnableTOTP && in.TOTPCode == "":
		en, err := p.admin.BeginTOTP(ctx, created.UserUID)
		if err != nil {
			return StepResult{}, err
		}
		res.Secrets = &OneTimeSecrets{TOTPSecret: en.Secret, TOTPOtpauthURI: en.OTPAuthURI}
		res.Message = "管理员已创建；请用验证器扫描二维码（或手输密钥），再次提交本步骤并附 totp_code 以启用 TOTP（密钥仅此一次展示）"
	case in.EnableTOTP:
		codes, err := p.admin.ConfirmTOTP(ctx, created.UserUID, in.TOTPCode)
		if err != nil {
			return StepResult{}, err
		}
		res.Secrets = &OneTimeSecrets{RecoveryCodes: codes}
		res.Message = "TOTP 已启用；恢复码仅此一次展示，请妥善保存"
	case created.Created:
		res.Message = "管理员已创建（口令由你设定，系统不存在任何默认口令）"
	default:
		res.Message = "管理员已存在，未做任何改动"
	}
	if err := s.markStep(ctx, p, StepAdmin, "管理员已创建（uid="+created.UserUID+"）", nil); err != nil {
		return StepResult{}, apierr.Internal(err)
	}
	return res, nil
}

// ---- 步骤 6：系统基础配置 ----

// ConfigureSystem 是步骤 6：站点名称、公开 URL、运行模式（D-3）、时区、缺省语言；走系统设置服务（校验、审计、信封加密同一套）。
func (s *Service) ConfigureSystem(req context.Context, in SystemInput) (StepResult, error) {
	ctx, cancel := s.opCtx(req, 2*time.Minute)
	defer cancel()
	return s.run(StepSystem, func() (StepResult, error) { return s.configureSystem(ctx, in) })
}

func (s *Service) configureSystem(ctx context.Context, in SystemInput) (StepResult, error) {
	if in.RunMode == "" {
		return StepResult{}, apierr.Validation(invalidField("run_mode", "required (self_hosted or commercial)"))
	}
	if err := s.require(ctx, StepAdmin); err != nil {
		return StepResult{}, err
	}
	p, err := s.ensurePost(ctx)
	if err != nil {
		return StepResult{}, apierr.Internal(err)
	}
	is := sysconfig.InitialSettings{RunMode: in.RunMode, BaseURL: strings.TrimSpace(in.BaseURL), Timezone: in.Timezone, DefaultLocale: in.DefaultLocale}
	if in.Brand != nil {
		is.BrandName, is.BrandNameEN = in.Brand.Name, in.Brand.NameEN
	}
	if err := p.sys.Service().ApplyInitial(ctx, is); err != nil {
		return StepResult{}, err
	}
	if err := s.markStep(ctx, p, StepSystem, "运行模式 "+in.RunMode, func(m *setupMeta) {
		if is.BaseURL != "" {
			m.BaseURL = is.BaseURL
		}
	}); err != nil {
		return StepResult{}, apierr.Internal(err)
	}
	return s.result(StepSystem, "系统基础配置已写入（运行模式 "+in.RunMode+"）"), nil
}

// ---- 步骤 7：内部身份与密钥初始化 ----

// InitKeys 是步骤 7：调用 Story 1.23 的 signingkeys.Initialize（私有 CA 10 年、配置签名密钥、Server gRPC 叶证书、deploy.lineage=1；幂等、并发安全）。
func (s *Service) InitKeys(req context.Context) (StepResult, error) {
	ctx, cancel := s.opCtx(req, 5*time.Minute)
	defer cancel()
	return s.run(StepKeys, func() (StepResult, error) { return s.initKeys(ctx, req) })
}

func (s *Service) initKeys(ctx, req context.Context) (StepResult, error) {
	if err := s.require(ctx, StepSystem); err != nil {
		return StepResult{}, err
	}
	p, err := s.ensurePost(ctx)
	if err != nil {
		return StepResult{}, apierr.Internal(err)
	}
	row, found, err := p.repo.Get(ctx, false)
	if err != nil || !found {
		return StepResult{}, apierr.Internal(fmt.Errorf("读取安装进度: %v", err))
	}
	base := parseMeta(row.Meta).BaseURL
	if base == "" {
		return StepResult{}, apierr.New(apierr.ResourceConflict, "base_url is not configured", apierr.Detail{Field: "base_url", Reason: "submit the system step with base_url first: the gRPC leaf certificate SAN is derived from it"})
	}
	r, err := p.keys.Initialize(ctx, signingkeys.InitOptions{ServerURL: base})
	switch {
	case errors.Is(err, signingkeys.ErrInvalidServerURL):
		return StepResult{}, apierr.Validation(invalidField("base_url", "the stored base_url is not a valid URL with a host name or IP; resubmit the system step"))
	case errors.Is(err, signingkeys.ErrMasterKeyMissing):
		return StepResult{}, precheck("master key unavailable", "主密钥不可用，无法加密保存私钥")
	case err != nil:
		return StepResult{}, apierr.Internal(err)
	}
	created := make([]string, 0, len(r.Created))
	for _, c := range r.Created {
		created = append(created, string(c))
	}
	if err := s.audit(ctx, StepKeys, map[string]any{"created": created, "lineage_initialized": r.LineageInitialized, "ca_fingerprint": r.CAFingerprint()}, req); err != nil {
		return StepResult{}, apierr.Internal(err)
	}
	if err := s.markStep(ctx, p, StepKeys, "CA 指纹 "+r.CAFingerprint(), nil); err != nil {
		return StepResult{}, apierr.Internal(err)
	}
	return s.result(StepKeys, "内部身份与密钥已初始化（私有 CA、配置签名密钥、Server gRPC 叶证书、deploy.lineage）；CA 指纹 "+r.CAFingerprint()+
		"。请把主密钥 master.key 与数据库备份分开保管（丢失主密钥将无法解密任何私钥，F-7）"), nil
}

// ---- 步骤 8：完成 ----

// Complete 是步骤 8：校验全部前置步骤 → 单事务写 system_installations(install_locked_at)（DB 层保证并发两个 complete 仅一个成功）与审计 →
// 事务提交后**最后**写 install.lock → 删除 setup token 哈希 → 关闭 migrate 窗口。此后 /public/setup/* 永久 410。
// 自动生成的迁移口令随结果一次性返回；调用方写出响应后必须调用 AfterComplete 清零。
func (s *Service) Complete(req context.Context) (StepResult, error) {
	ctx, cancel := s.opCtx(req, 2*time.Minute)
	defer cancel()
	return s.run(StepComplete, func() (StepResult, error) { return s.complete(ctx, req) })
}

func (s *Service) complete(ctx, req context.Context) (StepResult, error) {
	if err := s.require(ctx, StepDatabase, StepRedis, StepInitDB, StepAdmin, StepSystem, StepKeys); err != nil {
		return StepResult{}, err
	}
	p, err := s.ensurePost(ctx)
	if err != nil {
		return StepResult{}, apierr.Internal(err)
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "localhost"
	}
	ip := clientIP(req)
	now := s.clk.Now()
	err = p.app.WithTx(ctx, func(ctx context.Context) error {
		row, found, err := p.repo.Get(ctx, true) // 行锁：并发的第二个 complete 在此等待，提交后看到已锁定而得到 410
		if err != nil {
			return err
		}
		if !found {
			return apierr.New(apierr.ResourceConflict, "setup progress not found", apierr.Detail{Field: "step", Reason: "complete the previous steps first"})
		}
		if row.InstallLocked != nil {
			return apierr.New(apierr.SetupAlreadyInstalled, "setup already completed")
		}
		m := parseMeta(row.Meta)
		m.Steps[StepComplete] = stepMeta{Done: true, At: now.UTC().Format(time.RFC3339)}
		ok, err := p.repo.Complete(ctx, row.ID, host, version.Version, now, m.encode())
		if err != nil {
			return err
		}
		if !ok {
			return apierr.New(apierr.SetupAlreadyInstalled, "setup already completed")
		}
		return recordAudit(ctx, p.audit, StepComplete, auditEvent(StepComplete, map[string]any{"installed_at": now.UTC().Format(time.RFC3339), "version": version.Version, "migrate_generated": m.MigrateGenerated}, ip))
	})
	if err != nil {
		return StepResult{}, err
	}
	// ---- 事务已提交：以下为「最后」的动作 ----
	msg := "安装已完成"
	if err := writeLockFile(s.o.LockPath, now); err != nil {
		s.log.Error("写入 install.lock 失败（数据库已记录安装完成，下次启动会据此自愈并补写）", zap.Error(err))
		msg += "；警告：install.lock 写入失败，请检查数据目录权限，重启 Server 时会据数据库记录补写"
	}
	s.installed.Store(true)
	if removed, err := removeTokenHash(s.o.TokenHashPath); err != nil {
		s.log.Error("删除 setup token 哈希文件失败", zap.Error(err))
		msg += "；警告：setup token 哈希文件删除失败，请手工删除 " + s.o.TokenHashPath
	} else if !removed {
		s.log.Warn("setup token 哈希文件所在目录不可写，已清空其内容使其失效（fail-closed）；请手工删除该文件")
		msg += "；setup token 哈希文件所在目录不可写，已清空其内容使 token 永久失效，请手工删除 " + s.o.TokenHashPath
	}
	s.window.Close()
	res := s.result(StepComplete, msg+"。向导接口此后永久返回 410；请以刚创建的管理员登录 Admin。")
	s.mu.Lock()
	if s.held.generated && len(s.held.migrate) > 0 {
		res.Secrets = &OneTimeSecrets{MigratePassword: string(s.held.migrate)}
		res.Message += " 迁移账号口令由向导自动生成，仅在本响应展示一次，不会被保存：请立即在 root 侧执行 `sudo lingedge-server migrate credential init` 保存（预填 host/port/database，只需键入口令）；若来不及保存，之后用 `migrate credential reset --admin-user <管理员>` 重置。临时数据库管理员账号请自行删除。"
	} else {
		res.Message += " 升级前请在 root 侧执行 `sudo lingedge-server migrate credential init` 保存迁移账号口令（或之后用 credential reset）；临时数据库管理员账号请自行删除。"
	}
	s.mu.Unlock()
	return res, nil
}

// AfterComplete 在 complete 的响应已写出后调用：清零内存中的口令、关闭向导持有的 app 池与 Redis，并通知 server 退出 setup mode。幂等。
func (s *Service) AfterComplete() {
	s.mu.Lock()
	s.held.zeroMigrate()
	s.mu.Unlock()
	s.doneOnce.Do(func() {
		s.mu.Lock()
		p := s.post
		s.post = nil
		s.mu.Unlock()
		if p != nil {
			p.close()
		}
		if s.o.OnComplete != nil {
			s.o.OnComplete()
		}
		close(s.done)
	})
}

// removeTokenHash 删除 setup token 哈希文件。向导以 lingedge 用户运行、而 /etc/lingedge/server 属主是 root 时无权 unlink：此时退而清空文件内容
// （属主可写，tools-design §13：lingedge:lingedge 0600），空文件对 setuptoken.ReadHashFile 是非法格式，所有 token 校验 fail-closed——token 同样永久失效。
// 返回 removed=false 表示只做到了「清空使其失效」。
func removeTokenHash(path string) (removed bool, err error) {
	err = os.Remove(path)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	f, oerr := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if oerr != nil {
		return false, err
	}
	_ = f.Close()
	return false, nil
}

// writeLockFile 原子写 install.lock（内容只含完成时间与版本，不含任何机密）。
func writeLockFile(path string, at time.Time) error {
	b, _ := json.Marshal(map[string]string{"installed_at": at.UTC().Format(time.RFC3339), "version": version.Version})
	tmp, err := os.CreateTemp(filepath.Dir(path), ".install-lock-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(err error) error { _ = tmp.Close(); _ = os.Remove(name); return err }
	if err := tmp.Chmod(0o644); err != nil {
		return fail(err)
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// WriteLockFile 供 server 在「数据库记录已完成但 install.lock 缺失」时自愈补写。
func WriteLockFile(path string, at time.Time) error { return writeLockFile(path, at) }
