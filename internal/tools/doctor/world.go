package doctor

import (
	"context"
	"crypto/x509"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/backup"
	"github.com/wuekevin/lingedge/internal/tools/cfgcheck"
	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/probe"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

// Component 是 state.json 里的一个已装组件（与 cli.ComponentInfo 同源）。
type Component struct {
	Name       string
	Version    string
	State      string
	Unit       string
	RunUser    string
	Ports      []int
	ReleaseDir string
	ConfigDir  string
	DataDir    string
}

// Config 是 doctor 用到的阈值（来自 tools.yaml，installer.ToolsConfig 解析后由调用方填入；零值 = 缺省）。
type Config struct {
	MinMemoryMB        int64
	MinDiskMB          int64
	NofileMin          uint64
	TimeSkewWarn       time.Duration
	TimeSkewFail       time.Duration
	RestartsWarn       int
	BackupDir          string
	BackupMaxAge       time.Duration
	CustodyFingerprint string
	Offline            bool
}

func (c Config) withDefaults() Config {
	if c.MinMemoryMB <= 0 {
		c.MinMemoryMB = 256
	}
	if c.MinDiskMB <= 0 {
		c.MinDiskMB = 512
	}
	if c.NofileMin == 0 {
		c.NofileMin = 4096
	}
	if c.TimeSkewWarn <= 0 {
		c.TimeSkewWarn = 5 * time.Second
	}
	if c.TimeSkewFail <= 0 {
		c.TimeSkewFail = 5 * time.Minute
	}
	if c.RestartsWarn <= 0 {
		c.RestartsWarn = 3
	}
	if c.BackupMaxAge <= 0 {
		c.BackupMaxAge = 168 * time.Hour
	}
	if c.BackupDir == "" {
		c.BackupDir = backup.DefaultBackupDir
	}
	return c
}

// ClockMark 是 Tools 自己记录过的时间点（state.json 的 updated_at/修改时间、最近的 journal、备份创建时间……）：
// 当前时钟早于其中任何一个（超过容差）就说明时钟被回拨过。
type ClockMark struct {
	Name string
	T    time.Time
}

// MySQLSettings 是 MySQL 服务端设置（dep.mysql 的 utf8mb4/sql_mode 检查）。
type MySQLSettings struct {
	CharacterSet string
	Collation    string
	SQLMode      string
	Version      string
}

// PartitionRow 是一张分区表在 pmax 兜底分区里的行数（dep.mysql.pmax）。
type PartitionRow struct {
	Table string
	Rows  int64
}

// ConfigVerdict 是一个配置目标的校验结果（调用方用 `config validate` 同一实现产出）。
type ConfigVerdict struct {
	Target   string
	File     string
	Problems []cfgcheck.Problem
	// Notes 是不影响结果的说明（如 tools.yaml 不存在、使用缺省配置）。
	Notes []string
	// Partial 非空 = 只做了部分校验（原因），不能当作完整通过。
	Partial string
	// Unavailable 非空 = 无法校验（原因，如无权读取文件），结果为 skip。
	Unavailable string
}

// World 是 doctor 看到的本机：全部是只读视图或可注入的函数，检查项代码里没有任何直接的系统调用以外的全局状态。
// 零值的函数字段表示「该来源不可用」，对应检查项报告 skip（说明原因），而不是 panic 或猜测。
type World struct {
	// Root 非空时 /etc /opt /var 路径前缀到该目录（测试用；生产为空）。
	Root string
	Now  func() time.Time
	Euid int
	// FixPerms 为 true 时 fs.* 检查项修复明确定义的权限偏差（需 root；DryRun 时只报告将做什么）。
	FixPerms bool
	DryRun   bool

	Components []Component
	// StateErr 是读取 state.json 失败的错误（非 nil 时 Components 不可信）。
	StateErr error
	// StateDir 是 Tools 状态目录（state.json 所在）。
	StateDir string

	Cfg Config

	// Probe 采集系统信息（probe.Collect）；结果在一次运行内缓存。
	Probe func(ctx context.Context) (probe.Result, error)
	// Systemctl 为 nil = 无 systemd（svc.* 报告 skip）。
	Systemctl *service.Systemctl
	Own       service.Ownership

	// HTTPGet 对 url 发 GET，返回状态码（连接失败返回 error）。
	HTTPGet func(ctx context.Context, url string) (int, error)
	// TimeSync 报告时间同步状态；ClockMarks 列出 Tools 记录过的时间点。
	TimeSync   func(ctx context.Context) (TimeInfo, error)
	ClockMarks func() []ClockMark
	// PeerCerts 对 addr 做一次 TLS 握手并返回对端出示的证书链（只读诊断，不发送任何数据、不信任）。
	PeerCerts func(ctx context.Context, addr string) ([]*x509.Certificate, error)
	// ConfigVerify 校验 target（"tools" 或组件名）的配置文件。
	ConfigVerify func(ctx context.Context, target string) ConfigVerdict
	// Journals 返回 journal 事务摘要（registry.ListJournals）。
	Journals func(ctx context.Context) ([]registry.TxInfo, error)
	// Backups 列出备份目录里的备份；MasterKeyFingerprint 返回 master.key 的整体指纹（只读）。
	Backups              func() ([]backup.ListEntry, error)
	MasterKeyFingerprint func() (string, error)
	// Manifest 获取并验证发行清单（只读，不推进 sequence）；nil = 离线/未配置。
	Manifest func(ctx context.Context) (*manifest.Manifest, error)
	// MySQLSettings 以迁移账号查询 MySQL 服务端设置；nil/err = 无凭据，dep.mysql.settings 报告 skip。
	MySQLSettings func(ctx context.Context) (MySQLSettings, error)
	// PmaxRows 以迁移账号列出有 pmax 分区的表及其 pmax 里的行数；nil/err = 无凭据（dep.mysql.pmax 报告 skip）。
	PmaxRows func(ctx context.Context) ([]PartitionRow, error)
	// OpenrestyV 返回 `openresty -V` 的输出（stderr+stdout）。
	OpenrestyV func(ctx context.Context) (string, error)
	// Nofile 返回 RLIMIT_NOFILE 软限制。
	Nofile func() (uint64, error)
	// ServerListens 返回 server.yaml 的 http.listen 与 grpc.listen（宽松读取，缺省值兜底）。
	ServerListens func() (httpListen, grpcListen string)
	// FSKind 识别 path 所在文件系统（registry.FSKind）。
	FSKind func(path string) (registry.FSInfo, error)

	probeOnce sync.Once
	probeRes  probe.Result
	probeErr  error

	mfOnce sync.Once
	mfRes  *manifest.Manifest
	mfErr  error

	readyOnce sync.Once
	readyRes  serverHealth
}

func (w *World) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// path 把虚拟绝对路径映射到主机路径。
func (w *World) path(virtual string) string {
	if w.Root == "" {
		return virtual
	}
	return filepath.Join(w.Root, virtual)
}

func (w *World) cfg() Config { return w.Cfg.withDefaults() }

func (w *World) probe(ctx context.Context) (probe.Result, error) {
	w.probeOnce.Do(func() {
		if w.Probe == nil {
			w.probeErr = errNoProbe
			return
		}
		w.probeRes, w.probeErr = w.Probe(ctx)
	})
	return w.probeRes, w.probeErr
}

// component 返回已装组件。
func (w *World) component(name string) (Component, bool) {
	for _, c := range w.Components {
		if c.Name == name {
			return c, true
		}
	}
	return Component{}, false
}

// installed 返回（按名排序的）已装组件，filter 非空时只含该组件。
func (w *World) installed(f Filter) []Component {
	var out []Component
	for _, c := range w.Components {
		if f.Component == "" || c.Name == f.Component {
			out = append(out, c)
		}
	}
	return out
}

type doctorErr string

func (e doctorErr) Error() string { return string(e) }

const errNoProbe = doctorErr("系统探测不可用")

// loopbackize 把监听地址变成可连接的本机地址（通配 → 127.0.0.1）。
func loopbackize(listen string) string {
	h, p, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if ip := net.ParseIP(strings.Trim(h, "[]")); h == "" || (ip != nil && ip.IsUnspecified()) {
		h = "127.0.0.1"
	}
	return net.JoinHostPort(h, p)
}

// manifestOnce 在一次运行内只取一次清单（sys 的要求、ver.*、trust.* 共用）。离线/未配置返回 (nil, nil)。
func (w *World) manifestOnce(ctx context.Context) (*manifest.Manifest, error) {
	w.mfOnce.Do(func() {
		if w.Manifest == nil || w.Cfg.Offline {
			return
		}
		w.mfRes, w.mfErr = w.Manifest(ctx)
	})
	return w.mfRes, w.mfErr
}

// NofileLimit 返回本进程 RLIMIT_NOFILE 的软限制（World.Nofile 的生产实现）。
func NofileLimit() (uint64, error) { return nofileLimit() }

// CheckServerHealth 返回 health.server 的检查结果（status 命令复用同一判定，保证 doctor 与 status 一致）。Server 未安装时返回 skip。
func (w *World) CheckServerHealth(ctx context.Context) Finding {
	if _, ok := w.component("server"); !ok {
		return fnd("health.server", LevelSkip, "未安装 server", "")
	}
	return healthServer(ctx, w)
}

// CheckFrontend 返回静态前端组件的深度检查结果（fe.<c>.release、fe.<c>.nginx）。
func (w *World) CheckFrontend(c Component) []Finding {
	return []Finding{feRelease(w, c), feNginx(w, c)}
}
