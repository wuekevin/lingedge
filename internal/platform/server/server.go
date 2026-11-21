// Package server 是 lingedge-server 的进程装配（Story 1.13、architecture §5.4）：按角色把平台基础设施与各角色组件装配成一个进程，
// 并管理其生命周期（启动 → 就绪通知 → 优雅停机）。
//
// 装配顺序（任何一步失败都拒绝启动并回收已打开的资源）：
//
//	主密钥（secretbox.FileKeyProvider；缺失/权限不符即拒绝，ADR-011）→ app 连接池（MySQL）→ Redis（不可达只降级，ADR-025）
//	→ 内嵌迁移集合与迁移版本探针（app 池只读 schema_migrations，CC-3）→ 幂等存储 → metrics 监听 → 按角色装配组件 → 全部就绪后 sd_notify READY=1。
//
// 角色与现状（诚实声明，没有任何占位功能被伪装成真实功能）：
//
//	api        真实：REST 路由器（httpx 固定中间件链 + /healthz + /healthz/ready）与 HTTP 监听；业务路由由 Options.Register 注册（Story 1.16 起追加）。
//	grpc       仅生命周期骨架：不监听任何端口（Node mTLS :9443 / Enroll :9444 由 Story 3.13 实现）。
//	scheduler  真实（Story 1.15）：job_locks 租约选主（scheduler.leader）+ cron 注册表；仅 leader 触发。内置内部周期作业 tasks.reap / idempotency.cleanup /
//	           sessions.cleanup / partition.maintain（不属 TaskType，AA-8），后续 Story 经 Options.Cron 登记自己的周期任务（任务型以确定性幂等键入队）。
//	           partition.maintain 需要 maint 账号（Options.OpenMaint + database.maint.*）；未配置则每次触发都明确跳过并告警，不假装成功。
//	worker     真实（Story 1.14）：任务运行时——按 Options.Tasks 登记的任务类型从 tasks 表领取（SKIP LOCKED + 租约 + 围栏）、续约、写回并回收过期租约，
//	           Redis 唤醒仅加速（轮询兜底）；未登记任何类型时空闲（不领取）。停机时停止领取，排空在途任务，超时则归还。
//
// metrics（默认 127.0.0.1:9180）对所有角色常开：进程级指标不依赖 api 角色。
//
// 优雅停机（SIGTERM/SIGINT 取消 ctx 后）：sd_notify STOPPING=1 → 停止接受新请求（关闭监听）并排空在途请求（http.Server.Shutdown，
// 受 ShutdownTimeout 限制）→ 停止各角色骨架 → 关闭 Redis → 关闭 MySQL 连接池 → 清零主密钥。
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/idempotency"
	idemrepo "github.com/wuekevin/lingedge/internal/platform/idempotency/repository"
	"github.com/wuekevin/lingedge/internal/platform/logx"
	"github.com/wuekevin/lingedge/internal/platform/migrate"
	"github.com/wuekevin/lingedge/internal/platform/redisx"
	"github.com/wuekevin/lingedge/internal/platform/requestid"
	"github.com/wuekevin/lingedge/internal/platform/schedrt"
	"github.com/wuekevin/lingedge/internal/platform/schedrt/builtin"
	schedrepo "github.com/wuekevin/lingedge/internal/platform/schedrt/repository"
	"github.com/wuekevin/lingedge/internal/platform/sdnotify"
	"github.com/wuekevin/lingedge/internal/platform/taskrt"
	taskrepo "github.com/wuekevin/lingedge/internal/platform/taskrt/repository"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
	"github.com/wuekevin/lingedge/internal/platform/uid"
	"github.com/wuekevin/lingedge/migrations"
	"github.com/wuekevin/lingedge/pkg/secretbox"
)

// DefaultShutdownTimeout 是排空在途请求的最长等待（超时后强制关闭连接并以非零码退出）。
// [ASSUMPTION] 工程缺省，基线未规定；与 systemd 默认 TimeoutStopSec=90s 留有余量。
const DefaultShutdownTimeout = 30 * time.Second

