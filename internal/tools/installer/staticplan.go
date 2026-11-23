package installer

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/resolver"
)

// 静态前端的计划阶段规则（Story 1.40）：独立主机名（AC5）、同域名站点冲突（AC3）、Nginx 软链决策（AC2）。

// readInstalledSiteHosts 读取已安装的其他静态前端使用的主机名（来自 /etc/lingedge/nginx/<c>.conf 首行的标记；component → host）。
// 目录不存在 = 还没装过任何前端。读不了的目录/文件不能当作「没有冲突」放过——那会让独立主机名的强制被绕过——所以失败关闭。
func (in *Installer) readInstalledSiteHosts(self string) (map[string]string, error) {
	dir := in.d.hostPath(nginxEtcDir)
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, wrap(KindPrecondition, err, "无法读取 %s，无法确认主机名是否与已装前端重复", nginxEtcDir)
	}
	out := map[string]string{}
	for _, e := range ents {
		name := e.Name()
		comp, isConf := strings.CutSuffix(name, ".conf")
		if !isConf || !IsStaticComponent(comp) || comp == self || !e.Type().IsRegular() {
			continue
		}
		b, err := readHead(path.Join(dir, name), 4096)
		if err != nil {
			return nil, wrap(KindPrecondition, err, "无法读取 %s/%s，无法确认主机名是否与已装前端重复", nginxEtcDir, name)
		}
		if c, h, ok := ParseSiteMarker(b); ok && c == comp {
			out[comp] = h
		}
	}
	return out, nil
}

func readHead(p string, n int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, n)
	m, err := f.Read(buf)
	if err != nil && m == 0 {
		return nil, err
	}
	return buf[:m], nil
}

// checkStaticHosts：Plan 的最早阶段（只读、无需网络）强制独立主机名（IMP-17）：本次 --set host= 与任何**已安装**的其他静态前端相同 → 退出码 2，
// 不提供绕过开关。比较不区分大小写（主机名已规范化为小写）。
func (in *Installer) checkStaticHosts(req Request) error {
	if _, ok := in.d.Specs[req.Component].(staticFrontend); !ok {
		return nil
	}
	set, err := ParseStaticSettings(req.Component, req.Settings)
	if err != nil {
		return wrap(KindSettings, err, "--set 不合法")
	}
	others, err := in.readInstalledSiteHosts(req.Component)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(others))
	for c := range others {
		names = append(names, c)
	}
	sort.Strings(names)
	for _, c := range names {
		if others[c] == set.Host {
			return &Error{Kind: KindSettings, Msg: "主机名 " + set.Host + " 已被已安装的 " + c + " 使用：Admin、Console、官网（以及 docs、status）必须使用互不相同的主机名",
				Hint: "为 " + req.Component + " 另选一个主机名（如 admin.example.com / console.example.com / www.example.com）；本规则没有绕过开关——" +
					"同一主机名下 Cookie 为 host-only 的隔离、安全头与 /api/ 反代范围都无法分开（IMP-17，tools-design §11.3）",
				SystemState: "unchanged", Details: map[string]any{"host": set.Host, "conflicts_with": c}}
		}
	}
	return nil
}

// planStatic 在渲染之后、冲突判定之前执行：解析 --set、检查同域名站点、决定 Nginx 软链、补充计划步骤。
func (in *Installer) planStatic(res *PreflightResult) error {
	res.static = map[string]StaticSettings{}
	for _, c := range res.Plan.Components {
		if c.Action != "install" {
			continue
		}
		if _, ok := in.d.Specs[c.Name].(staticFrontend); !ok {
			continue
		}
		set, err := ParseStaticSettings(c.Name, res.req.Settings)
		if err != nil {
			return wrap(KindSettings, err, "--set 不合法")
		}
		res.static[c.Name] = set
		if err := in.planStaticOne(res, c, set); err != nil {
			return err
		}
	}
	return nil
}

