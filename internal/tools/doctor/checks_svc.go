package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

// ---- svc.<comp>.* ----

type initState int

const (
	initUnknown initState = iota
	initNo
	initYes
)

type serverHealth struct {
	listen      string
	live, ready int // HTTP 状态码；0 = 没有得到响应或没有探测
	liveErr     error
	readyErr    error
	initialized initState
}

// serverHealth 探测 Server 的 /healthz 与（向导完成后的）/healthz/ready，一次运行内缓存。
func (w *World) serverHealth(ctx context.Context) serverHealth {
	w.readyOnce.Do(func() {
		c, ok := w.component("server")
		if !ok {
			return
		}
		h := &w.readyRes
		listen := "127.0.0.1:8080"
		if w.ServerListens != nil {
			if l, _ := w.ServerListens(); l != "" {
				listen = l
			}
		}
		h.listen = listen
		dataDir := c.DataDir
		if dataDir == "" {
			dataDir = service.ServerDataDir
		}
		_, err := os.Lstat(w.path(dataDir + "/" + installer.InstallLockName))
		switch {
		case err == nil:
			h.initialized = initYes
		case errors.Is(err, fs.ErrNotExist):
			h.initialized = initNo
		}
		if w.HTTPGet == nil {
			h.liveErr = errors.New("HTTP 探测不可用")
			return
		}
		base := "http://" + loopbackize(listen)
		h.live, h.liveErr = w.HTTPGet(ctx, base+"/healthz")
		if h.initialized != initNo {
			h.ready, h.readyErr = w.HTTPGet(ctx, base+"/healthz/ready")
		}
	})
	return w.readyRes
}

func runSvc(ctx context.Context, w *World, f Filter) []Finding {
	var out []Finding
	cfg := w.cfg()
	if f.Component == "" {
		out = append(out, systemdFindings(ctx, w)...)
		for _, sf := range service.PendingNewUnits(w.Root) {
			out = append(out, fromServiceFinding(sf))
		}
	}
	for _, c := range w.installed(f) {
		units := service.StatusUnits(c.Name)
		if len(units) == 0 {
			continue
		}
		pre := "svc." + c.Name + "."
		if w.Systemctl == nil {
			for _, k := range []string{"active", "enabled", "restarts"} {
				x := fnd(pre+k, LevelSkip, "本机没有 systemd 控制能力，无法检查服务状态", "")
				x.Component = c.Name
				out = append(out, x)
			}
			continue
		}
		props := map[service.UnitName]service.UnitProps{}
		var perr error
		for _, u := range units {
			p, err := w.Systemctl.Show(ctx, u)
			if err != nil {
				perr = err
				break
			}
			props[u] = p
		}
		if perr != nil {
			for _, k := range []string{"active", "enabled", "restarts"} {
				x := fnd(pre+k, LevelSkip, "无法读取 systemd 状态："+perr.Error(), "")
				x.Component = c.Name
				out = append(out, x)
			}
			continue
		}
		out = append(out, svcActive(c, units, props), svcEnabled(c, props), svcRestarts(c, units[0], props[units[0]], cfg))
	}
	return out
}

func systemdFindings(ctx context.Context, w *World) []Finding {
	if w.Systemctl == nil {
		return nil
	}
	var out []Finding
	v, err := w.Systemctl.Version(ctx)
	if err != nil {
		return []Finding{fnd("service.systemd_version", LevelSkip, "无法读取 systemd 版本："+err.Error(), "")}
	}
	out = append(out, fromServiceFinding(service.CheckSystemdVersion(v)))
	if _, ok := w.component("server"); ok {
		b, rerr := os.ReadFile(w.path(service.UnitDir + "/" + string(service.UnitServer)))
		if rerr == nil {
			out = append(out, fromServiceFinding(service.CheckCredentialConsistency(b, v)))
		}
	}
	return out
}

func svcActive(c Component, units []service.UnitName, props map[service.UnitName]service.UnitProps) Finding {
	main := props[units[0]]
	x := Finding{ID: "svc." + c.Name + ".active", Component: c.Name}
	states := map[string]string{}
	for _, u := range units {
		states[string(u)] = props[u].ActiveState
	}
	x.Details = map[string]any{"units": states, "main_pid": main.MainPID}
	switch main.ActiveState {
	case "active":
		x.Level, x.Message = LevelPass, string(units[0])+" 运行中"
		if main.ActiveEnterTimestamp != "" {
			x.Message += "（自 " + main.ActiveEnterTimestamp + "）"
		}
	case "activating":
		x.Level, x.Message = LevelWarn, string(units[0])+" 正在启动（"+main.SubState+"）"
		x.Hint = "稍后重试；持续 activating 说明反复重启：lingedge logs " + c.Name
	case "failed":
		x.Level, x.Message = LevelFail, string(units[0])+" 已失败（"+orStr(main.Result, main.SubState)+"）"
		x.Hint = "lingedge logs " + c.Name + " 查看原因；修复后 lingedge service restart " + c.Name
	default:
		if main.LoadState == "not-found" {
			x.Level, x.Message = LevelFail, "systemd 找不到单元 "+string(units[0])+"（与 state.json 的记录不一致）"
			x.Hint = "重新安装该组件，或从备份恢复单元文件"
			break
		}
		x.Level, x.Message = LevelFail, string(units[0])+" 未运行（"+main.ActiveState+"）"
		x.Hint = "lingedge service start " + c.Name
		if c.State == "stopped" {
			x.Level, x.Message = LevelWarn, string(units[0])+" 已停止（state.json 记录为 stopped，可能是人为停止）"
		}
	}
	return x
}

