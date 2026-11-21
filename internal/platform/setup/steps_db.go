package setup

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/dbcred"
	"github.com/wuekevin/lingedge/internal/platform/migrate"
	"github.com/wuekevin/lingedge/internal/platform/migrate/provision"
	"github.com/wuekevin/lingedge/internal/platform/redisx"
	"github.com/wuekevin/lingedge/pkg/version"
)

func precheck(msg string, reasons ...string) error {
	ds := make([]apierr.Detail, 0, len(reasons))
	for _, r := range reasons {
		ds = append(ds, apierr.Detail{Reason: r})
	}
	return apierr.New(apierr.SetupPrecheckFailed, msg, ds...)
}

func invalidField(field, reason string) apierr.Detail {
	return apierr.Detail{Field: field, Reason: reason}
}

// ---- 环境检查 ----

// Checks 是步骤 1「环境检查」：Server 版本、系统时间、数据目录可写、master.key 可读（不展示内容）、迁移集合、监听与传输安全、setup token。
func (s *Service) Checks(req context.Context) (CheckResult, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if s.Installed() {
		return CheckResult{}, apierr.New(apierr.SetupAlreadyInstalled, "setup already completed")
	}
	var items []CheckItem
	add := func(key string, ok bool, sev, msg string) {
		items = append(items, CheckItem{Key: key, OK: ok, Severity: sev, Message: msg})
	}
	add("server.version", true, "info", "lingedge-server "+version.Version)
	now := s.clk.Now().UTC()
	if now.Year() < 2024 {
		add("system.time", false, "error", "系统时间异常（"+now.Format(time.RFC3339)+"）：证书与会话依赖正确的时钟，请先校准时间")
	} else {
		add("system.time", true, "info", "系统时间 "+now.Format(time.RFC3339)+"（UTC）")
	}
	if f, err := os.CreateTemp(s.cfg.Server.DataDir, ".setup-check-*"); err != nil {
		add("data_dir.writable", false, "error", "数据目录不可写（"+s.cfg.Server.DataDir+"）：请确认目录存在且属主为运行用户")
	} else {
		_ = f.Close()
		_ = os.Remove(f.Name())
		add("data_dir.writable", true, "info", "数据目录可写")
	}
	if s.o.Box == nil {
		add("master_key.readable", false, "error", "主密钥不可读：安装步骤 18 应已生成 /etc/lingedge/server/master.key 并经 systemd LoadCredential 提供（不展示内容）")
	} else {
		add("master_key.readable", true, "info", "主密钥可读（不展示内容）")
	}
	set, findings, err := migrate.Load(s.o.Migrations)
	if err != nil || len(findings) > 0 {
		add("migrations.embedded", false, "error", "内嵌迁移集合无效（构建缺陷）")
	} else {
		add("migrations.embedded", true, "info", fmt.Sprintf("内嵌迁移 %d 个", len(set.Migrations)))
	}
	if s.cfg.Server.Env == "" {
		add("server.env", false, "warn", "server.env 未配置：正常运行前必须配置（Redis 键空间 le:<env>:…）")
	} else {
		add("server.env", true, "info", "server.env 已配置")
	}
	if config.IsLoopbackListen(s.o.ListenAddr) {
		add("listen.loopback", true, "info", "向导仅监听回环地址 "+s.o.ListenAddr+"（远程访问请用 SSH 隧道）")
	} else {
		add("listen.loopback", true, "warn", "向导监听非回环地址 "+s.o.ListenAddr+"：携带口令的接口须经 TLS（直接 TLS 或可信反代的 X-Forwarded-Proto: https），否则返回 400 setup.insecure_transport")
	}
	if _, err := os.Stat(s.o.TokenHashPath); err != nil {
		add("setup_token.configured", false, "warn", "setup token 哈希文件不存在（"+s.o.TokenHashPath+"）：Web 向导的所有写接口将一律 401（由 Tools 生成）；无头路径 setup --from-file 不需要它")
	} else {
		add("setup_token.configured", true, "info", "setup token 已配置")
	}
	ok := true
	for _, it := range items {
		if !it.OK && it.Severity == "error" {
			ok = false
		}
	}
	s.mu.Lock()
	s.envOK = ok
	s.mu.Unlock()
	if ok {
		s.setProgress(StepEnvironment, StatusDone, "环境检查已通过")
	} else {
		s.setProgress(StepEnvironment, StatusFailed, "环境检查未通过")
	}
	ctx, cancel := s.opCtx(req, time.Minute)
	defer cancel()
	_ = s.audit(ctx, "checks", map[string]any{"ok": ok, "items": len(items)}, req)
	return CheckResult{Items: items}, nil
}

