package resolver

import (
	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/registry"
)

// Want 是用户选择的一个组件。
type Want struct {
	Component string `json:"component"`
	Version   string `json:"version,omitempty"` // 固定版本（--version，严格 SemVer）；空 = 通道内最高稳定版
	Variant   string `json:"variant,omitempty"` // 变体（server 的 default、website 的 static/ssr）；空 = 唯一变体或 default
}

// PortOverride 是 `--set` 换端口：把 Component 的 Proto/Port 改为 To（Bind 非空时同时改绑定地址）。
type PortOverride struct {
	Component string `json:"component"`
	Proto     string `json:"proto"`
	Port      int    `json:"port"`
	To        int    `json:"to"`
	Bind      string `json:"bind,omitempty"`
}

// Layout 是目录布局（tools-design §8）；零值字段取默认值。
type Layout struct {
	OptRoot     string `json:"opt_root,omitempty"`     // 默认 /opt/lingedge：release 与 current
	EtcRoot     string `json:"etc_root,omitempty"`     // 默认 /etc/lingedge：配置
	VarRoot     string `json:"var_root,omitempty"`     // 默认 /var/lib/lingedge：数据
	UnitDir     string `json:"unit_dir,omitempty"`     // 默认 /etc/systemd/system
	StagingRoot string `json:"staging_root,omitempty"` // 默认 /var/lib/lingedge/tools/staging（downloader 的 staging，磁盘需求计入其挂载点）
}

func (l Layout) withDefaults() Layout {
	d := func(v *string, def string) {
		if *v == "" {
			*v = def
		}
	}
	d(&l.OptRoot, "/opt/lingedge")
	d(&l.EtcRoot, "/etc/lingedge")
	d(&l.VarRoot, "/var/lib/lingedge")
	d(&l.UnitDir, "/etc/systemd/system")
	d(&l.StagingRoot, "/var/lib/lingedge/tools/staging")
	return l
}

// Input 是求解输入。
type Input struct {
	Want []Want
	// Installed 是 state.json 的内容（registry.State）。
	Installed registry.State
	// InstalledVariants 记录已装组件的变体（可空）：state.json schema v1 不含变体，由编排层（1.33）在知道时提供；
	// 缺失时，被动升级候选若有多个变体且无 default，会被拒绝并提示「请显式选择该组件及变体」。
	InstalledVariants map[string]string
	// Manifest 必须来自 manifest.ParseManifest / Verifier.Verify（版本已解析、吊销表已就绪）。
	Manifest *manifest.Manifest
	System   System
	Layout   Layout

	PortOverrides []PortOverride
	// AcceptPassiveUpgrade 表示用户已确认「被动升级」已装组件；未确认时 Plan 含 confirm_required 阻断（退出码 9）。
	AcceptPassiveUpgrade bool
}

// System 是系统探测结果——**Story 1.31 的产出契约**：1.31 把真实探测填进这个结构，resolver 不做任何探测。
type System struct {
	OS   string `json:"os"`   // runtime.GOOS 形式：linux
	Arch string `json:"arch"` // amd64 | arm64

	// 内存（MiB）。MemoryAvailableMB 来自 /proc/meminfo 的 MemAvailable；资源判定用它（已运行的进程已被扣除）。
	// 需要内存但此值为 0（未探测到）→ 阻断，而不是放行。
	MemoryTotalMB     int64 `json:"memory_total_mb"`
	MemoryAvailableMB int64 `json:"memory_available_mb"`

	// Disks 是各挂载点的可用空间（statfs 的 f_bavail × f_frsize，MiB）。磁盘需求按目标路径的最长前缀挂载点分别统计。
	Disks []Disk `json:"disks"`

	// Listeners 是本机监听（ss -H -lntup / /proc/net/* 的解析结果），含地址；tcp6/udp6 归一为 tcp/udp，地址保留 IPv6 原文。
	Listeners []Listener `json:"listeners"`

	// Deps 是已探测的外部依赖与运行时，键为规范名：mysql、redis、nodejs（requires.runtime 的 node 映射到 nodejs）。
	// 键缺失或 Detected=false 都表示「未探测到」。
	Deps map[string]Dep `json:"deps"`

	// Baota 是宝塔探测结果。宝塔存在本身不阻止安装（FR-7）；resolver 只用它丰富端口冲突的处置建议并给出提示。
	Baota Baota `json:"baota"`
}

