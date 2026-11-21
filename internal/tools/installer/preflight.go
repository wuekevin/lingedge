package installer

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/downloader"
	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/probe"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/resolver"
	"github.com/wuekevin/lingedge/internal/tools/service"
	"github.com/wuekevin/lingedge/pkg/setuptoken"
)

// Warning 是不阻断的提示（Step 为其所属的预检步骤，0 = 全局）。
type Warning struct {
	Code      string `json:"code"`
	Component string `json:"component,omitempty"`
	Step      int    `json:"step,omitempty"`
	Message   string `json:"message"`
}

// StepVerdict 是预检 1–9 各步的结论。
type StepVerdict struct {
	N       int    `json:"n"`
	Name    string `json:"name"`
	Status  string `json:"status"` // ok | warn | fail
	Message string `json:"message,omitempty"`
}

// PlanStepMapping 是 resolver.Step 到 §12.1 的 22 步的映射（PlanStepMap）。
type PlanStepMapping struct {
	Seq       int    `json:"seq"`
	Component string `json:"component"`
	Kind      string `json:"kind"`
	Steps     []int  `json:"steps"`
}

// Outcome 取值。
const (
	OutcomePlanned   = "planned"           // dry-run：计划成功
	OutcomeInstalled = "installed"         // 安装完成
	OutcomeNoop      = "already_installed" // 计划里所有组件都已是目标版本
)

// PreflightResult 是预检（步骤 1–9）与计划的结果：--dry-run 的全部输出。
type PreflightResult struct {
	Plan           *resolver.Plan    `json:"plan"`
	Steps          []StepVerdict     `json:"preflight"`
	Conflicts      []Conflict        `json:"conflicts"`
	Warnings       []Warning         `json:"warnings"`
	StepMap        []PlanStepMapping `json:"step_map"`
	CredentialMode string            `json:"credential_mode,omitempty"`
	ListenersKnown bool              `json:"listeners_known"`
	Node           *NodeRuntime      `json:"node,omitempty"`
	// Noop：计划里没有任何需要安装的组件。
	Noop bool `json:"noop"`

	req        Request
	files      []plannedFile
	units      map[string][]service.Unit // 组件 → 将安装的单元
	probe      probe.Result
	mf         *manifest.Result
	cred       service.CredentialMode
	token      string // 本次生成的 setup token（仅内存；步骤 22 显示一次）
	acceptedBy string
}

type plannedFile struct {
	Component string
	Path      string // 虚拟绝对路径
	Kind      string // config | secret_key | setup_hash
	Content   []byte
	Mode      fs.FileMode
	Owner     string
	Group     string
	// Random：内容是随机生成的（master.key/setup-token.hash），已存在时沿用而不比较，不算冲突。
	Random bool
}

