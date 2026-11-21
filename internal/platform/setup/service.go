// Package setup 实现 Server 的 setup mode 与 Web 安装向导后端（Story 1.24、tools-design §13/§13.1、ADR-026、ADR-016）。
//
// 无 install.lock 且无完成的 system_installations 时 Server 以 setup mode 启动：只有 /setup 静态页与 /api/v1/public/setup/*（本包）可用，其余 API 503，
// 默认仅监听 127.0.0.1:8080；所有向导接口要求 X-Setup-Token（与 /etc/lingedge/server/setup-token.hash 的 SHA-256 常量时间比较，失败节流）。
// 安装完成后（install.lock 存在）同样的路由永久返回 410 setup.already_installed（MIN-04），setup token 哈希被删除。
//
// 向导步骤（每步幂等，可中断续做）：环境检查 → MySQL → Redis → 数据库初始化 → 创建管理员 → 系统基础配置 → 内部身份与密钥初始化 → 完成。
// 同一个 Service 同时服务 HTTP 与无头路径（`lingedge-server setup --from-file`）：传输层只负责 token/限流/传输安全，业务全部在 Service 方法里。
//
// 凭据纪律（ADR-026）：临时管理员口令与 migrate 口令只在内存（[]byte，用后清零），不落盘、不入日志/审计/state/错误；app/maint/Redis 口令在对应步骤成功后
// 立即经 secretbox 信封加密写入 runtime-credentials.json（dbcred）；migrate 池只经 migrate.Window 构造，complete 之后窗口关闭，进程内不可再构造。
package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/audit"
	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/dbcred"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/migrate"
	"github.com/wuekevin/lingedge/internal/platform/redisx"
	"github.com/wuekevin/lingedge/internal/platform/requestid"
	setuprepo "github.com/wuekevin/lingedge/internal/platform/setup/repository"
	"github.com/wuekevin/lingedge/internal/platform/signingkeys"
	"github.com/wuekevin/lingedge/internal/platform/sysconfig"
	"github.com/wuekevin/lingedge/internal/platform/uid"
	"github.com/wuekevin/lingedge/pkg/secretbox"
	"github.com/wuekevin/lingedge/pkg/setuptoken"
	"github.com/wuekevin/lingedge/pkg/version"
)

// LockFileName 是 install.lock 在数据目录中的文件名（tools-design §13：/var/lib/lingedge/server/install.lock）。
const LockFileName = "install.lock"

// PostDeps 是创建管理员服务（认证模块）所需的依赖：迁移完成后由向导装配。
type PostDeps struct {
	Pool   db.Pool
	Redis  *redisx.Client
	Clock  clock.Clock
	IDs    *uid.Generator
	Log    *zap.Logger
	Box    *secretbox.Box
	Audit  audit.Recorder
	Argon2 config.Argon2
	// Ctx 是服务生命周期上下文（认证模块的权限失效订阅随它结束）。
	Ctx context.Context
}

// Options 是 New 的参数。
type Options struct {
	Config *config.Config
	// Box 是信封加密（主密钥已加载）；nil 时环境检查报错、涉及口令落盘的步骤拒绝（不存在「没有 KEK 也写」）。
	Box    *secretbox.Box
	Logger *zap.Logger
	Clock  clock.Clock
	IDs    *uid.Generator
	// Migrations 是内嵌迁移（migrations.FS）。
	Migrations fs.FS
	// NewAdmin 由装配层提供（用 auth.Module.Bootstrap 适配）；nil 时创建管理员步骤拒绝。
	NewAdmin func(PostDeps) (AdminService, error)
	// TokenHashPath 缺省 setuptoken.DefaultHashPath；LockPath 缺省 <server.data_dir>/install.lock。
	TokenHashPath string
	LockPath      string
	// Installed 为真表示已安装（正常模式）：全部向导路由永久 410。
	Installed bool
	// ManifestOnly 仅用于导出路由清单。
	ManifestOnly bool
	// ListenAddr 是 REST 监听地址（缺省取 Config.HTTP.Listen）：非回环且非 TLS 时携带口令的接口返回 400 setup.insecure_transport。
	ListenAddr string
	// FailMax/FailWindow 是 token 失败节流（缺省 10 次 / 5 分钟）；RateMax 是每 IP 每分钟请求上限（缺省 120）。
	FailMax    int
	FailWindow time.Duration
	RateMax    int
	// Redis 是可选的节流后端（Redis 不可用时 redisx 自动降级到进程内计数；nil = 只用进程内计数）。
	Redis *redisx.Client
	// OnComplete 在 complete 成功、响应已写出之后调用（server 据此退出 setup mode）；可为 nil。
	OnComplete func()
}

