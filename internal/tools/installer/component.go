package installer

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"sort"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/resolver"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

// ComponentSpec 是 installer 对「具体组件」的唯一依赖面：resolver 的部署画像只描述落地形态（用户/单元/文件路径），
// 而这些只有组件自己知道——哪些包内文件要可执行、初始配置文件的内容、怎样启动、怎样判定健康。
// 具体组件由后续 Story 实现并在 ProductionSpecs 登记（1.35 server、1.40 静态前端、Epic 3 node 等）；
// 生产构建只登记已交付的组件（见 ProductionSpecs），未登记的组件 install 会以 install.component_spec_missing 明确失败。
// 测试用的假组件只存在于 _test.go（G-11，由构建符号检查测试证明生产二进制不含它们）。
//
// 契约：
//   - 方法不得修改 installer 传入的任何对象；不得写 installer 管辖之外的系统位置（installer 为其 journal 写了 undo 的只有它自己的步骤）。
//   - InitConfig 只返回内容，不写文件：落盘（O_EXCL、先 chown 后写、不覆盖、冲突写 .new、journal undo）由 installer 的第 18 步统一负责。
//   - Start 在 installer 已记录「停止这些单元」的 undo 之后调用，可以安全地中途失败。
//   - Health 必须幂等、只读；installer 在窗口期内按间隔重复调用直到成功或超时。
type ComponentSpec interface {
	// Name 是组件名（与清单/resolver 一致，如 "server"）。
	Name() string
	// Executables 返回包内需要 0755 的 payload 路径（相对包根，如 "payload/lingedge-server"）。冻结的 META/package.json 没有可执行位字段
	// （tools-design §9.3/§18.1），按组件约定在此提供，installer 传给 downloader.Params.Executable。
	Executables(version, variant string) []string
	// ValidateSettings 校验 --set 里**非端口**的 key=value（经 schema 校验，非任意文本注入）；不认识的键/非法值返回错误（退出码 2）。
	ValidateSettings(settings map[string]string) error
	// InitConfig 返回步骤 18 要为本组件创建的配置文件内容。Path 必须是 resolver.Plan.Files 里本组件策略为 create_if_absent 的文件；
	// server 的 server.yaml/master.key/setup-token.hash 由 installer 内建生成（RenderServerYAML/GenerateMasterKey/setuptoken），不要在此返回。
	InitConfig(ctx context.Context, in ConfigInput) ([]ConfigFile, error)
	// Start 启动组件（步骤 19）。典型实现：rt.StartUnits(ctx)，并在前后做组件特有动作。
	Start(ctx context.Context, rt *Runtime) error
	// Health 探测组件是否健康（步骤 20）。
	Health(ctx context.Context, rt *Runtime) error
}

// ConfigInput 是 InitConfig 的输入。路径都是虚拟的绝对路径（生产即真实路径）。
type ConfigInput struct {
	Component  string
	Version    string
	Variant    string
	Settings   map[string]string
	Ports      []resolver.PlannedPort
	RunUser    string
	ConfigDir  string // /etc/lingedge/<c>
	DataDir    string // 空 = 无数据目录
	ReleaseDir string
	CurrentDir string
	Credential service.CredentialMode
	// Node 是 Node.js 运行时（website ssr 等需要）：PATH 上没有时是绝对路径（probe 的 dep.nodejs.not_in_path 与 NodeInstalls，Story 1.31/1.31b）。
	Node NodeRuntime
	// SetupListen 仅 server：安装向导监听地址。
	SetupListen string
}

// ConfigFile 是要创建的一个配置文件。Owner/Group 空则取 resolver.Plan.Files 声明的属主（"user:group"）；Mode 为 0 则取声明的权限。
type ConfigFile struct {
	Path    string
	Content []byte
	Mode    fs.FileMode
	Owner   string
	Group   string
}

// NodeRuntime 描述 Node.js 的位置（接口先备好，website 组件属后续 Story）。
type NodeRuntime struct {
	Detected bool
	// Bin 是应当用来启动的 node：PATH 上的为 "node"，非固定目录（nvm/fnm/volta/…）安装为绝对路径。
	Bin     string
	Version string
	// InPath 为 false 表示 Bin 是非固定目录的绝对路径，启动 website 时必须使用它（dep.nodejs.not_in_path）。
	InPath bool
}

// Runtime 是 Start/Health 的运行上下文。
type Runtime struct {
	Component string
	Version   string
	Variant   string
	Ports     []resolver.PlannedPort
	// Units 是本组件会被启动的单元（service.EnableTargets）。
	Units       []service.UnitName
	Node        NodeRuntime
	SetupListen string
	// Root 是虚拟根前缀（测试用；生产为空）。
	Root string

	start func(ctx context.Context, units ...service.UnitName) error
}

// StartUnits 经 service.Systemctl 启动 Units（撤销已由 installer 预先记录）。
func (rt *Runtime) StartUnits(ctx context.Context) error {
	if rt.start == nil {
		return fmt.Errorf("installer: Runtime 未绑定 systemctl")
	}
	if len(rt.Units) == 0 {
		return nil
	}
	return rt.start(ctx, rt.Units...)
}

// SpecSet 是可用的组件规格。
type SpecSet map[string]ComponentSpec

// Names 返回已登记的组件名（字典序）。
func (s SpecSet) Names() []string {
	out := make([]string, 0, len(s))
	for n := range s {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ProductionSpecs 返回生产构建的组件规格：server（Story 1.35）与静态前端 admin-web/console/docs/status/website（Story 1.40，同一框架，website 仅 static 变体）；
// 其余组件（node、dns、reporter 等，Epic 3 与 Epic 2）由后续 Story 在此登记，未登记的组件 install 以 install.component_spec_missing 明确失败。
// 刻意不用包级可变注册表——那会让测试里的假组件有机会经全局状态混进生产路径。
func ProductionSpecs() SpecSet {
	out := staticSpecs()
	out["server"] = serverSpec{}
	return out
}

// HTTPOK 对 url 发 GET，2xx 视为健康。仅用于组件的 Health 实现（本机环回探测，不跟随到非本机的重定向以外的任何东西）。
func HTTPOK(ctx context.Context, url string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	c := &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("GET %s 返回 %d", url, resp.StatusCode)
	}
	return nil
}

// TCPReachable 检查 addr（host:port）能否建立 TCP 连接。
func TCPReachable(ctx context.Context, addr string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	d := net.Dialer{Timeout: timeout}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return c.Close()
}