func (in *Installer) planStaticOne(res *PreflightResult, c resolver.PlanComponent, set StaticSettings) error {
	warn := func(code, msg string) {
		res.Warnings = append(res.Warnings, Warning{Code: code, Component: c.Name, Step: StepWebServer, Message: msg})
	}
	siteConflict := false
	// 同域名站点：宿主机探测到的任何其他 Nginx server 块声明了同一 server_name（宝塔/系统）。我们自己的文件不算。
	for _, s := range res.probe.Baota.Sites {
		own := strings.HasPrefix(s.File, nginxEtcDir+"/") || path.Base(s.File) == "lingedge-"+c.Name+".conf"
		if own {
			continue
		}
		for _, n := range s.ServerNames {
			if strings.EqualFold(n, set.Host) {
				siteConflict = true
				res.Conflicts = append(res.Conflicts, Conflict{Kind: "site", Component: c.Name, Path: s.File,
					Message: "已存在声明了同一域名 " + set.Host + " 的站点（" + s.Source + "）：不会覆盖或接管它",
					Hint:    "换一个主机名；或确认后只生成 " + StaticConfPath(c.Name) + "（不会创建 conf.d 软链，由你决定如何并存）"})
				break
			}
		}
	}

	// TLS 文件不存在只警告：证书可能稍后才签发，nginx -t 会在 include 时给出权威结论。
	for _, f := range []string{set.TLSCert, set.TLSKey} {
		if f == "" {
			continue
		}
		if _, err := os.Stat(in.d.hostPath(f)); err != nil {
			warn("static.tls_file_missing", f+" 当前不可读："+err.Error()+"（片段已按给定路径渲染；证书就绪前 nginx -t 会失败）")
		}
	}
	if set.TLSCert == "" {
		warn("static.no_tls", "未配置 TLS（--set tls-cert/tls-key）：站点以 HTTP 提供。Admin 的会话 Cookie 为 Secure，生产必须经 HTTPS 访问（可在 Nginx 前加 TLS 终止，或补上证书重装）")
	}

	cap := in.detectNginx(res.probe.Baota)
	res.nginx = cap
	switch {
	case set.NginxLink == "yes" && !cap.OK():
		// 显式要求而环境做不到：失败关闭，不静默降级成「只打印指引」。
		return &Error{Kind: KindSettings, Step: StepWebServer, Msg: "--set nginx-link=yes 无法满足：" + cap.Why,
			Hint: "宝塔环境一律只打印指引（请在面板新建站点并 include " + StaticConfPath(c.Name) + "）；去掉 nginx-link=yes 即可", SystemState: "unchanged"}
	case !cap.OK():
		if cap.Baota {
			warn("nginx.baota_guidance_only", cap.Why+"；安装完成后会打印「新建站点并 include 该文件 / root 指向 current/public」的指引")
		}
		res.req.NginxLink = false
		return nil
	}
	// 原生 Nginx：conf.d 软链决策。
	if siteConflict {
		res.req.NginxLink = false
		warn("nginx.link_skipped_site_conflict", "存在同域名站点：不会创建 conf.d 软链")
		return nil
	}
	st, err := in.linkStateOf(c.Name)
	if err != nil || st == linkForeign {
		res.Conflicts = append(res.Conflicts, Conflict{Kind: "file", Component: c.Name, Path: StaticLinkPath(c.Name),
			Message: "已存在同名 conf（不是指向 " + StaticConfPath(c.Name) + " 的软链）：不会覆盖",
			Hint:    "确认后保留现状且不创建软链；片段仍会生成到 " + StaticConfPath(c.Name)})
		res.req.NginxLink = false
		return nil
	}
	if res.req.SkipStart {
		res.req.NginxLink = false
		return nil
	}
	link := res.req.NginxLink // 续跑：沿用 journal 里已决定的值
	switch {
	case link:
	case set.NginxLink == "yes":
		link = true
	case set.NginxLink == "no":
	case res.req.NonInteractive || res.req.DryRun || in.d.Confirm == nil:
		warn("nginx.link_not_asked", "检测到原生 Nginx 的 conf.d 约定：非交互/--dry-run 下不会创建软链也不 reload；加 --set nginx-link=yes 可显式启用，或手工 include")
	default:
		ok, err := in.d.Confirm(Prompt{Kind: "nginx_link", Message: "是否创建 " + StaticLinkPath(c.Name) + " → " + StaticConfPath(c.Name) + " 软链，并执行 " + cap.Bin + " -t && " + cap.Bin + " -s reload？",
			Items: []string{"软链只指向 Tools 生成的片段，不修改任何既有 Nginx 配置", cap.Bin + " -t 失败则立即撤销软链，Nginx 配置不受影响", "选择否：只生成片段，安装完成后给出手工 include 指引"}})
		if err == nil && ok {
			link = true
		}
	}
	res.req.NginxLink = link
	if link {
		res.Plan.Steps = append(res.Plan.Steps, resolver.Step{Seq: len(res.Plan.Steps) + 1, Component: c.Name, Kind: "start",
			Summary: "创建 " + StaticLinkPath(c.Name) + " 软链并 " + cap.Bin + " -t && " + cap.Bin + " -s reload（失败则撤销软链）"})
		res.StepMap = stepMap(res.Plan)
	}
	return nil
}

// staticAccess 生成静态前端的访问/管理信息（步骤 22）。
func (x *execution) staticAccess(c string) []Access {
	set, ok := x.pre.static[c]
	if !ok {
		return nil
	}
	var out []Access
	add := func(kind, label, value string) {
		out = append(out, Access{Component: c, Kind: kind, Label: label, Value: value})
	}
	add("site_url", "站点地址（域名需解析到本机）", set.SiteURL())
	switch {
	case x.nginxLinked[c]:
		add("nginx_include", "Nginx 站点片段", StaticConfPath(c)+"（已创建 "+StaticLinkPath(c)+" 软链，"+x.pre.nginx.Bin+" -t 与 reload 通过）")
	case x.pre.nginx.Baota:
		add("baota_guide", "宝塔：请手工接入（Tools 不修改宝塔站点与面板配置）",
			"在面板里新建站点（域名 "+set.Host+"），把站点 root 指向 "+StaticRoot(c)+"，并 include "+StaticConfPath(c)+"（或直接采用该文件里的 server 块）；改完在面板里重载 Nginx")
	default:
		bin := x.pre.nginx.Bin
		if bin == "" {
			bin = "nginx"
		}
		add("nginx_include", "Nginx 站点片段（需手工接入）",
			StaticConfPath(c)+"：在 http{} 内 include 它，或 ln -s "+StaticConfPath(c)+" "+StaticLinkPath(c)+"；然后 "+bin+" -t && "+bin+" -s reload")
	}
	if prof := staticProfiles[c]; prof.API != apiNone {
		scope := "/api/ 反代到 " + set.APIUpstream
		if prof.API == apiPublic {
			scope = "只放行 /api/v1/public/ → " + set.APIUpstream + "（其余 /api/ 与 /api/v1/public/setup 返回 404）"
		}
		add("note", "API 上游", scope+"；Server 可以在远端，改 --set api-upstream= 后重装即可（Server 对静态前端是配置层依赖，不要求同机）")
	}
	if set.TLSCert == "" {
		add("note", "TLS", "当前是 HTTP 站点：会话 Cookie 为 Secure 与 host-only，生产须经 HTTPS 访问（补 --set tls-cert=/tls-key= 或在前置的反代终止 TLS）")
	}
	return out
}