// Service 是向导服务。并发安全：同一时刻只执行一个写操作（opMu），Status 在有操作进行时返回缓存进度。
type Service struct {
	o    Options
	log  *zap.Logger
	clk  clock.Clock
	ids  *uid.Generator
	cfg  *config.Config
	trus []netip.Prefix

	installed atomic.Bool
	opMu      sync.Mutex
	mu        sync.Mutex // 保护 prog/held/pending
	prog      map[string]*progress
	envOK     bool
	held      held
	pending   []pendingAudit
	post      *post
	window    *migrate.Window
	guard     *guard
	done      chan struct{}
	doneOnce  sync.Once
	svcCtx    context.Context
	svcCancel context.CancelFunc
}

type progress struct {
	status  string
	message string
}

// held 是仅内存的凭据（[]byte，用后清零）。
type held struct {
	migrateUser string
	migrate     []byte
	generated   bool // migrate 口令由向导生成（complete 响应里一次性返回）
}

func (h *held) zeroMigrate() {
	secretbox.Zero(h.migrate)
	h.migrate = nil
	h.generated = false
}

type pendingAudit struct {
	step    string
	changes map[string]any
	reqID   string
	ip      string
}

// post 是迁移完成之后才存在的资源（app 池、Redis 客户端与各服务）。
type post struct {
	app    db.Pool
	rc     *redisx.Client
	repo   *setuprepo.Repo
	audit  *audit.Writer
	sys    *sysconfig.Module
	keys   *signingkeys.Service
	admin  AdminService
	cancel context.CancelFunc
}

// New 创建 Service。
func New(o Options) (*Service, error) {
	if o.Logger == nil {
		o.Logger = zap.NewNop()
	}
	if o.Clock == nil {
		o.Clock = clock.System()
	}
	s := &Service{o: o, log: o.Logger.Named("setup"), clk: o.Clock, ids: o.IDs, cfg: o.Config, done: make(chan struct{}), prog: map[string]*progress{}}
	s.installed.Store(o.Installed)
	if o.ManifestOnly || o.Installed {
		// 清单导出 / 已安装（正常模式）：路由必须注册（已安装时每个 operation 返回 410），但不需要任何基础设施。
		return s, nil
	}
	if o.Config == nil || o.IDs == nil {
		return nil, errors.New("setup: 缺少必需依赖（Config、IDs）")
	}
	if o.TokenHashPath == "" {
		o.TokenHashPath = setuptoken.DefaultHashPath
	}
	if o.LockPath == "" {
		o.LockPath = filepath.Join(o.Config.Server.DataDir, LockFileName)
	}
	if o.ListenAddr == "" {
		o.ListenAddr = o.Config.HTTP.Listen
	}
	s.o = o
	for _, p := range o.Config.Server.TrustedProxies {
		if pf, err := netip.ParsePrefix(p); err == nil {
			s.trus = append(s.trus, pf)
		} else if a, err := netip.ParseAddr(p); err == nil {
			s.trus = append(s.trus, netip.PrefixFrom(a, a.BitLen()))
		}
	}
	s.guard = newGuard(o.TokenHashPath, o.FailMax, o.FailWindow, o.RateMax, o.Clock, o.Redis)
	s.svcCtx, s.svcCancel = context.WithCancel(context.Background())
	if !o.Installed {
		s.window = migrate.NewWindow()
	}
	return s, nil
}

// Done 在 complete 成功且响应已写出后关闭（server 据此退出 setup mode）。
func (s *Service) Done() <-chan struct{} { return s.done }

// Window 返回 migrate 窗口（测试断言用；nil = 已安装模式）。
func (s *Service) Window() *migrate.Window { return s.window }

