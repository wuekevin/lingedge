package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/security"
	"github.com/wuekevin/lingedge/internal/tools/security/safeexec"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

// ---- service status ----

func (r *Run) installedComponents(names []string) ([]ComponentInfo, *Error) {
	comps, err := r.env.State.Components(r.ctx)
	if err != nil {
		return nil, asCLIError(err)
	}
	if len(names) == 0 {
		return comps, nil
	}
	for _, c := range comps {
		if c.Name == names[0] {
			return []ComponentInfo{c}, nil
		}
	}
	return nil, notInstalled(names[0])
}

func statusResult(items []Inspected, warns []string) *Result {
	res := &Result{
		Data: map[string]any{"components": items, "summary": summarize(items)},
		Human: func(w io.Writer) {
			if len(items) == 0 {
				wln(w, "未安装任何组件")
				return
			}
			for _, it := range items {
				mark := "?"
				switch {
				case it.Healthy == nil:
				case *it.Healthy:
					mark = "✓"
				default:
					mark = "✗"
				}
				wf(w, "%s %s %s（记录：%s）\n", mark, it.Name, it.Version, it.State)
				if it.Systemd.Managed {
					if len(it.Systemd.Units) == 0 {
						wf(w, "    systemd：%s\n", it.Systemd.Note)
					}
					for _, u := range it.Systemd.Units {
						wf(w, "    %-34s %s/%s  开机自启：%s  主进程 %d  自动重启 %d 次\n", u.Unit, u.ActiveState, u.SubState, u.UnitFileState, u.MainPID, u.Restarts)
					}
				} else if it.Systemd.Note != "" {
					wf(w, "    systemd：%s\n", it.Systemd.Note)
				}
				wf(w, "    健康：%s —— %s\n", it.Health.Status, it.Health.Detail)
			}
		},
	}
	for _, w := range warns {
		res.Warnings = append(res.Warnings, Warning{Code: "systemd.unavailable", Message: w})
	}
	return res
}

func runServiceStatus(inv *invocation) (*Result, error) {
	r := inv.r
	comps, e := r.installedComponents(inv.args)
	if e != nil {
		return nil, e
	}
	items, warns := r.inspectComponents(r.ctx, comps)
	res := statusResult(items, warns)
	if he := healthFailedError(items); he != nil {
		return res, he
	}
	return res, nil
}

// ---- service start|stop|restart ----

type serviceOutcome struct {
	Component string   `json:"component"`
	Action    string   `json:"action"`
	Units     []string `json:"units"`
	DryRun    bool     `json:"dry_run,omitempty"`
	Active    *bool    `json:"active"`
	Health    string   `json:"health,omitempty"`
	Detail    string   `json:"detail,omitempty"`
	State     string   `json:"state,omitempty"`
}

func unitNames(us []service.UnitName) []string {
	out := make([]string, len(us))
	for i, u := range us {
		out[i] = string(u)
	}
	return out
}

// waitHealthy 在 install.health_timeout 内按 install.health_interval 轮询组件，直到主单元 active 且健康探测不是 unhealthy。
func (r *Run) waitHealthy(ctx context.Context, c ComponentInfo) Inspected {
	cfg := r.toolsConfigLenient()
	timeout, _ := cfg.HealthTimeout()
	interval, _ := cfg.HealthInterval()
	deadline := time.Now().Add(timeout)
	var it Inspected
	for {
		items, _ := r.inspectComponents(ctx, []ComponentInfo{c})
		it = items[0]
		if it.Healthy != nil && *it.Healthy {
			return it
		}
		if time.Now().Add(interval).After(deadline) || ctx.Err() != nil {
			return it
		}
		select {
		case <-ctx.Done():
			return it
		case <-time.After(interval):
		}
	}
}

func (r *Run) setRecordedState(ctx context.Context, name, state string) {
	err := r.registry().UpdateState(ctx, func(s *registry.State) error {
		if c, ok := s.Components[name]; ok {
			c.State = state
			s.Components[name] = c
		}
		return nil
	})
	if err != nil {
		r.warn("state.update_failed", "服务操作已完成，但无法更新 state.json 中的记录状态："+redactString(err.Error()))
	}
}