// Disk 是一个挂载点。
type Disk struct {
	Mount    string `json:"mount"`
	FSType   string `json:"fs_type,omitempty"`
	TotalMB  int64  `json:"total_mb,omitempty"`
	FreeMB   int64  `json:"free_mb"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// Listener 是一个本机监听。
type Listener struct {
	Proto   string `json:"proto"` // tcp | udp（tcp6/udp6 也接受并归一）
	Addr    string `json:"addr"`  // IP 字面量；0.0.0.0、::、* 表示通配
	Port    int    `json:"port"`
	PID     int    `json:"pid,omitempty"`
	Process string `json:"process,omitempty"`
	User    string `json:"user,omitempty"`
	Unit    string `json:"unit,omitempty"` // 所属 systemd 单元（cgroup 推得）；用于识别「这是我们已装组件自己的监听」
}

// Dep 是一个外部依赖的探测结果。
type Dep struct {
	Detected bool   `json:"detected"`
	Version  string `json:"version,omitempty"` // 原始版本串（如 8.0.36-0ubuntu0.22.04.1、v20.11.0）；resolver 取其数字前缀 x.y.z
	Source   string `json:"source,omitempty"`  // 证据（二进制路径、服务名），仅展示
	Flavor   string `json:"flavor,omitempty"`  // mysql|mariadb 等，仅展示
}

// Baota 是宝塔面板探测结果（对应 state.json.host.baota）。
type Baota struct {
	Detected   bool   `json:"detected"`
	Confidence string `json:"confidence,omitempty"`
	Version    string `json:"version,omitempty"`
	PanelPort  int    `json:"panel_port,omitempty"`
	NginxPath  string `json:"nginx_path,omitempty"`
}

// PlanComponent 是 Plan 中的一个组件。
type PlanComponent struct {
	Name           string                     `json:"name"`
	Action         string                     `json:"action"` // install | upgrade | keep
	Version        string                     `json:"version"`
	Variant        string                     `json:"variant,omitempty"`
	From           string                     `json:"from,omitempty"` // 已装版本（upgrade/keep）
	Wanted         bool                       `json:"wanted"`         // 用户直接选择
	Internal       bool                       `json:"internal,omitempty"`
	PassiveUpgrade bool                       `json:"passive_upgrade,omitempty"` // 非用户选择而因约束被迫升级（需确认）
	RequiredBy     string                     `json:"required_by,omitempty"`
	Platform       string                     `json:"platform,omitempty"` // 选中的清单平台键（linux-amd64 | any ...）
	URL            string                     `json:"url,omitempty"`
	SHA256         string                     `json:"sha256,omitempty"`
	Size           int64                      `json:"size,omitempty"`
	RunUser        string                     `json:"run_user,omitempty"`
	Migrations     *manifest.MigrationsNotice `json:"migrations,omitempty"`
}

// Step 是 Plan 的高层步骤（与 tools-design §12.1 的 22 步映射由 1.33 编排完成）。
type Step struct {
	Seq       int    `json:"seq"`
	Component string `json:"component"`
	Kind      string `json:"kind"` // download|extract|prepare|configure|units|activate|start|health|record
	Summary   string `json:"summary"`
}

// PlannedPort 是 Plan 中占用的一个端口。
type PlannedPort struct {
	Component   string `json:"component"`
	Proto       string `json:"proto"`
	Port        int    `json:"port"`
	Bind        string `json:"bind"`
	Purpose     string `json:"purpose,omitempty"`
	Conditional bool   `json:"conditional,omitempty"`
	Overridden  bool   `json:"overridden,omitempty"`
}

// Unit 是将安装的 systemd 单元。
type Unit struct {
	Component string `json:"component"`
	Name      string `json:"name"`
	Kind      string `json:"kind"` // service | path
	Path      string `json:"path"`
	RunUser   string `json:"run_user,omitempty"`
}

// 文件策略。
const (
	PolicyCreateIfAbsent = "create_if_absent" // 不存在才创建，已存在绝不覆盖（配置、密钥）
	PolicyManaged        = "managed"          // Tools 拥有，每次安装/升级重写（单元、Nginx 片段、release 目录）
)

// File 是将创建的文件/目录/符号链接。
type File struct {
	Component string `json:"component"`
	Path      string `json:"path"`
	Kind      string `json:"kind"` // file | dir | symlink
	Mode      string `json:"mode"`
	Owner     string `json:"owner"`
	Policy    string `json:"policy"`
	Note      string `json:"note,omitempty"`
}

// Warning 是不阻止执行的提示。
type Warning struct {
	Code      string `json:"code"`
	Component string `json:"component,omitempty"`
	Message   string `json:"message"`
}

// Blocker 是阻止执行的原因。
type Blocker struct {
	Kind      Kind           `json:"kind"`
	Code      string         `json:"code"`
	Component string         `json:"component,omitempty"`
	Message   string         `json:"message"`
	Hint      string         `json:"hint,omitempty"`
	Chain     []string       `json:"chain,omitempty"` // 冲突链（无解时从用户选择一路列到无候选的组件）
	Details   map[string]any `json:"details,omitempty"`
}

// DiskUsage 是某挂载点的需求与余量。
type DiskUsage struct {
	Mount    string `json:"mount"`
	NeededMB int64  `json:"needed_mb"`
	FreeMB   int64  `json:"free_mb"`
}

// Resources 是资源统计。
type Resources struct {
	MemoryNeededMB    int64       `json:"memory_needed_mb"`
	MemoryAvailableMB int64       `json:"memory_available_mb"`
	Disks             []DiskUsage `json:"disks"`
}

// Plan 是求解结果。Blockers 非空时不得执行（--dry-run 同样展示它）。
type Plan struct {
	Components []PlanComponent `json:"components"`
	Order      []string        `json:"order"`
	Steps      []Step          `json:"steps"`
	Ports      []PlannedPort   `json:"ports"`
	Units      []Unit          `json:"units"`
	Files      []File          `json:"files"`
	Warnings   []Warning       `json:"warnings"`
	Blockers   []Blocker       `json:"blockers"`
	Resources  Resources       `json:"resources"`
}

// Executable 报告 Plan 是否可执行（没有阻断）。
func (p *Plan) Executable() bool { return len(p.Blockers) == 0 }
