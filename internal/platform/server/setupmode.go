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
	"github.com/wuekevin/lingedge/internal/platform/logx"
	"github.com/wuekevin/lingedge/internal/platform/requestid"
	"github.com/wuekevin/lingedge/internal/platform/sdnotify"
	"github.com/wuekevin/lingedge/internal/platform/setup"
	"github.com/wuekevin/lingedge/internal/platform/uid"
	"github.com/wuekevin/lingedge/migrations"
	"github.com/wuekevin/lingedge/pkg/secretbox"
)

// SetupOptions 是 RunSetup 的参数。
type SetupOptions struct {
	Config *config.Config
	Logger *zap.Logger
	Clock  clock.Clock
	// NewAdmin 由装配层提供（auth 模块的管理员引导适配，platform 层不得 import internal/auth）。
	NewAdmin        func(setup.PostDeps) (setup.AdminService, error)
	ShutdownTimeout time.Duration
	// TokenHashPath/LockPath 缺省取 setup 包的约定值（测试覆盖）。
	TokenHashPath string
	LockPath      string
	// OnReady 在监听就绪且 READY=1 已发出后调用（集成测试据此取得实际监听地址）；可为 nil。
	OnReady func(addr string)
}

// RunSetup 运行 setup mode（Story 1.24 AC1）：只服务 /healthz、/setup 静态页与 /api/v1/public/setup/*，其余 API 一律 503；默认仅监听 127.0.0.1:8080
// （http.listen 的缺省值）。向导 complete 成功（响应已写出）后优雅关闭监听并返回 completed=true，由调用方继续以正常模式装配；
// ctx 被取消（SIGTERM/SIGINT）时返回 completed=false。
//
// 主密钥缺失不阻止进入 setup mode（环境检查会报告它，涉及口令落盘的步骤拒绝）：否则操作者看不到任何可操作的提示。
// setup mode 不装配 app 池/Redis/任务运行时/scheduler：它们在向导完成之前没有数据库可用。
func RunSetup(ctx context.Context, o SetupOptions) (completed bool, err error) {
	if o.Config == nil || o.Logger == nil {
		return false, errors.New("server: setup mode 缺少配置或日志")
	}
	if o.Clock == nil {
		o.Clock = clock.System()
	}
	if o.ShutdownTimeout <= 0 {
		o.ShutdownTimeout = 10 * time.Second
	}
	cfg := o.Config
	ids := uid.NewGenerator(o.Clock)
	bootID, err := ids.New()
	if err != nil {
		return false, fmt.Errorf("server: 生成启动 request-id: %w", err)
	}
	ctx, _ = requestid.NewContext(ctx, bootID)
	log := logx.WithContext(ctx, o.Logger)
	log.Info("lingedge-server 以 setup mode 启动：尚未完成安装，仅提供 /setup 与 /api/v1/public/setup/*，其余 API 返回 503",
		zap.String("listen", cfg.HTTP.Listen), zap.Bool("loopback_only", config.IsLoopbackListen(cfg.HTTP.Listen)))
	if !config.IsLoopbackListen(cfg.HTTP.Listen) {
		log.Warn("setup mode 监听非回环地址：携带口令的向导接口须经 TLS（直接 TLS 或可信反代的 X-Forwarded-Proto: https），否则返回 400 setup.insecure_transport；请确认已用 TLS 反代保护 setup token")
	}

	var box *secretbox.Box
	keys, kerr := secretbox.NewFileKeyProvider(secretbox.FileOptions{Path: cfg.Secrets.MasterKeyFile, CredentialsDir: secretbox.CredentialsDirFromEnv()})
	if kerr != nil {
		log.Warn("主密钥不可用：向导环境检查将报告该问题，涉及口令落盘的步骤会被拒绝", zap.Error(kerr))
	} else {
		defer keys.Close()
		if box, err = secretbox.New(keys); err != nil {
			return false, err
		}
	}

	if o.TokenHashPath == "" {
		o.TokenHashPath = cfg.Secrets.SetupTokenHashFile // 空则取 setup 包的约定缺省 /etc/lingedge/server/setup-token.hash
	}
	svc, err := setup.New(setup.Options{Config: cfg, Box: box, Logger: o.Logger, Clock: o.Clock, IDs: ids, Migrations: migrations.FS, NewAdmin: o.NewAdmin,
		TokenHashPath: o.TokenHashPath, LockPath: o.LockPath})
	if err != nil {
		return false, err
	}
	defer svc.Close()
	h, err := svc.BuildHandler(o.Logger, o.Clock, ids)
	if err != nil {
		return false, fmt.Errorf("装配 setup 路由失败: %w", err)
	}
	ln, err := net.Listen("tcp", cfg.HTTP.Listen)
	if err != nil {
		return false, fmt.Errorf("setup 监听 %s 失败: %w", cfg.HTTP.Listen, err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 35 * time.Minute, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10}
	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	log.Info("监听已就绪", zap.String("server", "setup"), zap.String("addr", ln.Addr().String()))
	if sent, err := sdnotify.Notify(sdnotify.Ready); err != nil {
		log.Warn("sd_notify READY=1 失败", zap.Error(err))
	} else if sent {
		log.Info("已通知 systemd：READY=1")
	}
	if o.OnReady != nil {
		o.OnReady(ln.Addr().String())
	}

	var runErr error
	select {
	case <-ctx.Done():
		log.Info("收到停机信号，setup mode 退出（安装未完成）")
	case runErr = <-errCh:
		log.Error("setup 服务异常退出", zap.Error(runErr))
	case <-svc.Done():
		completed = true
		log.Info("安装向导已完成：退出 setup mode，切换为正常模式")
	}
	if _, err := sdnotify.Notify(sdnotify.Stopping); err != nil {
		log.Warn("sd_notify STOPPING=1 失败", zap.Error(err))
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), o.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
		runErr = errors.Join(runErr, fmt.Errorf("排空 setup 监听超时: %w", err))
	}
	return completed, runErr
}
