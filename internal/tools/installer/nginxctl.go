package installer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/baota"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/security/safeexec"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

// 原生 Nginx 的 conf.d 软链（Story 1.40 AC2，tools-design §11.3）：
//   - 只在「原生 Nginx（非宝塔）且主配置 include 了 /etc/nginx/conf.d/*.conf」时才可能；询问用户（或 --set nginx-link=yes）后创建
//     /etc/nginx/conf.d/lingedge-<c>.conf → /etc/lingedge/nginx/<c>.conf，随后 `nginx -t && nginx -s reload`；
//     任何一步失败都撤销软链（事务回滚时同样撤销，并再 reload 一次使运行中的 Nginx 与磁盘一致）。
//   - 宝塔环境一律只打印指引（FR-7）：Tools 不写 /www/server/panel/vhost 下的任何东西，也不 reload 宝塔的 Nginx。
//   - 永不覆盖：已存在同名文件/软链（指向别处）→ 冲突，不动它。

const actNginxLink = "unlink_nginx_conf" // 撤销：摘掉 conf.d 软链（仅 Created=true、仍指向 Tools 片段），并 reload

var reNginxLink = regexp.MustCompile(`^/etc/nginx/conf\.d/lingedge-([a-z][a-z0-9-]{0,31})\.conf$`)

// webCtlError 是 nginx -t / reload 的失败（带 nginx 自己的输出，便于定位配置问题）。
type webCtlError struct {
	Op     string // test | reload
	Output string
	Err    error
}

func (e *webCtlError) Error() string {
	if e.Output != "" {
		return "nginx " + e.Op + " 失败：" + e.Output
	}
	return "nginx " + e.Op + " 失败：" + e.Err.Error()
}
func (e *webCtlError) Unwrap() error { return e.Err }

func webOutput(err error) string {
	var ee *safeexec.ExitError
	s := ""
	if errors.As(err, &ee) {
		s = ee.Stderr
	}
	s = strings.TrimSpace(s)
	if len(s) > 1500 {
		s = s[len(s)-1500:]
	}
	return s
}

func validWebBin(bin string) bool { return bin == "nginx" || bin == "openresty" }

func webTest(ctx context.Context, run service.Runner, bin string) error {
	if !validWebBin(bin) {
		return fmt.Errorf("不支持的 Web 服务器 %q", bin)
	}
	if _, err := run.Run(ctx, safeexec.Cmd{Path: bin, Args: []string{"-t"}}); err != nil {
		return &webCtlError{Op: "-t", Output: webOutput(err), Err: err}
	}
	return nil
}

func webReload(ctx context.Context, run service.Runner, bin string) error {
	if !validWebBin(bin) {
		return fmt.Errorf("不支持的 Web 服务器 %q", bin)
	}
	if _, err := run.Run(ctx, safeexec.Cmd{Path: bin, Args: []string{"-s", "reload"}}); err != nil {
		return &webCtlError{Op: "-s reload", Output: webOutput(err), Err: err}
	}
	return nil
}

// webRunnerOrNil 供撤销路径使用：构造失败时返回 nil（撤销只摘软链，不 reload）。
func (in *Installer) webRunnerOrNil() service.Runner {
	r, err := in.webRunner()
	if err != nil {
		return nil
	}
	return r
}

func (in *Installer) webRunner() (service.Runner, error) {
	if in.d.WebRunner != nil {
		return in.d.WebRunner, nil
	}
	return service.SystemWebServerRunner()
}

// nginxCapability 是宿主机对 conf.d 软链的支持情况。
type nginxCapability struct {
	// Baota：检测到宝塔（面板或其 Nginx）→ 一律只给指引。
	Baota bool
	// Bin：原生 Nginx/OpenResty 可执行名（nginx|openresty）；空 = 没有可用的原生 Web 服务器。
	Bin string
	// ConfD：原生 Nginx 的主配置确实 include 了 conf.d/*.conf。
	ConfD bool
	// Why 是不可用的原因（给用户看）。
	Why string
}

func (c nginxCapability) OK() bool { return !c.Baota && c.Bin != "" && c.ConfD }