// Options 是 Run 的参数。
type Options struct {
	Config *config.Config
	Roles  RoleSet
	Logger *zap.Logger
	Clock  clock.Clock

	// Register 是 REST 路由的装配点（cmd/lingedge-server 的 registerRoutes）；nil 表示不注册任何路由。
	Register func(*httpx.Router, Deps) error
	// Tasks 是任务类型注册表（白名单，Story 1.14）；nil = 空注册表（worker 角色空闲，入队一律被拒）。
	Tasks *taskrt.Registry
	// Hooks 是认证/授权/Scope/审计钩子；零值（各项 nil）= 安全拒绝默认实现（尚未实现的钩子保持不变，1.17–1.21）。
	Hooks httpx.Hooks
	// BuildHooks 在基础设施（MySQL/Redis）就绪后构造钩子（Story 1.16：认证钩子需要数据库与 Redis）；非 nil 时取代 Hooks。
	// 返回错误即拒绝启动。
	BuildHooks func(Deps) (httpx.Hooks, error)

	// Cron 是周期任务的登记点（Story 1.15）：后续 Story 在此向 cron 注册表登记自己的周期任务（Registry.Task：确定性幂等键入队 OpenAPI TaskType）
	// 或内部作业（Registry.Internal）。内置的四个内部作业已由 scheduler 角色自行登记，不需要在这里重复。nil = 只有内置作业。
	Cron func(*schedrt.Registry, Deps) error
	// OpenMaint 以 maint 账号（lingedge_maint）打开独立连接池，仅供 partition.maintain 使用（首次触发时才调用；失败只让该次作业失败并告警）。
	// 构造 maint 池的调用点受门禁 [db-pool-maint] 限制（只有 cmd/lingedge-server 等白名单位置可以调用 db.OpenMaint），因此由装配层注入。
	// nil 或配置未提供 maint 账号 = 分区维护作业明确跳过并告警。
	OpenMaint func(ctx context.Context, d config.Database) (*db.MaintPool, error)
	// LeaseTTL 与 Tick 是 Scheduler 选主租约与探测间隔（零值取 schedrt 缺省；集成测试缩短它们以便观察接管）。
	SchedulerLeaseTTL time.Duration
	SchedulerTick     time.Duration

	ShutdownTimeout time.Duration
	// OnReady 在全部监听就绪且 READY=1 已发出后调用（集成测试据此取得实际监听地址）；可为 nil。
	OnReady func(Info)
}

// Deps 是装配完成后交给路由注册函数的共享依赖。
type Deps struct {
	// Tasks 是任务存储（任务中心路由与业务模块入队使用）。
	Tasks taskrt.Store
	// Enqueuer 是入队服务（白名单校验 + Redis 唤醒）；业务模块的耗时操作经它返回 202 + Task。
	Enqueuer *taskrt.Enqueuer

	// 以下是装配层构造业务模块所需的共享基础设施（Story 1.16 起；路由清单导出模式下为零值）。
	// DB 是 app 连接池（请求路径只持有 app 账号，CC-3）；Redis 已按 ADR-025 封装（不可达时降级）。
	DB     db.Pool
	Redis  *redisx.Client
	Clock  clock.Clock
	IDs    *uid.Generator
	Logger *zap.Logger
	Config *config.Config
	// Secrets 是信封加密（pkg/secretbox，主密钥已加载）；业务模块用它加解密密文列（如 users.totp_secret_enc，Story 1.17）。
	Secrets *secretbox.Box
	// OnShutdown 登记停机钩子：在监听排空、角色停止之后、MySQL/Redis 关闭之前，按登记的相反顺序执行（模块用它收尾异步写入，如认证模块的 Flush，Story 1.16 遗留）。
	// 钩子在 ctx 的停机预算内运行；返回的错误并入停机错误。仅在装配阶段（BuildHooks/Register）调用。
	OnShutdown func(name string, fn func(ctx context.Context) error)
	// Migrations 是迁移版本探针（app 只读 schema_migrations，Story 1.13）：/admin/system/version 据此返回真实迁移版本（Story 1.22）。
	Migrations httpx.MigrationProbe
}

// Info 是就绪后的实际状态。
type Info struct {
	Roles       []Role
	HTTPAddr    string // api 角色未启用时为空
	MetricsAddr string
}

// component 是角色的生命周期抽象。
type component interface {
	Name() string
	// Start 启动后台工作（非阻塞）；骨架角色无事可做。
	Start(ctx context.Context)
	Shutdown(ctx context.Context) error
}

// closer 记录需要按相反顺序关闭的资源。
type closer struct {
	name string
	fn   func(ctx context.Context) error
}