// controlService 执行 start|stop|restart。调用方已持有该组件的任务锁。
func (r *Run) controlService(ctx context.Context, name, action string) (*serviceOutcome, error) {
	comps, e := r.installedComponents([]string{name})
	if e != nil {
		return nil, e
	}
	c := comps[0]
	units := service.RunUnits(name)
	if len(units) == 0 {
		why := "该组件没有 Tools 管理的服务单元"
		if installer.IsStaticComponent(name) {
			why = "静态前端没有服务进程，由 Nginx 直接提供文件"
		}
		return nil, &Error{Exit: ExitPrecondition, Code: "service.no_service", Message: name + "：" + why,
			Hint: "静态前端的启停由 Nginx 决定（nginx -s reload）；lingedge status 查看其发布物与站点片段的健康", Details: map[string]any{"component": name}}
	}
	out := &serviceOutcome{Component: name, Action: action, Units: unitNames(units)}
	if r.opts.DryRun {
		out.DryRun = true
		return out, nil
	}
	sc := r.systemctl()
	if sc == nil {
		return nil, newErr(ExitPrecondition, "service.systemctl_unavailable", "无法构造 systemctl 执行器", "")
	}
	var err error
	switch action {
	case "start":
		_, err = sc.Start(ctx, units...)
	case "stop":
		err = sc.Stop(ctx, units...)
	case "restart":
		err = sc.Restart(ctx, units...)
	}
	if err != nil {
		return nil, err
	}
	if action == "stop" {
		r.setRecordedState(ctx, name, "stopped")
		out.State, out.Active = "stopped", boolp(false)
		if p, perr := sc.Show(ctx, units[0]); perr == nil {
			out.Active = boolp(p.ActiveState == "active")
		}
		return out, nil
	}
	it := r.waitHealthy(ctx, c)
	out.Active, out.Health, out.Detail = it.Systemd.Active, it.Health.Status, it.Health.Detail
	if it.Healthy != nil && *it.Healthy {
		r.setRecordedState(ctx, name, "running")
		out.State = "running"
		return out, nil
	}
	out.State = "failed"
	d := it.Health.Detail
	if it.Systemd.Active != nil && !*it.Systemd.Active && len(it.Systemd.Units) > 0 {
		d = it.Systemd.Units[0].Unit + " 状态 " + it.Systemd.Units[0].ActiveState
	}
	return out, &Error{Exit: ExitHealthFailed, Code: "health.unhealthy", Message: fmt.Sprintf("%s 已%s，但健康检查未通过：%s", name, map[string]string{"start": "启动", "restart": "重启"}[action], d),
		Hint: "lingedge logs " + name + " 查看原因；lingedge doctor " + name + " 诊断；Tools 不会自动回滚一次手工的服务操作", SystemState: "partial", Details: map[string]any{"component": name}}
}

func (r *Run) lockedRestart(inv *invocation, name string) error {
	_, err := r.controlService(r.ctx, name, "restart")
	return err
}

func serviceAction(action string) func(inv *invocation) (*Result, error) {
	return func(inv *invocation) (*Result, error) {
		r := inv.r
		out, err := r.controlService(r.ctx, inv.args[0], action)
		if out == nil {
			return nil, err
		}
		verb := map[string]string{"start": "启动", "stop": "停止", "restart": "重启"}[action]
		res := &Result{Data: out, Human: func(w io.Writer) {
			if out.DryRun {
				wf(w, "将%s %s（%s）——--dry-run：未做任何改动\n", verb, out.Component, strings.Join(out.Units, "、"))
				return
			}
			switch {
			case action == "stop" && out.Active != nil && !*out.Active:
				wf(w, "已停止 %s（%s）\n", out.Component, strings.Join(out.Units, "、"))
			case action == "stop":
				wf(w, "已执行停止，但 %s 仍为 active\n", out.Units[0])
			case out.State == "running":
				wf(w, "已%s %s，健康检查通过（%s）\n", verb, out.Component, out.Detail)
			default:
				wf(w, "已%s %s，但健康检查未通过：%s\n", verb, out.Component, out.Detail)
			}
		}}
		return res, err
	}
}

// ---- logs ----

const (
	maxLogLine  = 64 << 10
	maxJSONLogs = 20000
)

// lineFilter 逐行脱敏地转发日志：令牌/口令/DSN 口令经 security 脱敏，PEM 块整体隐去；超长行截断。
type lineFilter struct {
	w     io.Writer
	buf   []byte
	inPEM bool
	skip  bool // 超长行的剩余部分
	lines int
	sink  func(line string)
}

func (f *lineFilter) emit(line string) error {
	switch {
	case strings.Contains(line, "-----BEGIN "):
		f.inPEM = true
		line = "***[PEM 块已隐去]***"
	case f.inPEM:
		if strings.Contains(line, "-----END ") {
			f.inPEM = false
		}
		return nil
	default:
		line = security.RedactString(line)
	}
	f.lines++
	if f.sink != nil {
		f.sink(line)
		return nil
	}
	_, err := io.WriteString(f.w, line+"\n")
	return err
}

