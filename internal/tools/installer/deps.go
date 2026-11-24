package installer

import (
	"context"
	"path/filepath"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/baota"
	"github.com/wuekevin/lingedge/internal/tools/downloader"
	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/probe"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

// Journal 是 installer 对事务日志的使用面（*registry.Journal 实现它）。抽成接口只为了 journal 故障注入测试
// （Deps.WrapJournal 在真实 journal 外包一层，在第 N 次调用处返回错误）；生产路径永远是 *registry.Journal 本身。
type Journal interface {
	ID() string
	Begin(step int, name string, undo registry.Undo) error
	Done(step int) error
	Undone(step int) error
	Commit() error
	Abort(reason string) error
	Archive() error
	Close()
}

// UndoApplier 执行一条撤销动作（生产实现是 *Undoer：先交给 service.Undoer，再处理 installer 自己的动作）。
type UndoApplier interface {
	Apply(ctx context.Context, a registry.UndoAction) (service.UndoResult, error)
}

// Prompt 是需要用户确认的事项。
type Prompt struct {
	Kind    string // passive_upgrade | conflicts | port_unverified | nginx_link
	Message string
	Items   []string
}

// Deps 是 installer 的全部外部依赖。生产由 cli 装配真实实现；测试注入临时根目录（Root）、伪二进制 Runner 等——
// 被测的业务逻辑（状态机、journal、撤销、冲突判定）不被替换。
type Deps struct {
	// Root 是虚拟根前缀：非空时所有 /etc、/opt、/var 路径前缀到该目录（测试用；生产为空）。registry 的 Layout 与 downloader 的 staging 另由 Reg 决定。
	Root string
	Reg  *registry.Registry
	// Verifier 构造清单验证器（内置公钥 + trust/keys.d + trust/state）。installer 会把其 Seen 包进 registry.LockedSequence（跨进程全局锁，1.29 的约定）。
	Verifier func() (*manifest.Verifier, error)
	Config   ToolsConfig
	Specs    SpecSet
	// Collect 做只读系统探测（生产为 probe.Collect）。
	Collect func(ctx context.Context) (probe.Result, error)
	// FS 是宝塔/既有环境探测的文件视图（缺省 baota.OSFS{Root: Root 或 "/"}）。
	FS     baota.FS
	Runner service.Runner
	// WebRunner 执行 nginx/openresty（静态前端的 conf.d 软链：nginx -t 与 -s reload，Story 1.40）；nil = service.SystemWebServerRunner()。
	WebRunner service.Runner
	Own       service.Ownership
	// Booted 判定本机是否 systemd 启动（nil = 平台实现）。
	Booted func() bool
	// Geteuid 返回有效 uid（nil = os.Geteuid）；创建/删除运行用户需要 root，非 root 时 service 在动手前就明确失败。
	Geteuid func() int
	Now     func() time.Time
	// NewClient 构造 HTTPS 客户端（缺省 downloader.New）。
	NewClient func(cfg downloader.Config) (*downloader.Client, error)
	// Confirm 向用户提问（交互模式）；nil 或 Request.NonInteractive 时只认 Request.Yes。
	Confirm func(Prompt) (bool, error)
	// Events 接收进度事件；可为 nil。发送是非阻塞的：通道满/被关闭都不影响安装。
	Events chan<- Event

	// RestoreSafety 执行 journal 里的 restore_safety_backup 撤销动作（恢复到「恢复前安全备份」：文件、数据库、按需重启服务）。
	// 它需要数据库凭据与（安全备份加密时的）口令，由 cli 以交互/环境变量取得后装配；nil 时该撤销以明确的错误失败并给出手工恢复指引，绝不假装成功。
	RestoreSafety func(ctx context.Context, a registry.UndoAction) (service.UndoResult, error)

	// Update 是升级/回滚（Story 1.43）的外部依赖装配；nil = 该调用不支持升级（install/uninstall/recover 的普通路径不需要它，但 recover 要撤销升级事务时需要）。
	Update *UpdateEnv

	// SelfPath 返回 Tools 自身可执行文件的真实路径（`update self` 的替换目标）；nil = os.Executable 并解析符号链接。
	SelfPath func() (string, error)
	// SelfRun 执行新版本的 `lingedge.new --version` 自检；nil = 只允许基名 lingedge.new 的 safeexec Runner。
	SelfRun service.Runner

	// Hook 在每个步骤的四个点被同步调用："before:N"（Begin 之前）、"begun:N"（begin 已落盘、尚未动手）、"effect:N"（副作用已完成、done 未写）、
	// "done:N"（done 已落盘）。仅供测试在这些点 kill -9 / 注入故障；生产为 nil。
	Hook func(point string)
	// WrapJournal / WrapUndoer 供故障注入测试包装真实实现；生产为 nil。
	WrapJournal func(Journal) Journal
	WrapUndoer  func(UndoApplier) UndoApplier
}

func (d *Deps) hostPath(virtual string) string {
	if d.Root == "" {
		return virtual
	}
	return filepath.Join(d.Root, virtual)
}

func (d *Deps) validate() error {
	switch {
	case d.Reg == nil:
		return newErr(KindInternal, "Deps.Reg 必填")
	case d.Verifier == nil:
		return newErr(KindInternal, "Deps.Verifier 必填")
	case d.Specs == nil:
		return newErr(KindInternal, "Deps.Specs 必填（生产传 ProductionSpecs()）")
	case d.Collect == nil:
		return newErr(KindInternal, "Deps.Collect 必填")
	case d.Runner == nil:
		return newErr(KindInternal, "Deps.Runner 必填")
	case d.Own == nil:
		return newErr(KindInternal, "Deps.Own 必填")
	case d.Now == nil:
		return newErr(KindInternal, "Deps.Now 必填")
	}
	return nil
}

// Installer 是安装编排器。
type Installer struct {
	d    Deps
	ev   *emitter
	sys  *service.Systemctl
	prov *service.Provisioner
}

// New 构造 Installer。
func New(d Deps) (*Installer, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}
	if d.FS == nil {
		root := d.Root
		if root == "" {
			root = "/"
		}
		d.FS = baota.OSFS{Root: root}
	}
	if d.NewClient == nil {
		d.NewClient = downloader.New
	}
	in := &Installer{d: d, ev: &emitter{ch: d.Events, now: d.Now}}
	in.sys = &service.Systemctl{Run: d.Runner, Booted: d.Booted}
	in.prov = &service.Provisioner{Run: d.Runner, Geteuid: d.Geteuid}
	return in, nil
}

// EventsDropped 返回因通道满/关闭而丢弃的事件数。
func (in *Installer) EventsDropped() int64 { return in.ev.Dropped() }

// Deps 返回构造 Installer 时使用的依赖（测试与装配代码用来取得 Registry 等）。
func (in *Installer) Deps() Deps { return in.d }