// ---- MySQL ----

type dbPlan struct {
	tgt                         provision.Target
	host                        string
	port                        int
	name                        string
	appUser, maintUser, migUser string
	appPW, maintPW, migPW       string
	maintOn                     bool
	create                      bool
	adminUser, adminPW          string
	generatedMig                bool
	resumed                     bool
}

func validPW(field, pw string, strict bool) *apierr.Detail {
	switch {
	case pw == "":
		return &apierr.Detail{Field: field, Reason: "required"}
	case len(pw) > 128 || strings.ContainsAny(pw, "\x00\r\n"):
		return &apierr.Detail{Field: field, Reason: "must be at most 128 bytes without NUL or line breaks"}
	case strict && utf8.RuneCountInString(pw) < 12:
		return &apierr.Detail{Field: field, Reason: "must be at least 12 characters"}
	}
	return nil
}

func (s *Service) planDatabase(in DatabaseInput) (*dbPlan, error) {
	p := &dbPlan{host: strings.TrimSpace(in.Host), port: in.Port, name: in.Database, appUser: in.Username, maintUser: in.MaintUsername, migUser: in.MigrateUsername,
		appPW: in.Password, maintPW: in.MaintPassword, migPW: in.MigratePassword, create: in.CreateIfMissing, adminUser: in.AdminUsername, adminPW: in.AdminPassword}
	var ds []apierr.Detail
	bad := func(f, r string) { ds = append(ds, invalidField(f, r)) }
	if p.migUser == "" {
		p.migUser = provision.MigrateUser
	}
	if p.maintUser == "" {
		p.maintUser = provision.MaintUser
	}
	if p.host == "" || len(p.host) > 255 {
		bad("host", "must be a host name or IP")
	}
	if p.port < 1 || p.port > 65535 {
		bad("port", "must be between 1 and 65535")
	}
	if !provision.ValidDatabaseName(p.name) {
		bad("database", "must be a MySQL identifier (letters, digits, _, $; at most 64)")
	}
	// 迁移 SQL 里的授权对象固定为 lingedge_app / lingedge_maint（迁移 lint 只允许授权给这两个账号），自定义名称无法获得任何表权限。
	if p.appUser != provision.AppUser {
		bad("username", "must be "+provision.AppUser+" (the migrations grant table privileges to this fixed account name)")
	}
	if p.maintUser != provision.MaintUser {
		bad("maint_username", "must be "+provision.MaintUser+" (fixed by the migrations)")
	}
	if !provision.ValidUserName(p.migUser) {
		bad("migrate_username", "must be 1-32 characters of letters, digits, _, . or -")
	}
	if p.migUser == p.appUser || p.migUser == p.maintUser {
		bad("migrate_username", "must differ from the app and maint accounts")
	}
	switch {
	case in.TLS:
		p.tgt.TLS = "true"
	case p.host != "" && !isLocalHost(p.host):
		bad("tls", "must be true for a non-loopback database host (credentials and data must not cross the network in clear text)")
	}
	p.tgt.Host, p.tgt.Port = p.host, p.port
	if p.create {
		// 向导只创建 lingedge_* 专用库与账号，绝不触碰其他库（宝塔场景同样，tools-design §13）；系统库名一律拒绝。
		if p.name != "lingedge" && !strings.HasPrefix(p.name, "lingedge_") {
			bad("database", "the wizard only creates dedicated databases named lingedge or lingedge_* (it never touches other databases); use create_if_missing=false for an existing empty database")
		}
		if p.adminUser == "" {
			bad("admin_username", "required when create_if_missing is true")
		} else if !provision.ValidUserName(p.adminUser) {
			bad("admin_username", "must be 1-32 characters of letters, digits, _, . or -")
		}
		if d := validPW("admin_password", p.adminPW, false); d != nil {
			ds = append(ds, *d)
		}
		p.maintOn = true
	} else {
		if d := validPW("migrate_password", p.migPW, false); d != nil && !s.hasHeldMigrate(p.migUser) {
			ds = append(ds, *d)
		}
		p.maintOn = p.maintPW != ""
	}
	if d := validPW("password", p.appPW, p.create); d != nil {
		ds = append(ds, *d)
	}
	if p.maintPW != "" {
		if d := validPW("maint_password", p.maintPW, p.create); d != nil {
			ds = append(ds, *d)
		}
	}
	if p.create && p.migPW != "" {
		if d := validPW("migrate_password", p.migPW, true); d != nil {
			ds = append(ds, *d)
		}
	}
	if len(ds) > 0 {
		return nil, apierr.Validation(ds...)
	}
	return p, nil
}

func isLocalHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func (s *Service) hasHeldMigrate(user string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.held.migrate) > 0 && s.held.migrateUser == user
}

// ConfigureDatabase 是步骤 2「MySQL」（见包注释与 ADR-026 §5）。两种模式：建账号模式（create_if_missing=true，临时管理员凭据仅内存使用）与预建账号模式。
func (s *Service) ConfigureDatabase(req context.Context, in DatabaseInput) (StepResult, error) {
	ctx, cancel := s.opCtx(req, 10*time.Minute)
	defer cancel()
	res, err := s.run(StepDatabase, func() (StepResult, error) { return s.configureDatabase(ctx, req, in) })
	in.AdminPassword, in.MigratePassword, in.Password, in.MaintPassword = "", "", "", ""
	return res, err
}

func (s *Service) configureDatabase(ctx, req context.Context, in DatabaseInput) (StepResult, error) {
	if s.o.Box == nil {
		return StepResult{}, precheck("master key unavailable", "主密钥不可用：没有 KEK 就不能加密 app/maint 口令（ADR-026 §3）。请确认 /etc/lingedge/server/master.key 已由 Tools 生成并可读")
	}
	p, err := s.planDatabase(in)
	if err != nil {
		return StepResult{}, err
	}
	path := s.credPath()
	// 续做（ADR-026 §6）：凭据文件已有同一目标的 database 段 → app/maint 口令读回，不再询问，也不改动。
	if f, lerr := dbcred.Load(path); lerr == nil && f.Database != nil && f.Database.Host == p.host && f.Database.Port == p.port && f.Database.Name == p.name {
		if pw, err := f.AppPassword(ctx, s.o.Box); err == nil {
			p.appPW, p.resumed = pw.Reveal(), true
		}
		if mp, ok, err := f.MaintPassword(ctx, s.o.Box); err == nil && ok {
			p.maintPW, p.maintOn = mp.Reveal(), true
		}
	} else if lerr != nil && !errors.Is(lerr, dbcred.ErrNotFound) {
		return StepResult{}, precheck("runtime credentials file invalid", "运行期凭据文件无效，不会覆盖："+lerr.Error())
	}
	// 迁移口令：请求给出 > 本进程内存中保留的 > （建账号模式）自动生成。
	s.mu.Lock()
	switch {
	case p.migPW != "":
		s.held.zeroMigrate()
	case len(s.held.migrate) > 0 && s.held.migrateUser == p.migUser:
		p.migPW, p.generatedMig = string(s.held.migrate), s.held.generated
	}
	s.mu.Unlock()
	if p.migPW == "" && p.create {
		gen, err := migrate.GeneratePassword()
		if err != nil {
			return StepResult{}, apierr.Internal(err)
		}
		p.migPW, p.generatedMig = gen, true
	}
	if p.create && p.maintPW == "" {
		gen, err := migrate.GeneratePassword()
		if err != nil {
			return StepResult{}, apierr.Internal(err)
		}
		p.maintPW = gen
	}
	mode := "prebuilt_accounts"
	var created []string
	warn := ""
	if p.create {
		mode = "create_accounts"
		r, err := s.createAccounts(ctx, p)
		if err != nil {
			return StepResult{}, err
		}
		created = []string{p.appUser, p.migUser, p.maintUser}
		if !r.Granted {
			warn = "（管理员无法授予全局 SET_USER_ID，已交由触发器探针判定是否需要）"
		}
	}
	if err := s.verifyDatabase(ctx, p); err != nil {
		return StepResult{}, err
	}
	if err := dbcred.WriteDatabase(ctx, path, s.o.Box, dbcred.DatabaseInput{Host: p.host, Port: p.port, Name: p.name, TLS: p.tgt.TLS,
		AppUser: p.appUser, AppPass: config.NewSecret(p.appPW), MaintUser: map[bool]string{true: p.maintUser}[p.maintOn], MaintPass: config.NewSecret(p.maintPW)}); err != nil {
		return StepResult{}, apierr.Internal(err)
	}
	// 迁移口令留在内存供步骤 4 使用（步骤结束清零；自动生成的在 complete 响应里一次性返回）。
	s.mu.Lock()
	s.held.zeroMigrate()
	s.held.migrate, s.held.migrateUser, s.held.generated = []byte(p.migPW), p.migUser, p.generatedMig
	s.mu.Unlock()
	// 凭据文件的 database 段变了：丢弃旧的 app 池与服务，下次按新凭据重建。
	s.dropPost()
	changes := map[string]any{"mode": mode, "host": p.host, "port": p.port, "database": p.name, "migrate_user": p.migUser, "migrate_generated": p.generatedMig,
		"maint_configured": p.maintOn, "runtime_credentials_written": true, "resumed": p.resumed}
	if created != nil {
		changes["accounts_created"] = created
	}
	if err := s.audit(ctx, StepDatabase, changes, req); err != nil {
		return StepResult{}, apierr.Internal(err)
	}
	msg := "MySQL 预检通过；app" + map[bool]string{true: "/maint"}[p.maintOn] + " 口令已加密写入运行期凭据文件（迁移口令仅内存）" + warn
	if !p.maintOn {
		msg += "。未配置 maint 账号：分区维护作业将明确跳过并告警"
	}
	s.log.Info("向导：MySQL 步骤完成", zap.String("mode", mode), zap.String("database", p.name))
	return s.result(StepDatabase, msg), nil
}