// Plan 执行预检（步骤 1–9，只读）并产出计划。它不取锁、不开 journal、不创建任何文件或目录——`--dry-run` 就是它。
// 失败返回：*resolver.Error（无解/资源/系统/端口/未发布/内置/吊销/被动升级未确认）、manifest/downloader 的校验或传输错误、*Error（见 Kind）。
// 需要确认的冲突在交互模式下经 Deps.Confirm 询问；非交互且没有 Request.Yes → KindConfirmRequired（其 Details["plan"] 含完整计划，dry-run 同样展示）。
func (in *Installer) Plan(ctx context.Context, req Request) (*PreflightResult, error) {
	if err := in.checkRequest(&req); err != nil {
		return nil, err
	}
	in.ev.emit(Event{Type: EventPhase, Phase: PhasePreflight, Message: "预检（只读）"})

	// scheduler|worker 内置于 Server（退出码 8）：无需取清单就能判定。
	if req.Component == "scheduler" || req.Component == "worker" {
		return nil, resolver.Resolve(resolver.Input{Want: []resolver.Want{{Component: req.Component}}, Manifest: &manifest.Manifest{},
			System: resolver.System{OS: "linux", Arch: "amd64"}}).Err()
	}

	// --set：非端口键先过组件自己的 schema（退出码 2），不进入任何副作用。
	if err := in.checkSettings(req); err != nil {
		return nil, err
	}

	pr, err := in.d.Collect(ctx)
	if err != nil {
		return nil, wrap(KindStepFailed, err, "系统探测失败")
	}
	mfRes, err := in.loadManifest(ctx, req)
	if err != nil {
		return nil, err
	}

	state, _, err := in.d.Reg.LoadState()
	if err != nil {
		return nil, err
	}
	in_ := resolver.Input{
		Want: wants(req), Installed: state, InstalledVariants: installedVariants(state), Manifest: mfRes.Manifest,
		System: pr.System, PortOverrides: req.PortOverrides,
	}
	plan := fixPlan(resolver.Resolve(in_))

	res := &PreflightResult{req: req, probe: pr, mf: mfRes, Plan: plan, ListenersKnown: pr.ListenersKnown, Conflicts: []Conflict{}, Warnings: []Warning{}}
	in.collectWarnings(res, pr, plan)
	for _, w := range mfRes.Warnings { // 清单验证的警告（如 --allow-expired-manifest 放行了已过期的清单）
		res.Warnings = append(res.Warnings, Warning{Code: "manifest.warning", Step: StepVersion, Message: w})
	}

	// 被动升级：只有「确认」这一类阻断时，经确认后重新求解（AcceptPassiveUpgrade）。
	if onlyConfirmPassive(plan) {
		ok, cerr := in.confirm(req, Prompt{Kind: "passive_upgrade", Message: passiveMessage(plan), Items: passiveItems(plan)})
		if cerr != nil {
			return res.fail(in, cerr)
		}
		if ok {
			in_.AcceptPassiveUpgrade = true
			plan = fixPlan(resolver.Resolve(in_))
			res.Plan = plan
			res.Warnings = res.Warnings[:0]
			in.collectWarnings(res, pr, plan)
			res.acceptedBy = "passive_upgrade"
		}
	}
	res.StepMap = stepMap(plan)
	res.Steps = verdicts(plan, res.Warnings)
	if !plan.Executable() {
		in.emitVerdicts(res.Steps)
		return res, plan.Err()
	}

	// 已装组件的升级不属于 install（Story 1.43 的 update：备份、迁移决策、健康回滚）。
	var up []string
	for _, c := range plan.Components {
		if c.Action == "upgrade" {
			up = append(up, c.Name+" "+c.From+" → "+c.Version)
		}
	}
	if len(up) > 0 {
		in.emitVerdicts(res.Steps)
		e := &Error{Kind: KindUpgradeRequired, Step: StepDependencies, Msg: "本次安装需要升级已装组件：" + strings.Join(up, "；"),
			Hint:    "先运行 lingedge update <组件>（升级含备份与迁移决策，Story 1.43），再重试安装；本命令不会在安装流程里升级已装组件",
			Details: map[string]any{"upgrades": up, "plan": plan}, SystemState: "unchanged"}
		return res, e
	}

	// 组件规格：每个要安装的组件都必须有 ComponentSpec（生产构建目前为空）。
	if err := in.checkSpecs(plan); err != nil {
		return res, err
	}
	res.Noop = true
	for _, c := range plan.Components {
		if c.Action == "install" {
			res.Noop = false
		}
	}
	if res.Noop {
		in.emitVerdicts(res.Steps)
		return res, nil
	}

	if req.Component == "server" {
		// server 的监听随计划端口（--set port.server… 换端口后，server.yaml、健康检查与访问地址必须一致）；有效的向导监听写回请求（journal 续跑据此）。
		ls, lerr := ServerListensFromPlan(req.SetupListen, plan.Ports)
		if lerr != nil {
			return res, wrap(KindSettings, lerr, "server 监听地址不合法")
		}
		req.SetupListen, res.req.SetupListen = ls.HTTP, ls.HTTP
	}
	if host, _, err := net.SplitHostPort(req.SetupListen); err == nil && req.SetupListen != "" && !isLoopbackHost(host) {
		res.Warnings = append(res.Warnings, Warning{Code: "setup.listen_exposed", Component: "server", Step: StepPorts,
			Message: "安装向导将监听 " + req.SetupListen + "（非本机回环）：向导在 complete 之前对网络可达，仅凭 setup token 保护（明文 HTTP 传输 token 与向导填写的口令）；" +
				"强烈建议保持默认 127.0.0.1:8080，用 SSH 隧道访问（ssh -L 8080:127.0.0.1:8080 <用户>@<主机>），或在前面加 TLS 反代"})
	}
	// systemd：版本决定主密钥交付方式（ADR-027）；不是 systemd/过旧 → 退出码 3。
	ver, err := in.sys.Version(ctx)
	if err != nil {
		return res, wrap(KindStepFailed, err, "无法确认 systemd 版本")
	}
	cred, err := service.SelectCredentialMode(ver)
	if err != nil {
		return res, wrap(KindStepFailed, err, "systemd 版本不满足要求")
	}
	res.cred, res.CredentialMode = cred, string(cred)
	if cred == service.CredentialKeyFile {
		res.Warnings = append(res.Warnings, Warning{Code: "systemd.credentials_degraded", Step: StepOSDetect,
			Message: "systemd " + strconv.Itoa(ver.Major) + " 不支持 LoadCredential=：Server 主密钥降级为 root:<运行组> 0440 的密钥文件方式（ADR-027）"})
	}

	// 渲染将要创建的全部文件（内存），并与磁盘现状比对。
	if err := in.render(ctx, res); err != nil {
		return res, err
	}
	in.checkConflicts(res)
	in.emitVerdicts(res.Steps)
	for _, w := range res.Warnings {
		in.ev.emit(Event{Type: EventWarning, Step: w.Step, Code: w.Code, Component: w.Component, Message: w.Message})
	}

	// 端口无法检测（ss 与 /proc 均不可用）：不当作无冲突；与其他冲突一并确认。
	if !pr.ListenersKnown && len(plan.Ports) > 0 {
		res.Conflicts = append(res.Conflicts, Conflict{Kind: "port_unverified", Message: "无法检测端口占用（ss 与 /proc 均不可用），不能确认计划占用的端口空闲",
			Hint: "用 root 运行，或确认端口空闲后加 --yes 继续"})
	}
	if len(res.Conflicts) > 0 {
		return res, in.resolveConflicts(req, res)
	}
	return res, nil
}

