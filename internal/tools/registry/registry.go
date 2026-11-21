package registry

import (
	"os"
	"time"

	"github.com/oklog/ulid/v2"
)

// Options 是 Registry 的进程环境量；零值取生产缺省。测试注入固定时钟/主机名，不替换任何业务逻辑。
type Options struct {
	Now      func() time.Time
	Hostname func() (string, error)
	PID      int    // 缺省 os.Getpid()
	RunID    string // 本次 Tools 运行的 run_id（写入锁文件与 journal，便于对照日志）
	Command  string // 本次命令（如 "install server"；经脱敏后写入）
	// GlobalWait 是全局锁（短临界区）的最长等待，缺省 5s。组件锁永不等待。
	GlobalWait time.Duration

	onAcquire func(name string)      // 测试钩子：观察组件锁的获取顺序
	syncHook  func(f *os.File) error // 测试钩子：包装（不替代）journal 的 fsync，用于计数
}

// Registry 是对一个状态目录（Layout）的访问入口。
type Registry struct {
	L   Layout
	opt Options
}

// New 构造 Registry。
func New(l Layout, o Options) *Registry {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Hostname == nil {
		o.Hostname = os.Hostname
	}
	if o.PID == 0 {
		o.PID = os.Getpid()
	}
	if o.GlobalWait <= 0 {
		o.GlobalWait = 5 * time.Second
	}
	return &Registry{L: l, opt: o}
}

func (r *Registry) now() time.Time { return r.opt.Now() }

// NewTxID 生成事务 ID（ULID：时间有序、可作文件名、满足 ValidTxID）。
func NewTxID(t time.Time) string {
	return ulid.MustNew(ulid.Timestamp(t), ulid.DefaultEntropy()).String()
}

func (r *Registry) holder(scope string) Holder {
	host, _ := r.opt.Hostname()
	return Holder{
		PID: r.opt.PID, RunID: r.opt.RunID, Command: cleanText(r.opt.Command),
		StartedAt: r.now().UTC().Format("2006-01-02T15:04:05.000Z"), Host: host,
		StartTicks: procStartTicks(r.opt.PID), Scope: scope,
	}
}