// Run 装配并运行进程，阻塞到 ctx 被取消（SIGTERM/SIGINT）或某个监听异常退出；返回 nil 表示启动成功且优雅停机无错误。
func Run(ctx context.Context, o Options) error {
	if o.Config == nil {
		return errors.New("server: 缺少配置")
	}
	if len(o.Roles.List()) == 0 {
		return errors.New("server: 至少需要一个角色")
	}
	if o.Logger == nil {
		return errors.New("server: 缺少 Logger")
	}
	if o.Clock == nil {
		o.Clock = clock.System()
	}
	if o.ShutdownTimeout <= 0 {
		o.ShutdownTimeout = DefaultShutdownTimeout
	}
	cfg := o.Config
	ids := uid.NewGenerator(o.Clock)

	// 进程级 request-id：启动/停机阶段没有入站请求，每次进程启动生成一个，使这些日志也能被关联（NFR-21）。
	bootID, err := ids.New()
	if err != nil {
		return fmt.Errorf("server: 生成启动 request-id: %w", err)
	}
	ctx, _ = requestid.NewContext(ctx, bootID)
	log := logx.WithContext(ctx, o.Logger)

	var closers []closer
	closeAll := func(sctx context.Context) error {
		var errs []error
		for i := len(closers) - 1; i >= 0; i-- {
			if err := closers[i].fn(sctx); err != nil {
				errs = append(errs, fmt.Errorf("关闭 %s: %w", closers[i].name, err))
			}
		}
		closers = nil
		return errors.Join(errs...)
	}
	fail := func(err error) error {
		log.Error("启动失败，拒绝启动", zap.Error(err))
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), o.ShutdownTimeout)
		defer cancel()
		_ = closeAll(sctx)
		return err
	}

	log.Info("lingedge-server 启动", zap.String("roles", o.Roles.String()), zap.String("env", cfg.Server.Env))

	// 1. 主密钥：缺失或权限/属主不符即拒绝（fail-closed）。
	keys, err := secretbox.NewFileKeyProvider(secretbox.FileOptions{Path: cfg.Secrets.MasterKeyFile, CredentialsDir: secretbox.CredentialsDirFromEnv()})
	if err != nil {
		return fail(fmt.Errorf("主密钥不可用: %w。指引：Tools 安装步骤 18 会生成 /etc/lingedge/server/master.key（root:root 0400）并由 systemd LoadCredential 提供；"+
			"手工运行时请在 server.yaml 配置 secrets.master_key_file（绝对路径），文件须为 0400/0440 且属主是 root 或运行用户", err))
	}
	closers = append(closers, closer{"主密钥", func(context.Context) error { keys.Close(); return nil }})
	log.Info("主密钥已加载", zap.String("current_kek_id", keys.CurrentKeyID()))

	// 2. MySQL（app 账号）。
	app, err := db.OpenApp(ctx, db.AppConfigFrom(cfg.Database))
	if err != nil {
		return fail(fmt.Errorf("无法连接数据库（app 账号）: %w。指引：核对 database.host/port/name/user 与口令（环境变量 LINGEDGE_DATABASE__PASSWORD 或 database.password_file）", err))
	}
	closers = append(closers, closer{"MySQL 连接池", func(context.Context) error { return app.Close() }})

	// 3. Redis：不可达不是启动失败（降级运行，ADR-025）。
	rc, err := redisx.New(ctx, redisx.FromConfig(*cfg), redisx.Options{Logger: o.Logger, Clock: o.Clock})
	if err != nil {
		return fail(fmt.Errorf("无效的 Redis 配置: %w", err))
	}
	closers = append(closers, closer{"Redis", func(context.Context) error { return rc.Close() }})

	// 4. 迁移版本探针（app 只读 schema_migrations；不持有 migrate 凭据，CC-3）。
	set, findings, err := migrate.Load(migrations.FS)
	if err != nil || len(findings) > 0 {
		return fail(fmt.Errorf("内嵌迁移集合无效（构建缺陷）: %v %v", err, findings))
	}
	probe := httpx.MigrationProbeFromApp(app, set)

	// 5. 幂等存储（MySQL 为真相源，Redis 不参与）。
	idem, err := idemrepo.New(app, idemrepo.Options{Clock: o.Clock})
	if err != nil {
		return fail(err)
	}

	// 5b. 任务运行时（Story 1.14）：tasks 存储（app 池）、入队服务（Redis 唤醒仅加速）。
	if o.Tasks == nil {
		o.Tasks = taskrt.NewRegistry()
	}
	taskStore, err := taskrepo.New(app, ids)
	if err != nil {
		return fail(err)
	}
	waker, err := taskrt.NewRedisWaker(rc, o.Logger)
	if err != nil {
		return fail(err)
	}
	enq, err := taskrt.NewEnqueuer(taskStore, o.Tasks, ids, waker)
	if err != nil {
		return fail(err)
	}
	enq.InTx = func(ctx context.Context) bool { _, in := db.TxFromContext(ctx, app); return in }
	box, err := secretbox.New(keys)
	if err != nil {
		return fail(err)
	}
	deps := Deps{Tasks: taskStore, Enqueuer: enq, DB: app, Redis: rc, Clock: o.Clock, IDs: ids, Logger: o.Logger, Config: cfg, Secrets: box, Migrations: probe,
		OnShutdown: func(name string, fn func(context.Context) error) { closers = append(closers, closer{name, fn}) }}

	metrics := httpx.NewMetrics()
	metrics.RegisterGauge("lingedge_db_open_connections", "app 连接池当前打开的连接数", func() float64 { return float64(app.Stats().OpenConnections) })
	metrics.RegisterGauge("lingedge_redis_up", "Redis 是否可用（1=up，0=降级）", func() float64 {
		if rc.State() == redisx.StateUp {
			return 1
		}
		return 0
	})

	// 6. 监听：先全部绑定（地址冲突立即失败），再开始服务。
	type served struct {
		name string
		srv  *http.Server
		ln   net.Listener
	}
	var servers []served
	var comps []component
	info := Info{Roles: o.Roles.List()}

	mln, err := net.Listen("tcp", cfg.Metrics.Listen)
	if err != nil {
		return fail(fmt.Errorf("metrics 监听 %s 失败: %w", cfg.Metrics.Listen, err))
	}
	closers = append(closers, closer{"metrics 监听", func(context.Context) error { _ = mln.Close(); return nil }})
	info.MetricsAddr = mln.Addr().String()
	if host, _, _ := net.SplitHostPort(cfg.Metrics.Listen); !isLoopbackHost(host) {
		log.Warn("metrics 监听在非回环地址：指标端口应仅本机可达（architecture §7）", zap.String("listen", cfg.Metrics.Listen))
	}
	servers = append(servers, served{"metrics", httpx.NewMetricsServer(cfg.Metrics.Listen, metrics), mln})

	if o.Roles.Has(RoleAPI) {
		hooks := o.Hooks
		if o.BuildHooks != nil {
			if hooks, err = o.BuildHooks(deps); err != nil {
				return fail(fmt.Errorf("装配认证/授权钩子失败: %w", err))
			}
		}
		router, err := httpx.NewRouter(httpx.Options{
			Logger: o.Logger, Clock: o.Clock, IDs: ids, Hooks: hooks, Metrics: metrics, Idempotency: idem,
			Health: httpx.HealthOptions{DB: app, Migrations: probe, Redis: rc},
		})
		if err != nil {
			return fail(err)
		}
		if o.Register != nil {
			if err := o.Register(router, deps); err != nil {
				return fail(fmt.Errorf("注册路由失败: %w", err))
			}
		}
		h, err := router.Handler() // 任何注册错误（元数据缺失等）在此变成启动失败
		if err != nil {
			return fail(err)
		}
		ln, err := net.Listen("tcp", cfg.HTTP.Listen)
		if err != nil {
			return fail(fmt.Errorf("API 监听 %s 失败: %w", cfg.HTTP.Listen, err))
		}
		closers = append(closers, closer{"API 监听", func(context.Context) error { _ = ln.Close(); return nil }})
		info.HTTPAddr = ln.Addr().String()
		servers = append(servers, served{"api", &http.Server{
			Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second,
			IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10,
		}, ln})
	}
	for _, r := range o.Roles.List() {
		switch r {
		case RoleAPI:
			continue
		case RoleWorker:
			wc, err := newWorkerComponent(o, taskStore, rc, log)
			if err != nil {
				return fail(err)
			}
			comps = append(comps, wc)
		case RoleScheduler:
			sc, err := newSchedulerComponent(o, app, taskStore, idem, enq, deps, metrics, log)
			if err != nil {
				return fail(err)
			}
			comps = append(comps, sc)
		default:
			comps = append(comps, newSkeleton(r, log))
		}
	}

	// 7. 开始服务。
	errCh := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			if err := s.srv.Serve(s.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("%s 服务异常退出: %w", s.name, err)
			}
		}()
		log.Info("监听已就绪", zap.String("server", s.name), zap.String("addr", s.ln.Addr().String()))
	}
	for _, c := range comps {
		if sk, ok := c.(*skeleton); ok {
			log.Info("角色已装配（仅生命周期骨架，尚无功能）", zap.String("role", sk.Name()), zap.String("implemented_by", skeletonOwner[sk.role]))
			continue
		}
		c.Start(ctx)
	}

	// 8. 就绪通知：仅写 $NOTIFY_SOCKET，非 systemd 环境为 no-op。失败不终止进程（Type=notify 由 systemd 超时判定）。
	if sent, err := sdnotify.Notify(sdnotify.Ready); err != nil {
		log.Warn("sd_notify READY=1 失败", zap.Error(err))
	} else if sent {
		log.Info("已通知 systemd：READY=1")
	}
	log.Info("lingedge-server 就绪", zap.String("roles", o.Roles.String()))
	if o.OnReady != nil {
		o.OnReady(info)
	}

	// 9. 等待停机信号或监听异常。
	var runErr error
	select {
	case <-ctx.Done():
		log.Info("收到停机信号，开始优雅停机")
	case runErr = <-errCh:
		log.Error("服务异常，开始停机", zap.Error(runErr))
	}

	// 10. 优雅停机：停接新请求 → 排空 → 停角色 → 关 Redis/DB/主密钥。
	if _, err := sdnotify.Notify(sdnotify.Stopping); err != nil {
		log.Warn("sd_notify STOPPING=1 失败", zap.Error(err))
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), o.ShutdownTimeout)
	defer cancel()
	var errs []error
	if runErr != nil {
		errs = append(errs, runErr)
	}
	for _, s := range servers {
		if err := s.srv.Shutdown(sctx); err != nil { // 立即关闭监听（停接新请求），再等待在途请求完成
			_ = s.srv.Close()
			errs = append(errs, fmt.Errorf("排空 %s 超时或失败，已强制关闭: %w", s.name, err))
		}
		log.Info("监听已停止并排空", zap.String("server", s.name))
	}
	for i := len(comps) - 1; i >= 0; i-- {
		if err := comps[i].Shutdown(sctx); err != nil {
			errs = append(errs, fmt.Errorf("停止角色 %s: %w", comps[i].Name(), err))
		}
	}
	if err := closeAll(sctx); err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		log.Error("停机完成但有错误", zap.Error(err))
		return err
	}
	log.Info("优雅停机完成")
	return nil
}