func (r *PreflightResult) fail(in *Installer, err error) (*PreflightResult, error) {
	in.emitVerdicts(r.Steps)
	return r, err
}

func (in *Installer) checkRequest(req *Request) error {
	if !registry.ValidComponent(req.Component) {
		return newErr(KindSettings, "组件名不合法")
	}
	if req.Channel == "" {
		req.Channel = "stable"
	}
	if req.Channel != "stable" && req.Channel != "beta" {
		return newErr(KindSettings, "通道必须是 stable 或 beta")
	}
	if req.AllowExpired && req.Offline == "" {
		return newErr(KindSettings, "--allow-expired-manifest 只能与 --offline 一起使用")
	}
	if req.Component == "server" && req.SetupListen == "" {
		req.SetupListen = "127.0.0.1:8080"
	}
	return nil
}

// checkSettings 校验非端口的 --set：server 的内建键由 installer 校验，其余交给组件规格；没有规格的组件不接受任何设置。
func (in *Installer) checkSettings(req Request) error {
	if len(req.Settings) == 0 {
		return nil
	}
	rest := map[string]string{}
	for k, v := range req.Settings {
		if req.Component == "server" && serverSettingKeys[k] {
			continue
		}
		rest[k] = v
	}
	if req.Component == "server" {
		if _, err := ServerYAMLFromSettings(req.Settings, req.SetupListen, service.CredentialSystemd); err != nil {
			return wrap(KindSettings, err, "--set 的 server 配置项不合法")
		}
	}
	if len(rest) == 0 {
		return nil
	}
	spec, ok := in.d.Specs[req.Component]
	if !ok {
		// 没有规格就没有 schema 可校验：不在这里编造错误——规格缺失会在求解之后以 install.component_spec_missing 明确失败（未发布/内置的组件仍先得到 7/8）。
		return nil
	}
	if err := spec.ValidateSettings(rest); err != nil {
		return wrap(KindSettings, err, "--set 不合法")
	}
	return nil
}

func wants(req Request) []resolver.Want {
	if len(req.Pinned) > 0 {
		return append([]resolver.Want(nil), req.Pinned...)
	}
	return []resolver.Want{{Component: req.Component, Version: req.Version, Variant: req.Variant}}
}

// installedVariants 推断已装组件的变体（state.json 不记变体）：website 由单元推断（ssr 才有 lingedge-website.service）。
func installedVariants(st registry.State) map[string]string {
	out := map[string]string{}
	if c, ok := st.Components["website"]; ok {
		if c.Unit != "" {
			out["website"] = "ssr"
		} else {
			out["website"] = "static"
		}
	}
	return out
}

func fixPlan(p *resolver.Plan) *resolver.Plan {
	if p.Components == nil {
		p.Components = []resolver.PlanComponent{}
	}
	if p.Order == nil {
		p.Order = []string{}
	}
	if p.Steps == nil {
		p.Steps = []resolver.Step{}
	}
	if p.Ports == nil {
		p.Ports = []resolver.PlannedPort{}
	}
	if p.Units == nil {
		p.Units = []resolver.Unit{}
	}
	if p.Files == nil {
		p.Files = []resolver.File{}
	}
	if p.Warnings == nil {
		p.Warnings = []resolver.Warning{}
	}
	if p.Blockers == nil {
		p.Blockers = []resolver.Blocker{}
	}
	if p.Resources.Disks == nil {
		p.Resources.Disks = []resolver.DiskUsage{}
	}
	return p
}