// Installed 报告是否已安装（内存标记或 install.lock 存在）。
func (s *Service) Installed() bool {
	if s.installed.Load() {
		return true
	}
	if s.o.LockPath != "" {
		if _, err := os.Stat(s.o.LockPath); err == nil {
			s.installed.Store(true)
			return true
		}
	}
	return false
}

// Close 释放向导持有的全部资源（app 池、Redis、migrate 窗口、内存凭据）。幂等。
func (s *Service) Close() {
	s.mu.Lock()
	s.held.zeroMigrate()
	p := s.post
	s.post = nil
	s.mu.Unlock()
	if s.window != nil {
		s.window.Close()
	}
	if p != nil {
		p.close()
	}
	if s.svcCancel != nil {
		s.svcCancel()
	}
}

func (p *post) close() {
	if p.cancel != nil {
		p.cancel()
	}
	if p.rc != nil {
		_ = p.rc.Close()
	}
	if p.app != nil {
		_ = p.app.Close()
	}
}

// ---- 上下文与审计 ----

// opCtx 为一次操作派生上下文：与请求同生共死（取消/超时），但不携带路由元数据（路由元数据会让审计记录器按 operation 校验动作；向导以系统组件身份写审计）。
func (s *Service) opCtx(req context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	bg := context.Background()
	if id, ok := requestid.FromContext(req); ok {
		bg, _ = requestid.NewContext(bg, id)
	} else if id, err := s.ids.New(); err == nil {
		bg, _ = requestid.NewContext(bg, id)
	}
	ctx, cancel := context.WithTimeout(bg, timeout)
	stop := context.AfterFunc(req, cancel)
	return ctx, func() { stop(); cancel() }
}

func clientIP(req context.Context) string { return httpx.ClientIPFrom(req) }

func (s *Service) setProgress(step, status, msg string) {
	s.mu.Lock()
	s.prog[step] = &progress{status: status, message: msg}
	s.mu.Unlock()
}

// recordAudit 把步骤事件写入审计（动作必须是 audit.Action* 常量，门禁 audit-check 据此核对 Catalog 与 .Record 调用点）。
func recordAudit(ctx context.Context, w audit.Recorder, step string, ev audit.Event) error {
	sys := audit.SystemActor("setup")
	ev.Actor = &sys
	switch step {
	case "checks":
		return w.Record(ctx, audit.ActionSetupChecks, ev)
	case StepDatabase:
		return w.Record(ctx, audit.ActionSetupDatabase, ev)
	case StepRedis:
		return w.Record(ctx, audit.ActionSetupRedis, ev)
	case StepInitDB:
		return w.Record(ctx, audit.ActionSetupInitialize, ev)
	case StepKeys:
		return w.Record(ctx, audit.ActionSetupKeys, ev)
	case StepComplete:
		return w.Record(ctx, audit.ActionSetupComplete, ev)
	}
	return fmt.Errorf("setup: 未知的审计步骤 %q", step)
}

func auditEvent(step string, changes map[string]any, ip string) audit.Event {
	ev := audit.Event{ResourceType: audit.ResourceSystem, ResourceUID: "setup", Changes: changes}
	if changes == nil {
		ev.Changes = map[string]any{}
	}
	ev.Changes["step"] = step
	if ip != "" {
		ev.Meta = map[string]any{"client_ip": ip}
	}
	return ev
}

// audit 记录一个步骤事件：app 池与审计写入器已就绪时在事务内立即写（失败 = 该步骤失败，fail-closed）；否则（迁移完成之前 audit_logs 尚不存在）
// 先缓冲，待迁移完成后在 initialize 的状态事务内补写。缓冲只在内存：进程在补写前退出则这些事件丢失（同时已有结构化运行日志，不含口令）。
func (s *Service) audit(ctx context.Context, step string, changes map[string]any, req context.Context) error {
	s.mu.Lock()
	p := s.post
	s.mu.Unlock()
	ip := clientIP(req)
	if p == nil || p.audit == nil || !p.ready() {
		rid, _ := requestid.FromContext(ctx)
		s.mu.Lock()
		s.pending = append(s.pending, pendingAudit{step: step, changes: changes, reqID: rid, ip: ip})
		s.mu.Unlock()
		s.log.Info("向导步骤（审计待迁移完成后补写）", zap.String("step", step), zap.Any("summary", changes))
		return nil
	}
	return p.app.WithTx(ctx, func(ctx context.Context) error { return recordAudit(ctx, p.audit, step, auditEvent(step, changes, ip)) })
}