func isLoopbackHost(h string) bool {
	ip := net.ParseIP(h)
	return h == "localhost" || (ip != nil && ip.IsLoopback())
}

// skeletonOwner 登记各骨架角色由哪个 Story 实现（文档与日志都引用它，避免「假装有功能」）。
var skeletonOwner = map[Role]string{
	RoleGRPC: "Story 3.13（gRPC 骨架：Node mTLS :9443 / Enroll :9444）",
}

// skeleton 是尚无真实实现的角色的生命周期骨架：不监听、不调度、不消费，仅占位并随进程停止。
type skeleton struct {
	role Role
	log  *zap.Logger
}

func newSkeleton(r Role, log *zap.Logger) *skeleton { return &skeleton{role: r, log: log} }

func (s *skeleton) Name() string { return string(s.role) }

// Start 实现 component（骨架无后台工作）。
func (s *skeleton) Start(context.Context) {}

func (s *skeleton) Shutdown(context.Context) error {
	s.log.Info("角色骨架已停止", zap.String("role", string(s.role)))
	return nil
}

// workerComponent 是 worker 角色的真实运行时（Story 1.14）：taskrt.Worker 的生命周期封装。
type workerComponent struct {
	w      *taskrt.Worker
	log    *zap.Logger
	cancel context.CancelFunc
	done   chan struct{}
}