// detectNginx 判定能否走 conf.d 软链：非宝塔 + 系统 Nginx 的主配置是 /etc/nginx/nginx.conf 且 include 了 conf.d。
func (in *Installer) detectNginx(rep baota.Report) nginxCapability {
	if rep.Detected {
		return nginxCapability{Baota: true, Why: "检测到宝塔面板：Tools 不修改宝塔站点与 Nginx（FR-7）"}
	}
	for _, w := range rep.WebServers {
		if w.Variant == "baota" {
			return nginxCapability{Baota: true, Why: "检测到宝塔的 Web 服务器：Tools 不修改宝塔站点与 Nginx（FR-7）"}
		}
	}
	for _, w := range rep.WebServers {
		if w.Variant != "system" || (w.Kind != "nginx" && w.Kind != "openresty") {
			continue
		}
		if path.Dir(w.Conf) != "/etc/nginx" {
			return nginxCapability{Bin: w.Kind, Why: "Nginx 主配置不在 /etc/nginx（" + w.Conf + "），不假定 conf.d 约定"}
		}
		b, err := in.d.FS.ReadFile(w.Conf, 1<<20)
		if err != nil {
			return nginxCapability{Bin: w.Kind, Why: "无法读取 " + w.Conf + "：" + err.Error()}
		}
		if !includesConfD(string(b)) {
			return nginxCapability{Bin: w.Kind, Why: w.Conf + " 没有 include conf.d/*.conf"}
		}
		return nginxCapability{Bin: w.Kind, ConfD: true}
	}
	return nginxCapability{Why: "没有探测到原生 Nginx/OpenResty"}
}

// includesConfD 判断主配置是否 include 了 conf.d 下的 *.conf（含 conf.d/*、相对 /etc/nginx 的写法）。
func includesConfD(conf string) bool {
	root, err := parseNginx(conf)
	if err != nil {
		return false
	}
	var walk func(ds []nginxDirective) bool
	walk = func(ds []nginxDirective) bool {
		for _, d := range ds {
			if d.name == "include" && len(d.args) == 1 {
				a := path.Clean(d.args[0])
				switch a {
				case "conf.d/*.conf", "/etc/nginx/conf.d/*.conf", "conf.d/*", "/etc/nginx/conf.d/*":
					return true
				}
			}
			if d.hasB && walk(d.block) {
				return true
			}
		}
		return false
	}
	return walk(root)
}

// linkState 描述 conf.d 里同名项的现状。
type linkState int

const (
	linkAbsent  linkState = iota
	linkOurs              // 已是指向 Tools 片段的软链
	linkForeign           // 存在但不是（普通文件/目录/指向别处的软链）
)

func (in *Installer) linkStateOf(c string) (linkState, error) {
	real := in.d.hostPath(StaticLinkPath(c))
	st, err := os.Lstat(real)
	if errors.Is(err, fs.ErrNotExist) {
		return linkAbsent, nil
	}
	if err != nil {
		return linkForeign, err
	}
	if st.Mode()&fs.ModeSymlink != 0 {
		if t, rerr := os.Readlink(real); rerr == nil && t == StaticConfPath(c) {
			return linkOurs, nil
		}
	}
	return linkForeign, nil
}

// ---- 激活（步骤 19）----

// nginxLinkUndo 是步骤 19 为 conf.d 软链记录的撤销动作（先记 undo 再动手）。
func (x *execution) nginxLinkUndo(c string) (registry.UndoAction, error) {
	st, err := x.in.linkStateOf(c)
	if err != nil {
		return registry.UndoAction{}, err
	}
	return registry.UndoAction{Action: actNginxLink, Path: StaticLinkPath(c), Created: st == linkAbsent,
		Extra: map[string]string{targetKey: StaticConfPath(c), "bin": x.pre.nginx.Bin}}, nil
}