func (f *lineFilter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if f.skip {
				return n, nil
			}
			f.buf = append(f.buf, p...)
			if len(f.buf) > maxLogLine {
				if err := f.emit(string(f.buf[:maxLogLine]) + "…[行过长已截断]"); err != nil {
					return n, err
				}
				f.buf, f.skip = f.buf[:0], true
			}
			return n, nil
		}
		chunk := p[:i]
		p = p[i+1:]
		if f.skip {
			f.skip = false
			continue
		}
		f.buf = append(f.buf, chunk...)
		line := string(f.buf)
		if len(line) > maxLogLine {
			line = line[:maxLogLine] + "…[行过长已截断]"
		}
		f.buf = f.buf[:0]
		if err := f.emit(strings.TrimRight(line, "\r")); err != nil {
			return n, err
		}
	}
	return n, nil
}

func (f *lineFilter) Close() error {
	if len(f.buf) > 0 && !f.skip {
		line := string(f.buf)
		f.buf = nil
		return f.emit(line)
	}
	return nil
}

func (r *Run) journalRunner() (StreamRunner, error) {
	if j := r.ops().Journal; j != nil {
		return j, nil
	}
	return safeexec.New(safeexec.Options{Allowed: []string{"journalctl"}, SearchDirs: safeexec.DefaultSearchDirs, MaxOutput: 64 << 10})
}

func runLogs(inv *invocation) (*Result, error) {
	r := inv.r
	name := inv.args[0]
	comps, e := r.installedComponents([]string{name})
	if e != nil {
		return nil, e
	}
	_ = comps
	units := service.LogUnits(name)
	if len(units) == 0 {
		why := "该组件没有 Tools 管理的服务单元"
		if installer.IsStaticComponent(name) {
			why = "静态前端没有服务进程，没有 journal 日志；访问日志由 Nginx 管理"
		}
		return nil, &Error{Exit: ExitPrecondition, Code: "logs.no_service", Message: name + "：" + why, Details: map[string]any{"component": name}}
	}
	follow := inv.flag("follow")
	if follow && r.jsonMode() {
		return nil, usageErr("usage.flag_not_applicable", "--follow 不能与 --json 同用（持续输出无法装进单个 JSON 文档）", "去掉 --follow，或去掉 --json")
	}
	n, _ := inv.cmd.Flags().GetInt("lines")
	args := []string{"--no-pager", "--output=short-iso", "-n", fmt.Sprint(n)}
	for _, u := range units {
		args = append(args, "-u", string(u))
	}
	if s := inv.str("since"); s != "" {
		d, _ := time.ParseDuration(s)
		args = append(args, fmt.Sprintf("--since=-%ds", int64(d.Seconds())))
	}
	if follow {
		args = append(args, "--follow")
	}
	run, err := r.journalRunner()
	if err != nil {
		return nil, newErr(ExitGeneral, "general.unexpected", "无法构造 journalctl 执行器："+err.Error(), "")
	}
	ctx := r.ctx
	if !follow {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
	}
	var collected []string
	truncated := false
	f := &lineFilter{w: r.env.Stdout}
	if r.jsonMode() {
		f.sink = func(l string) {
			if len(collected) >= maxJSONLogs {
				truncated = true
				return
			}
			collected = append(collected, l)
		}
	}
	serr := run.Stream(ctx, safeexec.Cmd{Path: "journalctl", Args: args}, f, nil)
	_ = f.Close()
	if serr != nil {
		switch {
		case errors.Is(serr, context.Canceled) && follow:
			// 用户中断跟随（Ctrl-C）：这是 -f 的正常结束方式。
		case errors.Is(serr, safeexec.ErrNotFound) || errors.Is(serr, safeexec.ErrNotAllowed):
			return nil, &Error{Exit: ExitPrecondition, Code: "logs.journalctl_unavailable", Message: "本机没有可用的 journalctl（Tools 仅支持 Linux + systemd）", Hint: "确认 systemd-journald 与 journalctl 已安装"}
		case errors.Is(serr, context.DeadlineExceeded):
			return nil, &Error{Exit: ExitGeneral, Code: "logs.timeout", Message: "读取日志超时", Hint: "缩小范围：--since 10m、-n 100"}
		default:
			return nil, &Error{Exit: ExitGeneral, Code: "logs.journalctl_failed", Message: "journalctl 执行失败：" + redactString(serr.Error()),
				Hint: "非 root 用户需要在 systemd-journal 组才能读取系统单元日志；请 sudo lingedge logs " + name}
		}
	}
	if !r.jsonMode() {
		return &Result{Data: map[string]any{"component": name, "units": unitNames(units)}}, nil
	}
	if collected == nil {
		collected = []string{}
	}
	return &Result{Data: map[string]any{"component": name, "units": unitNames(units), "lines": collected, "truncated": truncated}}, nil
}