func (s *Service) dropPost() {
	s.mu.Lock()
	p := s.post
	s.post = nil
	s.mu.Unlock()
	if p != nil {
		p.close()
	}
}

func provErr(err error) error {
	var pe *provision.Error
	switch {
	case errors.As(err, &pe):
		return precheck("database precheck failed", pe.Error())
	case errors.Is(err, provision.ErrAccountInUse):
		return precheck("database precheck failed", err.Error())
	}
	return precheck("database precheck failed", err.Error())
}

// createAccounts 是建账号模式：管理员连接 → 预检 → 库（空）→ 三类账号。管理员口令用后立即清零引用。
func (s *Service) createAccounts(ctx context.Context, p *dbPlan) (provision.SetUserIDResult, error) {
	var none provision.SetUserIDResult
	adm, err := provision.Open(ctx, p.tgt, p.adminUser, config.NewSecret(p.adminPW), "")
	if err != nil {
		return none, provErr(err)
	}
	defer adm.Close()
	defer func() { p.adminPW = "" }()
	facts, err := adm.Facts(ctx)
	if err != nil {
		return none, provErr(err)
	}
	if err := factsError(facts); err != nil {
		return none, err
	}
	exists, err := adm.DatabaseExists(ctx, p.name)
	if err != nil {
		return none, provErr(err)
	}
	if exists {
		if err := s.checkEmpty(ctx, adm, p.name); err != nil {
			return none, err
		}
	} else if err := adm.CreateDatabase(ctx, p.name); err != nil {
		return none, provErr(err)
	}
	r, err := adm.CreateAccounts(ctx, provision.AccountSpec{Database: p.name, AppUser: p.appUser, MaintUser: p.maintUser, MigrateUser: p.migUser,
		AppPass: config.NewSecret(p.appPW), MaintPass: config.NewSecret(p.maintPW), MigratePass: config.NewSecret(p.migPW), AllowPreexistingAccounts: true})
	if err != nil {
		return none, provErr(err)
	}
	return r, nil
}