func stepMap(p *resolver.Plan) []PlanStepMapping {
	out := make([]PlanStepMapping, 0, len(p.Steps))
	for _, s := range p.Steps {
		out = append(out, PlanStepMapping{Seq: s.Seq, Component: s.Component, Kind: s.Kind, Steps: append([]int(nil), PlanStepMap[s.Kind]...)})
	}
	return out
}

func onlyConfirmPassive(p *resolver.Plan) bool {
	if len(p.Blockers) == 0 {
		return false
	}
	for _, b := range p.Blockers {
		if b.Kind != resolver.KindConfirm {
			return false
		}
	}
	return true
}

func passiveItems(p *resolver.Plan) []string {
	var out []string
	for _, c := range p.Components {
		if c.PassiveUpgrade {
			out = append(out, c.Name+" "+c.From+" → "+c.Version)
		}
	}
	return out
}

func passiveMessage(p *resolver.Plan) string {
	if len(p.Blockers) > 0 {
		return p.Blockers[0].Message
	}
	return "已装组件需要被动升级"
}

// confirm 询问用户：已预先同意（Yes）→ 是；非交互或无提问通道 → 否（调用方据此以退出码 9 失败）。
func (in *Installer) confirm(req Request, p Prompt) (bool, error) {
	if req.Yes {
		return true, nil
	}
	if req.NonInteractive || req.DryRun || in.d.Confirm == nil {
		return false, nil
	}
	return in.d.Confirm(p)
}

// stepOfBlocker 把 resolver 的阻断归到 §12.1 的预检步骤（1–9）。
func stepOfBlocker(b resolver.Blocker) int {
	switch {
	case b.Code == "system.os_unsupported":
		return StepOSDetect
	case b.Code == "system.arch_unsupported":
		return StepArch
	case b.Kind == resolver.KindResource:
		return StepMemDisk
	case b.Kind == resolver.KindPort:
		return StepPorts
	case b.Kind == resolver.KindSystem:
		return StepDatabases
	case b.Kind == resolver.KindNotReleased, b.Kind == resolver.KindBuiltin, b.Kind == resolver.KindRevoked,
		strings.HasPrefix(b.Code, "version."), strings.HasPrefix(b.Code, "variant."):
		return StepVersion
	}
	return StepDependencies
}

// stepOfWarning 把警告归到预检步骤；0 = 不属于某一步。
func stepOfWarning(code string) int {
	switch {
	case strings.HasPrefix(code, "memory."), strings.HasPrefix(code, "disks."), code == "platform.degraded":
		return StepMemDisk
	case strings.HasPrefix(code, "listeners."), code == "listener.addr_invalid":
		return StepPorts
	case strings.HasPrefix(code, "baota."):
		return StepBaota
	case strings.HasPrefix(code, "services."):
		return StepWebServer
	case strings.HasPrefix(code, "dep."), strings.HasPrefix(code, "external."):
		return StepDatabases
	case strings.HasPrefix(code, "dependency."), strings.HasPrefix(code, "peer."), code == "already.installed", code == "passive.upgrade_accepted":
		return StepDependencies
	case strings.HasPrefix(code, "upgrade."), strings.HasPrefix(code, "installed."):
		return StepVersion
	}
	return 0
}

func (in *Installer) collectWarnings(res *PreflightResult, pr probe.Result, plan *resolver.Plan) {
	for _, w := range pr.Warnings {
		res.Warnings = append(res.Warnings, Warning{Code: w.Code, Step: stepOfWarning(w.Code), Message: w.Message})
	}
	for _, w := range plan.Warnings {
		res.Warnings = append(res.Warnings, Warning{Code: w.Code, Component: w.Component, Step: stepOfWarning(w.Code), Message: w.Message})
	}
	if d := pr.System.Deps["nodejs"]; d.Detected {
		n := NodeRuntime{Detected: true, Version: d.Version, Bin: "node", InPath: true}
		if filepath.IsAbs(d.Source) { // dep.nodejs.not_in_path：非固定目录安装，启动 website 必须用这个绝对路径（Story 1.31b）
			n.Bin, n.InPath = d.Source, false
		}
		res.Node = &n
	}
}

