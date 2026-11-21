package setup

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/dbcred"
	setuprepo "github.com/wuekevin/lingedge/internal/platform/setup/repository"
	"github.com/wuekevin/lingedge/pkg/secretbox"
)

// LockPath 返回 install.lock 的路径（<server.data_dir>/install.lock）。
func LockPath(cfg *config.Config) string { return filepath.Join(cfg.Server.DataDir, LockFileName) }

// DetectInstalled 判定 Server 应以正常模式还是 setup mode 启动（Story 1.24 AC1）：
//   - install.lock 存在 → 已安装（正常模式）；
//   - 否则，若运行期凭据可解析、数据库可连且 system_installations 里有完成的安装记录（install_locked_at 非空）→ 也视为已安装，并补写缺失的 install.lock
//     （完成事务已提交、进程在写 lock 之前退出的自愈）；
//   - 其余情况 → setup mode（含数据库不可达：此时无从证明已安装，宁可进入只提供向导的受限模式，也不把一个未初始化的库当作生产）。
//
// box 可为 nil（主密钥不可用时无法解密运行期凭据，自愈检查被跳过）。
func DetectInstalled(ctx context.Context, cfg *config.Config, box *secretbox.Box, log *zap.Logger, clk clock.Clock) bool {
	lock := LockPath(cfg)
	if _, err := os.Stat(lock); err == nil {
		return true
	}
	merged := *cfg
	if _, err := dbcred.Merge(ctx, &merged, box, dbcred.MergeOptions{}); err != nil {
		log.Info("无 install.lock，且运行期凭据不可用：进入 setup mode", zap.Error(err))
		return false
	}
	d := merged.Database
	if d.Host == "" || d.Name == "" || d.User == "" || !d.Password.IsSet() {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	app, err := db.OpenApp(cctx, db.AppConfigFrom(d))
	if err != nil {
		log.Info("无 install.lock，数据库不可用于判定安装状态：进入 setup mode", zap.Error(err))
		return false
	}
	defer func() { _ = app.Close() }()
	repo, err := setuprepo.New(app)
	if err != nil {
		return false
	}
	done, err := repo.Completed(cctx)
	if err != nil || !done {
		return false
	}
	if err := WriteLockFile(lock, clk.Now()); err != nil {
		log.Warn("数据库记录显示安装已完成，但补写 install.lock 失败（以正常模式继续）", zap.Error(err))
	} else {
		log.Warn("数据库记录显示安装已完成，但 install.lock 缺失：已补写（上次 complete 在写 lock 之前被中断）")
	}
	return true
}