func factsError(f provision.Facts) error {
	var reasons []string
	for _, it := range f.Check() {
		if !it.OK && it.Severity == "error" {
			reasons = append(reasons, it.Message)
		}
	}
	if len(reasons) > 0 {
		return precheck("database precheck failed", reasons...)
	}
	return nil
}

// checkEmpty 实现「目标库必须为空」（FR-6）：非空且不是本向导中断留下的进行中安装 → 400 setup.database_not_empty；库字符集必须是 utf8mb4。
func (s *Service) checkEmpty(ctx context.Context, c *provision.Conn, name string) error {
	n, err := c.TableCount(ctx, name)
	if err != nil {
		return provErr(err)
	}
	if n > 0 {
		inprog, err := c.InProgressInstall(ctx, name)
		if err != nil {
			return provErr(err)
		}
		if !inprog {
			return apierr.New(apierr.SetupDatabaseNotEmpty, "target database is not empty", apierr.Detail{Field: "database", Reason: fmt.Sprintf("database %q already contains %d table(s); the wizard never overwrites an existing database (FR-6). Use an empty dedicated database", name, n)})
		}
		s.log.Info("目标库非空，但属于本向导中断的安装：允许续做", zap.String("database", name))
	}
	cs, err := c.DatabaseCharset(ctx, name)
	if err != nil {
		return provErr(err)
	}
	if !strings.EqualFold(cs, "utf8mb4") {
		return precheck("database precheck failed", fmt.Sprintf("目标库默认字符集是 %s，必须是 utf8mb4（可 ALTER DATABASE %s CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci 后重试）", cs, name))
	}
	return nil
}

// verifyDatabase 用 migrate 凭据做预检：连接 → 事实 → 空库 → 版本 → GRANT OPTION 与触发器探针（ERROR 1419 阻断并给两种处置）；再验证 app/maint 能登录。
func (s *Service) verifyDatabase(ctx context.Context, p *dbPlan) error {
	mc, err := provision.Open(ctx, p.tgt, p.migUser, config.NewSecret(p.migPW), p.name)
	if err != nil {
		var pe *provision.Error
		if errors.As(err, &pe) && pe.Number == 1049 {
			return precheck("database precheck failed", "目标库 "+p.name+" 不存在：请先创建（或使用 create_if_missing 与临时管理员凭据由向导创建）")
		}
		return provErr(err)
	}
	facts, ferr := mc.Facts(ctx)
	if ferr == nil {
		ferr = factsError(facts)
	}
	if ferr != nil {
		mc.Close()
		var ae *apierr.Error
		if errors.As(ferr, &ae) {
			return ferr
		}
		return provErr(ferr)
	}
	if !p.create {
		if err := s.checkEmpty(ctx, mc, p.name); err != nil {
			mc.Close()
			return err
		}
	}
	mc.Close()
	cfg := db.Config{Host: p.host, Port: p.port, Name: p.name, User: p.migUser, Password: config.NewSecret(p.migPW), TLS: p.tgt.TLS}
	r, closeFn, err := s.window.OpenRunner(ctx, cfg, s.o.Migrations)
	if err != nil {
		return precheck("database precheck failed", "无法以迁移账号建立连接池："+scrubErr(err, p))
	}
	defer closeFn()
	if _, err := r.CheckVersion(ctx); err != nil {
		return precheck("database precheck failed", err.Error())
	}
	if err := r.Preflight(ctx); err != nil {
		return precheck("database precheck failed", scrubErr(err, p))
	}
	for _, u := range []struct{ user, pw string }{{p.appUser, p.appPW}, {p.maintUser, p.maintPW}} {
		if u.user == p.maintUser && !p.maintOn {
			continue
		}
		c, err := provision.Open(ctx, p.tgt, u.user, config.NewSecret(u.pw), "")
		if err != nil {
			return provErr(err)
		}
		perr := c.Ping(ctx)
		c.Close()
		if perr != nil {
			return provErr(perr)
		}
	}
	return nil
}