// activateNginx 创建软链并 nginx -t && reload。任何失败都撤销软链并以警告告知（静态文件与片段已就位，用户可按指引手工处理），不使安装失败。
func (x *execution) activateNginx(ctx context.Context, c string) {
	warn := func(code, msg string) { x.warn(Warning{Code: code, Component: c, Step: StepStart, Message: msg}) }
	run, err := x.in.webRunner()
	if err != nil {
		warn("nginx.unavailable", "无法构造 Nginx 执行器："+err.Error()+"；未创建软链，请按指引手工 include")
		return
	}
	bin := x.pre.nginx.Bin
	st, err := x.in.linkStateOf(c)
	if err != nil || st == linkForeign {
		warn("nginx.link_conflict", StaticLinkPath(c)+" 已存在且不是指向 "+StaticConfPath(c)+" 的软链：不会覆盖，也不创建软链；请人工处理后手工 include")
		return
	}
	link := x.hp(StaticLinkPath(c))
	if _, derr := os.Stat(filepath.Dir(link)); derr != nil {
		warn("nginx.link_skipped", nginxConfD+" 不存在：未创建软链")
		return
	}
	created := false
	if st == linkAbsent {
		if err := os.Symlink(StaticConfPath(c), link); err != nil {
			warn("nginx.link_failed", "创建软链失败："+err.Error())
			return
		}
		created = true
	}
	revert := func() {
		if created {
			_ = os.Remove(link)
		}
	}
	if err := webTest(ctx, run, bin); err != nil {
		revert()
		warn("nginx.test_failed", err.Error()+"；已撤销软链，Nginx 配置未被改变。请修正后手工 include 并 "+bin+" -t && "+bin+" -s reload")
		return
	}
	if err := webReload(ctx, run, bin); err != nil {
		revert()
		warn("nginx.reload_failed", err.Error()+"；已撤销软链。Nginx 是否在运行？可修复后手工 include 并 "+bin+" -s reload")
		return
	}
	x.nginxLinked[c] = true
}

// ---- 撤销 ----

func (u *Undoer) unlinkNginx(ctx context.Context, a registry.UndoAction) (service.UndoResult, error) {
	m := reNginxLink.FindStringSubmatch(a.Path)
	if m == nil {
		return service.UndoResult{}, fmt.Errorf("撤销路径 %s 不是 conf.d 下的 Tools 软链，拒绝", a.Path)
	}
	if a.Extra[targetKey] != StaticConfPath(m[1]) {
		return service.UndoResult{}, fmt.Errorf("撤销动作的目标 %q 与 %s 不匹配，拒绝", a.Extra[targetKey], a.Path)
	}
	if !a.Created {
		return service.UndoResult{Outcome: service.OutcomeNoop}, nil
	}
	real := u.host(a.Path)
	st, err := os.Lstat(real)
	if errors.Is(err, fs.ErrNotExist) {
		return service.UndoResult{Outcome: service.OutcomeAbsent}, nil
	}
	if err != nil {
		return service.UndoResult{}, err
	}
	if st.Mode()&fs.ModeSymlink == 0 {
		return service.UndoResult{Outcome: service.OutcomeSkipped, Detail: a.Path + " 已不是符号链接，未动"}, nil
	}
	if cur, rerr := os.Readlink(real); rerr != nil || cur != a.Extra[targetKey] {
		return service.UndoResult{Outcome: service.OutcomeSkipped, Detail: a.Path + " 已指向别处，未动"}, nil
	}
	if err := os.Remove(real); err != nil {
		return service.UndoResult{}, err
	}
	res := service.UndoResult{Outcome: service.OutcomeDone}
	// 软链可能已被 reload 载入：再 reload 一次使运行中的 Nginx 与磁盘一致（失败只告警，软链已摘除，磁盘状态正确）。
	bin := a.Extra["bin"]
	if u.Web != nil && validWebBin(bin) {
		if err := webReload(ctx, u.Web, bin); err != nil {
			res.Detail = "已摘除 " + a.Path + "，但 " + bin + " reload 失败：" + err.Error() + "；请手工 " + bin + " -s reload"
			if u.Notice != nil {
				u.Notice("nginx.undo_reload_failed", res.Detail)
			}
		}
	}
	return res, nil
}