func svcEnabled(c Component, props map[service.UnitName]service.UnitProps) Finding {
	x := Finding{ID: "svc." + c.Name + ".enabled", Component: c.Name}
	var bad []string
	st := map[string]string{}
	for _, u := range service.EnableTargets(c.Name) {
		p, ok := props[u]
		if !ok {
			continue
		}
		st[string(u)] = p.UnitFileState
		switch p.UnitFileState {
		case "enabled", "enabled-runtime", "static", "alias", "indirect", "generated":
		default:
			bad = append(bad, string(u)+"（"+orStr(p.UnitFileState, "未知")+"）")
		}
	}
	x.Details = map[string]any{"unit_file_state": st}
	if len(bad) > 0 {
		x.Level, x.Message = LevelWarn, "以下单元没有开机自启："+strings.Join(bad, "、")
		x.Hint = "systemctl enable <单元>（重装组件也会重新启用）；否则重启主机后服务不会自动恢复"
		return x
	}
	x.Level, x.Message = LevelPass, "单元已设置开机自启"
	return x
}

func svcRestarts(c Component, u service.UnitName, p service.UnitProps, cfg Config) Finding {
	x := Finding{ID: "svc." + c.Name + ".restarts", Component: c.Name, Details: map[string]any{"restarts": p.NRestarts, "sub_state": p.SubState}}
	switch {
	case p.SubState == "auto-restart":
		x.Level, x.Message = LevelFail, string(u)+" 正处于自动重启循环（auto-restart）"
		x.Hint = "lingedge logs " + c.Name + " 查看崩溃原因"
	case p.NRestarts >= cfg.RestartsWarn:
		x.Level, x.Message = LevelWarn, fmt.Sprintf("%s 自上次手动启动以来已自动重启 %d 次（阈值 %d）", u, p.NRestarts, cfg.RestartsWarn)
		x.Hint = "lingedge logs " + c.Name + " 查看崩溃原因；阈值可在 tools.yaml 的 doctor.restarts_warn 调整"
	default:
		x.Level, x.Message = LevelPass, fmt.Sprintf("%s 自动重启次数 %d（阈值 %d）", u, p.NRestarts, cfg.RestartsWarn)
	}
	return x
}

// ---- health.* ----

func runHealth(ctx context.Context, w *World, f Filter) []Finding {
	var out []Finding
	if f.Component == "" || f.Component == "server" {
		if _, ok := w.component("server"); ok {
			out = append(out, healthServer(ctx, w))
		}
	}
	if f.Component == "" || f.Component == "node" {
		if _, ok := w.component("node"); ok {
			x := fnd("health.node", LevelSkip, "Node Agent 与 Server 的连通、最近心跳、当前配置代 nginx -t 的检查随 Epic 3（Story 3.31）交付", "")
			x.Component = "node"
			out = append(out, x)
		}
	}
	return out
}

func healthServer(ctx context.Context, w *World) Finding {
	h := w.serverHealth(ctx)
	x := Finding{ID: "health.server", Component: "server", Details: map[string]any{"listen": h.listen}}
	if h.liveErr != nil || h.live == 0 {
		x.Level = LevelFail
		x.Message = "无法连接 Server（" + h.listen + "）：" + errText(h.liveErr)
		x.Hint = "lingedge service status server；lingedge logs server"
		return x
	}
	x.Details["live"] = h.live
	if h.live != 200 {
		x.Level, x.Message = LevelFail, "Server /healthz 返回 HTTP "+strconv.Itoa(h.live)
		x.Hint = "lingedge logs server"
		return x
	}
	switch h.initialized {
	case initNo:
		x.Level, x.Message = LevelPass, "Server 存活（setup mode：安装向导尚未完成，/healthz/ready 在向导完成前不可用）"
		return x
	}
	x.Details["ready"] = h.ready
	switch {
	case h.readyErr != nil || h.ready == 0:
		x.Level, x.Message = LevelFail, "Server 存活但 /healthz/ready 无响应："+errText(h.readyErr)
		x.Hint = "lingedge logs server"
	case h.ready == 200:
		x.Level, x.Message = LevelPass, "Server 存活且就绪（/healthz/ready 200）"
	case h.initialized == initUnknown:
		x.Level, x.Message = LevelWarn, fmt.Sprintf("Server 存活，/healthz/ready 返回 %d（无权读取 install.lock，无法区分 setup mode 与依赖未就绪）", h.ready)
		x.Hint = "以 root 运行 lingedge doctor；lingedge logs server"
	default:
		x.Level, x.Message = LevelFail, fmt.Sprintf("Server 存活但未就绪（/healthz/ready 返回 %d：数据库/迁移/依赖未就绪）", h.ready)
		x.Hint = "lingedge doctor 的 dep.mysql/dep.redis 与 lingedge logs server 可定位原因"
	}
	return x
}

func errText(err error) string {
	if err == nil {
		return "无响应"
	}
	return err.Error()
}