func newWorkerComponent(o Options, store taskrt.Store, rc *redisx.Client, log *zap.Logger) (*workerComponent, error) {
	drain := taskrt.DefaultDrainTimeout
	if lim := o.ShutdownTimeout * 5 / 6; lim < drain { // 排空必须在进程停机预算内结束，留出归还与关闭资源的时间
		drain = lim
	}
	w, err := taskrt.NewWorker(taskrt.WorkerConfig{
		Store: store, Registry: o.Tasks, Logger: log, Redis: rc, Clock: o.Clock, DrainTimeout: drain,
		Owner: taskrt.NewOwner(uid.NewGenerator(o.Clock)),
		Scope: tenantscope.PlatformForJob(0), // [tenantscope-mint] Worker 是跨租户的平台进程；处理函数按任务的 tenant_id 获得各自的 Scope
	})
	if err != nil {
		return nil, err
	}
	return &workerComponent{w: w, log: log, done: make(chan struct{})}, nil
}

func (c *workerComponent) Name() string { return string(RoleWorker) }

// Start 在后台运行 Worker。其 ctx 与进程 ctx 解耦：SIGTERM 时由 Shutdown 触发停止，使排空发生在监听关闭之后的统一停机阶段。
func (c *workerComponent) Start(ctx context.Context) {
	wctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c.cancel = cancel
	c.log.Info("角色已装配（任务运行时）", zap.String("role", c.Name()), zap.String("owner", c.w.Owner()))
	go func() {
		defer close(c.done)
		if err := c.w.Run(wctx); err != nil {
			c.log.Error("任务运行时异常退出", zap.Error(err))
		}
	}()
}