// scrubErr 把已知口令从错误文本里抹掉。
func scrubErr(err error, p *dbPlan) string {
	msg := err.Error()
	for _, s := range []string{p.appPW, p.maintPW, p.migPW, p.adminPW} {
		if s != "" {
			msg = strings.ReplaceAll(msg, s, "***")
		}
	}
	return msg
}

// ---- Redis ----

// ConfigureRedis 是步骤 3「Redis」：INFO server 校验主版本 ≥8，不使用 FLUSHDB；成功后把 Redis 地址与口令加密写入运行期凭据文件。
func (s *Service) ConfigureRedis(req context.Context, in RedisInput) (StepResult, error) {
	ctx, cancel := s.opCtx(req, 2*time.Minute)
	defer cancel()
	res, err := s.run(StepRedis, func() (StepResult, error) { return s.configureRedis(ctx, req, in) })
	in.Password = ""
	return res, err
}

func (s *Service) configureRedis(ctx, req context.Context, in RedisInput) (StepResult, error) {
	if err := s.require(ctx, StepDatabase); err != nil {
		return StepResult{}, err
	}
	var ds []apierr.Detail
	if host, port, err := net.SplitHostPort(strings.TrimSpace(in.Address)); err != nil || host == "" || port == "" {
		ds = append(ds, invalidField("address", "must be <host>:<port>"))
	} else if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		ds = append(ds, invalidField("address", "port must be between 1 and 65535"))
	}
	if in.DB < 0 || in.DB > 15 {
		ds = append(ds, invalidField("db", "must be between 0 and 15"))
	}
	if len(in.Password) > 512 || strings.ContainsAny(in.Password, "\x00\r\n") {
		ds = append(ds, invalidField("password", "must be at most 512 bytes without NUL or line breaks"))
	}
	if len(ds) > 0 {
		return StepResult{}, apierr.Validation(ds...)
	}
	if s.o.Box == nil {
		return StepResult{}, precheck("master key unavailable", "主密钥不可用：没有 KEK 就不能加密 Redis 口令")
	}
	addr := strings.TrimSpace(in.Address)
	rcfg := redisx.Config{Addr: addr, DB: int(in.DB), TLS: in.TLS, Password: config.NewSecret(in.Password)}
	info, err := redisx.Probe(ctx, rcfg)
	if err != nil {
		if errors.Is(err, redisx.ErrRedisVersion) {
			return StepResult{}, precheck("redis precheck failed", err.Error())
		}
		msg := err.Error()
		if in.Password != "" {
			msg = strings.ReplaceAll(msg, in.Password, "***")
		}
		return StepResult{}, precheck("redis precheck failed", msg)
	}
	if err := info.CheckMajor(); err != nil {
		return StepResult{}, precheck("redis precheck failed", err.Error())
	}
	if err := dbcred.WriteRedis(ctx, s.credPath(), s.o.Box, dbcred.RedisInput{Addr: addr, DB: int(in.DB), TLS: in.TLS, Pass: config.NewSecret(in.Password)}); err != nil {
		return StepResult{}, apierr.Internal(err)
	}
	s.dropPost()
	if err := s.audit(ctx, StepRedis, map[string]any{"addr": addr, "db": in.DB, "tls": in.TLS, "redis_version": info.Version, "auth_configured": in.Password != ""}, req); err != nil {
		return StepResult{}, apierr.Internal(err)
	}
	return s.result(StepRedis, "Redis "+info.Version+" 校验通过（主版本 ≥8）；凭据已加密写入运行期凭据文件"), nil
}
