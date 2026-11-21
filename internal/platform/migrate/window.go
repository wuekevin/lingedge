package migrate

import (
	"context"
	"errors"
	"io/fs"
	"sync"

	"github.com/wuekevin/lingedge/internal/platform/db"
)

// ErrWindowClosed：setup 窗口已关闭，不得再构造 migrate 连接池（ADR-026 §2、AD-26）。
var ErrWindowClosed = errors.New("migrate: setup 窗口已关闭，进程内不再允许构造 migrate 连接池（CC-3：Server 不常驻持有 migrate 凭据）")

// Window 是「安装向导期间瞬时持有 migrate 池」的唯一授权句柄（ADR-026 §2）。
//
// 规则：Server 进程里没有任何代码路径可以在 Window 之外构造 RoleMigrate 池——本包是 db.OpenMigrate 的白名单调用点之一
// （forbidigo [db-pool-migrate]），而 Server 侧只有 setup 服务持有 Window，且 Window 仅在 setup mode（install.lock 不存在）下创建；
// 安装完成（complete）时 Close，此后 OpenRunner 一律返回 ErrWindowClosed。门禁测试（internal/platform/setup）扫描全仓库，
// 断言 migrate.Window 之外的 Server 代码不调用 OpenRunner/db.OpenMigrate。
type Window struct {
	mu     sync.Mutex
	closed bool
	open   []*db.MigratePool
}

// NewWindow 创建一个开启的窗口。
func NewWindow() *Window { return &Window{} }

// Closed 报告窗口是否已关闭。
func (w *Window) Closed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closed
}

// OpenRunner 以 cfg 构造 migrate 池并创建 Runner；返回的 closeFn 关闭该池（步骤结束必须调用，随后调用方清零口令）。窗口已关闭返回 ErrWindowClosed。
func (w *Window) OpenRunner(ctx context.Context, cfg db.Config, fsys fs.FS) (*Runner, func(), error) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil, nil, ErrWindowClosed
	}
	w.mu.Unlock()
	cfg.MaxOpenConns, cfg.MaxIdleConns = 4, 2 // 互斥连接 + goose 执行连接 + 状态读写（同 CLI）
	pool, err := db.OpenMigrate(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	r, err := NewRunner(pool, fsys)
	if err != nil {
		_ = pool.Close()
		return nil, nil, err
	}
	w.mu.Lock()
	if w.closed { // 打开期间被关闭
		w.mu.Unlock()
		_ = pool.Close()
		return nil, nil, ErrWindowClosed
	}
	w.open = append(w.open, pool)
	w.mu.Unlock()
	var once sync.Once
	return r, func() { once.Do(func() { _ = pool.Close() }) }, nil
}

// Close 关闭窗口并关闭仍打开的池；幂等。
func (w *Window) Close() {
	w.mu.Lock()
	w.closed = true
	pools := w.open
	w.open = nil
	w.mu.Unlock()
	for _, p := range pools {
		_ = p.Close()
	}
}