// Shutdown 停止领取并等待在途任务排空（至多 ctx 的剩余预算；Worker 自身有 DrainTimeout，超时会归还任务）。
func (c *workerComponent) Shutdown(ctx context.Context) error {
	if c.cancel == nil {
		return nil
	}
	c.cancel()
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("任务运行时未在停机预算内退出: %w", ctx.Err())
	}
}

func newSchedulerComponent(o Options, app *db.AppPool, tasks taskrt.Store, idem idempotency.Store, enq *taskrt.Enqueuer, deps Deps, metrics *httpx.Metrics, log *zap.Logger) (*schedulerComponent, error) {
	ids := uid.NewGenerator(o.Clock)
	locks, err := schedrepo.NewLocks(app)
	if err != nil {
		return nil, err
	}
	sessions, err := schedrepo.NewSessions(app)
	if err != nil {
		return nil, err
	}
	sc := tenantscope.PlatformForJob(0) // [tenantscope-mint] Scheduler 是跨租户的平台进程：入队与清理都以平台 Scope 进行
	d := builtin.Deps{Scope: sc, Clock: o.Clock, Logger: log, Tasks: tasks, Idempotency: idem, Sessions: sessions}
	var maint *lazyPartitions
	if open := maintOpener(o, o.Config.Database); open != nil {
		maint = &lazyPartitions{open: open}
		d.Partitions = maint
	} else {
		log.Warn("未配置 maint 账号（database.maint.*）或装配层未提供 OpenMaint：partition.maintain 每次触发都将明确跳过并告警，分区不会滚动预建（写入不受影响，数据落入 pmax 兜底分区）",
			zap.Bool("alert", true))
	}
	reg := schedrt.NewRegistry()
	if err := builtin.Register(reg, d); err != nil {
		return nil, err
	}
	if o.Cron != nil {
		if err := o.Cron(reg, deps); err != nil {
			return nil, fmt.Errorf("登记周期任务失败: %w", err)
		}
	}
	r, err := schedrt.NewRunner(schedrt.Config{
		Locks: locks, Registry: reg, Enqueuer: enq, Scope: sc, Logger: log, Clock: o.Clock,
		Instance: taskrt.NewOwner(ids), LeaseTTL: o.SchedulerLeaseTTL, Tick: o.SchedulerTick,
	})
	if err != nil {
		return nil, err
	}
	metrics.RegisterGauge("lingedge_scheduler_is_leader", "本实例是否持有 scheduler.leader（1=是）", func() float64 {
		if r.IsLeader() {
			return 1
		}
		return 0
	})
	metrics.RegisterGauge("lingedge_scheduler_job_failures", "周期作业累计失败次数", func() float64 { return float64(r.Failures()) })
	metrics.RegisterGauge("lingedge_scheduler_job_skips", "周期作业累计明确跳过次数（如缺少 maint 账号）", func() float64 { return float64(r.Skips()) })
	return &schedulerComponent{r: r, log: log, maint: maint, done: make(chan struct{})}, nil
}