func (p *post) ready() bool { return p.repo != nil && p.audit != nil }

// flushPending 在事务 ctx 内补写缓冲的审计事件。
func (s *Service) flushPending(ctx context.Context, p *post) error {
	s.mu.Lock()
	list := s.pending
	s.mu.Unlock()
	for _, pa := range list {
		if err := recordAudit(ctx, p.audit, pa.step, auditEvent(pa.step, pa.changes, pa.ip)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) clearPending() {
	s.mu.Lock()
	s.pending = nil
	s.mu.Unlock()
}

// ---- 持久进度（system_installations.meta_json["setup"]） ----

type stepMeta struct {
	Done   bool   `json:"done"`
	At     string `json:"at,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type setupMeta struct {
	V                int                 `json:"v"`
	Steps            map[string]stepMeta `json:"steps"`
	BaseURL          string              `json:"base_url,omitempty"`
	MigrateGenerated bool                `json:"migrate_generated"`
	StartedAt        string              `json:"started_at,omitempty"`
}

type metaDoc struct {
	Setup setupMeta `json:"setup"`
}

func parseMeta(raw *string) setupMeta {
	m := setupMeta{V: 1, Steps: map[string]stepMeta{}}
	if raw == nil {
		return m
	}
	var d metaDoc
	if json.Unmarshal([]byte(*raw), &d) == nil && d.Setup.V == 1 {
		m = d.Setup
		if m.Steps == nil {
			m.Steps = map[string]stepMeta{}
		}
	}
	return m
}

func (m setupMeta) encode() string {
	b, _ := json.Marshal(metaDoc{Setup: m})
	return string(b)
}

// mutateMeta 在事务内读-改-写进度行（不存在则创建）。fn 返回后 extra（可为 nil）在同一事务内执行（如补写审计）。
func (s *Service) mutateMeta(ctx context.Context, p *post, fn func(*setupMeta), extra func(ctx context.Context) error) error {
	return p.app.WithTx(ctx, func(ctx context.Context) error {
		row, found, err := p.repo.Get(ctx, true)
		if err != nil {
			return err
		}
		m := parseMeta(row.Meta)
		if row.InstallLocked != nil {
			return apierr.New(apierr.SetupAlreadyInstalled, "setup already completed")
		}
		if m.StartedAt == "" {
			m.StartedAt = s.clk.Now().UTC().Format(time.RFC3339)
		}
		fn(&m)
		if found {
			if _, err := p.repo.UpdateMeta(ctx, row.ID, m.encode()); err != nil {
				return err
			}
		} else {
			id, err := s.ids.New()
			if err != nil {
				return err
			}
			inst, err := s.ids.New()
			if err != nil {
				return err
			}
			if err := p.repo.Insert(ctx, id, inst, version.Version, s.clk.Now(), m.encode()); err != nil {
				return err
			}
		}
		if extra != nil {
			return extra(ctx)
		}
		return nil
	})
}

func (s *Service) markStep(ctx context.Context, p *post, step, detail string, extra func(*setupMeta)) error {
	return s.mutateMeta(ctx, p, func(m *setupMeta) {
		m.Steps[step] = stepMeta{Done: true, At: s.clk.Now().UTC().Format(time.RFC3339), Detail: detail}
		if extra != nil {
			extra(m)
		}
	}, nil)
}

// ---- 凭据文件与 post 装配 ----

func (s *Service) credPath() string { return dbcred.Path(s.cfg) }

func envName(cfg *config.Config) string {
	if cfg.Server.Env != "" {
		return cfg.Server.Env
	}
	return "setup"
}

// ensurePost 打开 app 池（凭据取自运行期凭据文件）并装配迁移完成后需要的服务。要求 database 与 redis 两段都已写入。
// app 账号在迁移之前对库没有任何权限，连接会失败（ERROR 1044）——调用方据此把 init_db 视为未完成。
func (s *Service) ensurePost(ctx context.Context) (*post, error) {
	s.mu.Lock()
	if s.post != nil {
		p := s.post
		s.mu.Unlock()
		return p, nil
	}
	s.mu.Unlock()
	if s.o.Box == nil {
		return nil, secretbox.ErrMasterKeyMissing
	}
	f, err := dbcred.Load(s.credPath())
	if err != nil {
		return nil, err
	}
	if f.Database == nil || f.Redis == nil {
		return nil, errors.New("setup: 运行期凭据文件缺少 database/redis 段")
	}
	pw, err := f.AppPassword(ctx, s.o.Box)
	if err != nil {
		return nil, err
	}
	app, err := db.OpenApp(ctx, db.Config{Host: f.Database.Host, Port: f.Database.Port, Name: f.Database.Name, User: f.Database.App.User, Password: pw, TLS: f.Database.TLS})
	if err != nil {
		return nil, err
	}
	rpw, err := f.RedisPassword(ctx, s.o.Box)
	if err != nil {
		_ = app.Close()
		return nil, err
	}
	rc, err := redisx.New(s.svcCtx, redisx.Config{Addr: f.Redis.Addr, DB: f.Redis.DB, TLS: f.Redis.TLS, Password: rpw, Env: envName(s.cfg)}, redisx.Options{Logger: s.o.Logger, Clock: s.clk})
	if err != nil {
		_ = app.Close()
		return nil, err
	}
	pctx, cancel := context.WithCancel(s.svcCtx)
	p := &post{app: app, rc: rc, cancel: cancel}
	fail := func(err error) (*post, error) { p.close(); return nil, err }
	if p.repo, err = setuprepo.New(app); err != nil {
		return fail(err)
	}
	if p.audit, err = audit.New(audit.Options{Pool: app, IDs: s.ids, Clock: s.clk, Log: s.o.Logger}); err != nil {
		return fail(err)
	}
	if p.sys, err = sysconfig.New(sysconfig.Options{Pool: app, Clock: s.clk, IDs: s.ids, Log: s.o.Logger, Box: s.o.Box, Audit: p.audit}); err != nil {
		return fail(err)
	}
	if p.keys, err = signingkeys.New(signingkeys.Options{Pool: app, Box: s.o.Box, Audit: p.audit, IDs: s.ids, Clock: s.clk, Log: s.o.Logger}); err != nil {
		return fail(err)
	}
	if s.o.NewAdmin != nil {
		if p.admin, err = s.o.NewAdmin(PostDeps{Pool: app, Redis: rc, Clock: s.clk, IDs: s.ids, Log: s.o.Logger, Box: s.o.Box, Audit: p.audit, Argon2: s.cfg.Auth.Argon2, Ctx: pctx}); err != nil {
			return fail(err)
		}
	}
	s.mu.Lock()
	if s.post != nil { // 并发创建：保留先到者
		s.mu.Unlock()
		p.close()
		return s.post, nil
	}
	s.post = p
	s.mu.Unlock()
	return p, nil
}

// ---- 状态 ----

func (s *Service) next(after string) *string {
	for i, k := range stepOrder {
		if k == after && i+1 < len(stepOrder) {
			n := stepOrder[i+1]
			return &n
		}
	}
	return nil
}

func ptr(s string) *string { return &s }

// Status 返回向导状态（只读）。有写操作进行时返回缓存进度（耗时步骤据此轮询，不会阻塞）。
func (s *Service) Status(ctx context.Context) (Status, error) {
	st := Status{Installed: s.Installed(), ServerVersion: version.Version}
	if s.opMu.TryLock() {
		s.refresh(ctx)
		s.opMu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range stepOrder {
		it := StepStatus{Key: k, Status: StatusPending}
		if p := s.prog[k]; p != nil {
			it.Status = p.status
			if p.message != "" {
				it.Message = ptr(p.message)
			}
		}
		if st.Installed {
			it.Status = StatusDone
		}
		st.Steps = append(st.Steps, it)
	}
	return st, nil
}

// refresh 由持久事实重建各步骤进度（续做的依据）：凭据文件的 database/redis 段、system_installations 里的进度行与迁移版本。须持有 opMu。
func (s *Service) refresh(ctx context.Context) {
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	set := func(step string, done bool, msg string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		cur := s.prog[step]
		switch {
		case done:
			s.prog[step] = &progress{status: StatusDone, message: msg}
		case cur != nil && cur.status == StatusFailed:
			// 保留最近一次失败原因，直到该步骤再次成功
		default:
			s.prog[step] = &progress{status: StatusPending}
		}
	}
	s.mu.Lock()
	envOK := s.envOK
	s.mu.Unlock()
	set(StepEnvironment, envOK, "环境检查已通过")
	set(StepComplete, false, "")
	f, err := dbcred.Load(s.credPath())
	if err != nil && !errors.Is(err, dbcred.ErrNotFound) {
		s.setProgress(StepDatabase, StatusFailed, "运行期凭据文件无效："+err.Error())
		return
	}
	dbDone := f != nil && f.Database != nil
	set(StepDatabase, dbDone, "数据库与专用账号已就绪，app/maint 口令已加密写入运行期凭据文件")
	set(StepRedis, f != nil && f.Redis != nil, "Redis 已验证，凭据已写入运行期凭据文件")
	if !dbDone || f.Redis == nil {
		for _, k := range []string{StepInitDB, StepAdmin, StepSystem, StepKeys} {
			set(k, false, "")
		}
		return
	}
	p, err := s.ensurePost(rctx)
	if err != nil { // 迁移之前 app 账号尚无库权限：连接失败 = init_db 未完成
		for _, k := range []string{StepInitDB, StepAdmin, StepSystem, StepKeys} {
			set(k, false, "")
		}
		return
	}
	probe := httpx.MigrationProbeFromApp(p.app, s.migrationSet())
	ms, perr := probe.MigrationState(rctx)
	migrated := perr == nil && ms.Problem == "" && ms.Pending == 0 && ms.Applied > 0
	row, found, gerr := p.repo.Get(rctx, false)
	if migrated && (!found || gerr != nil) && gerr == nil {
		// 迁移已完成但进度行尚未写入（上次在两者之间中断）：补记。
		if err := s.markStep(rctx, p, StepInitDB, fmt.Sprintf("迁移已应用到 %06d", ms.Applied), nil); err != nil {
			s.log.Warn("补记 init_db 进度失败", zap.Error(err))
		} else if row2, f2, _ := p.repo.Get(rctx, false); f2 {
			row, found = row2, true
		}
	}
	m := parseMeta(row.Meta)
	if !found {
		m = parseMeta(nil)
	}
	set(StepInitDB, migrated, fmt.Sprintf("迁移已应用到 %06d", ms.Applied))
	for _, k := range []string{StepAdmin, StepSystem, StepKeys} {
		sm := m.Steps[k]
		set(k, migrated && sm.Done, sm.Detail)
	}
}

func (s *Service) migrationSet() *migrate.Set {
	set, _, err := migrate.Load(s.o.Migrations)
	if err != nil {
		return &migrate.Set{}
	}
	return set
}

// stepDone 报告某步骤当前是否完成（须在 refresh 之后调用，持有 opMu）。
func (s *Service) stepDone(step string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.prog[step]
	return p != nil && p.status == StatusDone
}

// require 校验前置步骤已完成，否则 409 并说明应先完成哪一步。
func (s *Service) require(ctx context.Context, steps ...string) error {
	s.refresh(ctx)
	for _, st := range steps {
		if !s.stepDone(st) {
			return apierr.New(apierr.ResourceConflict, "previous setup step not completed", apierr.Detail{Field: "step", Reason: "complete step " + st + " first"})
		}
	}
	return nil
}

// run 包装一个写操作：串行化、标记 running、记录失败原因。
func (s *Service) run(step string, fn func() (StepResult, error)) (StepResult, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if s.Installed() {
		return StepResult{}, apierr.New(apierr.SetupAlreadyInstalled, "setup already completed")
	}
	s.setProgress(step, StatusRunning, "")
	res, err := fn()
	if err != nil {
		msg := "失败"
		if ae, ok := apierr.As(err); ok {
			msg = ae.Message
			for _, d := range ae.Details {
				msg += "；" + d.Reason
			}
		}
		s.setProgress(step, StatusFailed, msg)
		return StepResult{}, err
	}
	s.setProgress(step, StatusDone, res.Message)
	return res, nil
}

func (s *Service) result(step, msg string) StepResult {
	return StepResult{Step: step, OK: true, Message: msg, Next: s.next(step)}
}