func verdicts(plan *resolver.Plan, warns []Warning) []StepVerdict {
	out := make([]StepVerdict, 0, 9)
	for n := StepOSDetect; n <= StepVersion; n++ {
		v := StepVerdict{N: n, Name: StepName(n), Status: "ok"}
		for _, b := range plan.Blockers {
			if stepOfBlocker(b) == n {
				v.Status = "fail"
				if v.Message == "" {
					v.Message = b.Message
				}
			}
		}
		if v.Status == "ok" {
			for _, w := range warns {
				if w.Step == n {
					v.Status, v.Message = "warn", w.Message
					break
				}
			}
		}
		out = append(out, v)
	}
	return out
}

func (in *Installer) emitVerdicts(vs []StepVerdict) {
	for _, v := range vs {
		t := EventStepDone
		if v.Status == "fail" {
			t = EventStepFailed
		}
		in.ev.emit(Event{Type: t, Step: v.N, Name: v.Name, Phase: PhasePreflight, Message: v.Message, Code: v.Status})
	}
}

func (in *Installer) checkSpecs(plan *resolver.Plan) error {
	var missing []string
	for _, c := range plan.Components {
		if c.Action != "install" {
			continue
		}
		if _, ok := in.d.Specs[c.Name]; !ok {
			missing = append(missing, c.Name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return &Error{Kind: KindSpecMissing, Step: StepDependencies, Msg: "组件 " + strings.Join(missing, "、") + " 的安装规格（ComponentSpec）尚未交付，无法安装",
		Hint:    "该组件的安装实现由后续 Story 交付（1.40 静态前端、Epic 3 node 等）；本命令不会用占位实现假装安装",
		Details: map[string]any{"components": missing, "registered": in.d.Specs.Names()}, SystemState: "unchanged"}
}

// loadManifest 取得并验证清单（只读：不推进 sequence，提交由第 21 步前的 Acquired.Commit 完成）。
func (in *Installer) loadManifest(ctx context.Context, req Request) (*manifest.Result, error) {
	v, err := in.verifier(false)
	if err != nil {
		return nil, err
	}
	var mf, sig []byte
	if req.Offline != "" {
		b, err := downloader.OpenBundle(req.Offline)
		if err != nil {
			return nil, err
		}
		mf, sig, err = b.Manifest()
		_ = b.Close()
		if err != nil {
			return nil, err
		}
	} else {
		url, err := in.d.Config.ManifestURL(req.Channel)
		if err != nil {
			return nil, err
		}
		cl, err := in.d.NewClient(in.d.Config.DownloaderConfig("", false))
		if err != nil {
			return nil, err
		}
		defer cl.Close()
		if mf, err = cl.FetchBytes(ctx, url, 8<<20); err != nil {
			return nil, err
		}
		if sig, err = cl.FetchBytes(ctx, url+".sig", 4<<10); err != nil {
			return nil, err
		}
	}
	return v.Verify(mf, sig, manifest.VerifyOptions{Channel: req.Channel, Offline: req.Offline != "", AllowExpired: req.AllowExpired})
}

// verifier 构造验证器。locked=true（取包阶段，会 Commit 推进 sequence）把 sequence 存储包进全局锁（跨进程读-改-写不丢更新，1.29 的约定）；
// locked=false（预检/--dry-run，只 Verify 不写）直接读文件：取全局锁会创建锁文件与状态目录，而 dry-run 必须零副作用。
func (in *Installer) verifier(locked bool) (*manifest.Verifier, error) {
	base, err := in.d.Verifier()
	if err != nil {
		return nil, err
	}
	v := *base
	if locked {
		v.Seen = registry.LockedSequence{R: in.d.Reg, Inner: base.Seen}
	}
	return &v, nil
}

// ---- 渲染与冲突 ----

func parseMode(s string) fs.FileMode {
	n, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0
	}
	return fs.FileMode(n)
}

func splitOwner(s string) (string, string) {
	u, g, _ := strings.Cut(s, ":")
	if g == "" {
		g = u
	}
	return u, g
}

// render 把计划里每个组件要创建的文件渲染成内存里的内容（单元、server 内建三件套、组件规格的 InitConfig）。
func (in *Installer) render(ctx context.Context, res *PreflightResult) error {
	plan := res.Plan
	res.units = map[string][]service.Unit{}
	runUser := map[string]string{}
	for _, c := range plan.Components {
		runUser[c.Name] = c.RunUser
	}
	for _, c := range plan.Components {
		if c.Action != "install" {
			continue
		}
		us, err := renderUnits(c.Name, res.cred)
		if err != nil {
			return err
		}
		res.units[c.Name] = us
		have := map[string]bool{}
		for _, u := range us {
			have[string(u.Name)] = true
		}
		for _, pu := range plan.Units {
			if pu.Component == c.Name && !have[pu.Name] {
				return &Error{Kind: KindSpecMissing, Step: StepInstallUnits, Msg: "组件 " + c.Name + " 的单元 " + pu.Name + " 的模板尚未交付，无法安装",
					Hint: "该单元由后续 Story 交付（service 包的模板）；本命令不会跳过它", SystemState: "unchanged"}
			}
		}
		// 计划里补上 resolver 画像没有列出的单元（如 node 的升级 path/service），使计划如实。
		listed := map[string]bool{}
		for _, pu := range plan.Units {
			if pu.Component == c.Name {
				listed[pu.Name] = true
			}
		}
		for _, u := range us {
			if !listed[string(u.Name)] {
				kind := "service"
				if strings.HasSuffix(string(u.Name), ".path") {
					kind = "path"
				}
				plan.Units = append(plan.Units, resolver.Unit{Component: c.Name, Name: string(u.Name), Kind: kind, Path: service.UnitDir + "/" + string(u.Name), RunUser: runUser[c.Name]})
			}
		}
	}
	sort.SliceStable(plan.Units, func(i, j int) bool { return plan.Units[i].Name < plan.Units[j].Name })

	for _, c := range plan.Components {
		if c.Action != "install" {
			continue
		}
		fs_, err := in.renderConfigs(ctx, res, c)
		if err != nil {
			return err
		}
		res.files = append(res.files, fs_...)
	}
	return nil
}

func renderUnits(component string, cred service.CredentialMode) ([]service.Unit, error) {
	var out []service.Unit
	switch component {
	case "server":
		p := service.DefaultServerParams()
		p.Credential = cred
		u, err := service.RenderServer(p)
		if err != nil {
			return nil, wrap(KindInternal, err, "渲染 server 单元失败")
		}
		out = append(out, u)
	case "node":
		for _, n := range []service.UnitName{service.UnitNode, service.UnitUpgradePath, service.UnitUpgradeService} {
			u, err := service.RenderNode(n, service.DefaultNodeParams())
			if err != nil {
				return nil, wrap(KindInternal, err, "渲染 node 单元失败")
			}
			out = append(out, u)
		}
	case "openresty":
		for _, n := range []service.UnitName{service.UnitOpenresty, service.UnitOpenrestyPath} {
			u, err := service.RenderNode(n, service.DefaultNodeParams())
			if err != nil {
				return nil, wrap(KindInternal, err, "渲染 openresty 单元失败")
			}
			out = append(out, u)
		}
	}
	return out, nil
}

func (in *Installer) configInput(res *PreflightResult, c resolver.PlanComponent) ConfigInput {
	var ports []resolver.PlannedPort
	for _, p := range res.Plan.Ports {
		if p.Component == c.Name {
			ports = append(ports, p)
		}
	}
	ci := ConfigInput{Component: c.Name, Version: c.Version, Variant: c.Variant, Settings: res.req.Settings, Ports: ports, RunUser: c.RunUser,
		ConfigDir: "/etc/lingedge/" + c.Name, ReleaseDir: "/opt/lingedge/" + c.Name + "/releases/" + c.Version,
		CurrentDir: "/opt/lingedge/" + c.Name + "/current", Credential: res.cred, SetupListen: res.req.SetupListen}
	if res.Node != nil {
		ci.Node = *res.Node
	}
	for _, f := range res.Plan.Files {
		if f.Component == c.Name && f.Kind == "dir" && f.Policy == resolver.PolicyCreateIfAbsent {
			ci.DataDir = f.Path
		}
	}
	return ci
}

func (in *Installer) renderConfigs(ctx context.Context, res *PreflightResult, c resolver.PlanComponent) ([]plannedFile, error) {
	var out []plannedFile
	ci := in.configInput(res, c)
	user, group := c.RunUser, c.RunUser
	// 规格给出的文件（server 之外，及 server 的额外文件）。
	spec := in.d.Specs[c.Name]
	fromSpec := map[string]ConfigFile{}
	if spec != nil {
		cfs, err := spec.InitConfig(ctx, ci)
		if err != nil {
			return nil, wrap(KindStepFailed, err, "组件 %s 的 InitConfig 失败", c.Name)
		}
		for _, cf := range cfs {
			if _, dup := fromSpec[cf.Path]; dup {
				return nil, newErr(KindInternal, "组件 %s 的 InitConfig 重复返回 %s", c.Name, cf.Path)
			}
			fromSpec[cf.Path] = cf
		}
	}
	for _, f := range res.Plan.Files {
		if f.Component != c.Name || f.Kind != "file" || (f.Policy != resolver.PolicyCreateIfAbsent && f.Policy != resolver.PolicyManaged) {
			continue
		}
		pf := plannedFile{Component: c.Name, Path: f.Path, Mode: parseMode(f.Mode)}
		pf.Owner, pf.Group = splitOwner(f.Owner)
		base := path.Base(f.Path)
		switch {
		case c.Name == "server" && base == "server.yaml":
			p, err := ServerYAMLFromPlan(res.req.Settings, res.req.SetupListen, res.cred, res.Plan.Ports)
			if err != nil {
				return nil, wrap(KindSettings, err, "server.yaml 参数不合法")
			}
			if pf.Content, err = RenderServerYAML(p); err != nil {
				return nil, wrap(KindSettings, err, "渲染 server.yaml 失败")
			}
			pf.Kind = "config"
			pf.Owner, pf.Group = "root", group
		case c.Name == "server" && base == "master.key":
			key, err := GenerateMasterKey()
			if err != nil {
				return nil, wrap(KindStepFailed, err, "生成主密钥失败")
			}
			sp := service.SpecMasterKey(res.cred, group)
			pf.Kind, pf.Content, pf.Random, pf.Mode, pf.Owner, pf.Group = "secret_key", key, true, sp.Mode, sp.Owner, sp.Group
		case c.Name == "server" && base == "setup-token.hash":
			tok, hash, err := setuptoken.Generate()
			if err != nil {
				return nil, wrap(KindStepFailed, err, "生成 setup token 失败")
			}
			res.token = tok
			pf.Kind, pf.Content, pf.Random = "setup_hash", []byte(hash+"\n"), true
			pf.Owner, pf.Group = user, group
		default:
			cf, ok := fromSpec[f.Path]
			if !ok {
				return nil, newErr(KindInternal, "组件 %s 的规格没有提供计划中的文件 %s（InitConfig 须覆盖 create_if_absent 的配置文件）", c.Name, f.Path)
			}
			delete(fromSpec, f.Path)
			pf.Kind, pf.Content = "config", cf.Content
			if cf.Mode != 0 {
				pf.Mode = cf.Mode
			}
			if cf.Owner != "" {
				pf.Owner = cf.Owner
			}
			if cf.Group != "" {
				pf.Group = cf.Group
			}
		}
		out = append(out, pf)
	}
	for p := range fromSpec {
		return nil, newErr(KindInternal, "组件 %s 的 InitConfig 返回了计划之外的文件 %s", c.Name, p)
	}
	return out, nil
}

// checkConflicts 把「会覆盖既有内容」的情形列为冲突：配置/单元/站点内容不同（或是符号链接）、既有目录权限/属主不符、目标 release 目录已存在。
// 纯读；确认之前不会写任何东西（FR-6、SM-C3）。
func (in *Installer) checkConflicts(res *PreflightResult) {
	for _, pf := range res.files {
		if pf.Random {
			continue
		}
		if c := in.fileConflict(pf.Component, pf.Path, pf.Content, "file"); c != nil {
			res.Conflicts = append(res.Conflicts, *c)
		}
	}
	names := make([]string, 0, len(res.units))
	for n := range res.units {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, u := range res.units[n] {
			if c := in.fileConflict(n, service.UnitDir+"/"+string(u.Name), u.Content, "unit"); c != nil {
				res.Conflicts = append(res.Conflicts, *c)
			}
		}
	}
	own := &service.Ensurer{Root: in.d.Root, Own: in.d.Own}
	for _, c := range res.Plan.Components {
		if c.Action != "install" {
			continue
		}
		plans, err := own.Plan(in.dirSpecs(res, c))
		if err != nil {
			continue // 不可检查（如路径含符号链接）：留给执行期的 Ensure 以硬错误报告
		}
		for _, p := range plans {
			for _, m := range p.Mismatch {
				res.Conflicts = append(res.Conflicts, Conflict{Kind: "dir", Component: c.Name, Path: p.Path, Message: "既有目录与规格不符：" + m,
					Hint: "确认后目录保持原样（不会被改权限）；可事后 lingedge doctor --fix-perms 修正"})
			}
		}
	}
	sort.SliceStable(res.Conflicts, func(i, j int) bool {
		a, b := res.Conflicts[i], res.Conflicts[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Kind < b.Kind
	})
}

// fileConflict：目标不存在或内容一致 → nil；内容不同/不是普通文件 → 冲突。
func (in *Installer) fileConflict(component, virtual string, want []byte, kind string) *Conflict {
	real := in.d.hostPath(virtual)
	st, err := os.Lstat(real)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return &Conflict{Kind: kind, Component: component, Path: virtual, Message: "无法检查既有文件：" + err.Error()}
	}
	if st.Mode().IsRegular() {
		if b, rerr := os.ReadFile(real); rerr == nil && string(b) == string(want) {
			return nil
		}
		return &Conflict{Kind: kind, Component: component, Path: virtual, Message: "已存在且内容不同，不会被覆盖",
			Hint: "确认后保留现有文件，新内容写入 " + virtual + ".new 供人工比对合并"}
	}
	return &Conflict{Kind: kind, Component: component, Path: virtual, Message: "已存在但不是普通文件（符号链接/目录等），不会被覆盖或跟随",
		Hint: "确认后保留现状，新内容写入 " + virtual + ".new"}
}

// resolveConflicts：已预先同意 → 继续；交互 → 询问；否则退出码 9（或端口无法验证时 4）。
func (in *Installer) resolveConflicts(req Request, res *PreflightResult) error {
	items := make([]string, 0, len(res.Conflicts))
	onlyPort := true
	for _, c := range res.Conflicts {
		items = append(items, c.Kind+" "+c.Path+"："+c.Message)
		if c.Kind != "port_unverified" {
			onlyPort = false
		}
	}
	for _, c := range res.Conflicts {
		res.Warnings = append(res.Warnings, Warning{Code: "conflict." + c.Kind, Component: c.Component, Message: c.Path + " " + c.Message})
	}
	ok, err := in.confirm(req, Prompt{Kind: "conflicts", Message: "存在需要确认的冲突：既有内容不会被覆盖", Items: items})
	if err != nil {
		return err
	}
	if ok {
		res.acceptedBy = "conflicts"
		return nil
	}
	k := KindConfirmRequired
	msg := "存在需要显式确认的冲突（既有配置/单元/站点内容不同、目录权限不符等）：不会覆盖任何既有内容"
	if onlyPort {
		k, msg = KindPortUnverified, "无法检测端口占用（ss 与 /proc 均不可用），且未获确认"
	}
	return &Error{Kind: k, Step: StepPorts, Msg: msg, Conflicts: append([]Conflict(nil), res.Conflicts...),
		Hint:    "核对冲突清单；确认后保留既有内容并写 .new：交互运行回答 y，或显式传入 --yes",
		Details: map[string]any{"conflicts": len(res.Conflicts), "plan": res.Plan}, SystemState: "unchanged"}
}

// dirSpecs 是组件的目录计划（步骤 16）。server/node 用 service 包的权威总表，其余按 resolver 画像的数据目录。
func (in *Installer) dirSpecs(res *PreflightResult, c resolver.PlanComponent) []service.DirSpec {
	user, group := c.RunUser, c.RunUser
	switch c.Name {
	case "server":
		return service.ServerDirs(user, group)
	case "node":
		return append(service.NodeDirs(user, group), service.UpgradeResultDir())
	case "openresty":
		return nil
	}
	var out []service.DirSpec
	hasCfg := false
	for _, f := range res.Plan.Files {
		if f.Component == c.Name && f.Kind == "file" {
			hasCfg = true
		}
	}
	if hasCfg {
		out = append(out, service.DirSpec{Path: "/etc/lingedge", Mode: 0o755, Owner: "root", Group: "root", Why: "§8 配置根", Loose: true},
			service.DirSpec{Path: "/etc/lingedge/" + c.Name, Mode: 0o750, Owner: "root", Group: group, Why: "§8 配置：root:<run-user> 0750"})
	}
	for _, f := range res.Plan.Files {
		if f.Component == c.Name && f.Kind == "dir" && f.Policy == resolver.PolicyCreateIfAbsent {
			o, g := splitOwner(f.Owner)
			out = append(out, service.DirSpec{Path: "/var/lib/lingedge", Mode: 0o755, Owner: "root", Group: "root", Why: "§8 数据根", Loose: true},
				service.DirSpec{Path: f.Path, Mode: parseMode(f.Mode), Owner: o, Group: g, Why: "§8 数据：<run-user> 0750"})
		}
	}
	return out
}

func isLoopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	a, err := netip.ParseAddr(h)
	return err == nil && a.IsLoopback()
}
