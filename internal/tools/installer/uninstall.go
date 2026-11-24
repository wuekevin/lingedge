package installer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/backup"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/security/safeexec"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

// 卸载（Story 1.41 AC5，tools-design §3/§17）。
//
// 默认**保留**配置、数据与备份（FR-9「不默认删除用户数据」）：只摘除「Tools 装上去的运行时」——停止并停用 systemd 单元、删除单元文件、摘除 conf.d 软链（静态前端，
// nginx -t && reload）、删除 /opt/lingedge/<c>（releases 与 current 链接；二进制不是用户数据）、从 state.json 里移除该组件。
// --purge（须主机名确认，由 cli 在调用前完成）另外删除该组件的配置目录、数据目录、该组件的备份文件，以及静态前端的 Nginx 片段与安全头文件。
// 静态前端没有数据目录（只有 /etc/lingedge/nginx/<c>*.conf 配置，该目录由多个前端共用，purge 只删本组件的两个文件，绝不删目录）。
// Tools 不删除运行用户、不 DROP 数据库、不动 Nginx 的其他配置——这些要么可能被他人使用，要么就是用户数据。
//
// 步骤都是幂等的「向前」操作（卸载不可撤销）：journal 记录每一步，失败时事务以 abort 结束并报告 partial，重新运行 `lingedge uninstall <组件>` 即可完成；
// 进程被杀后 `recover --rollback` 只结束事务记录并说明这一点。state.json 的组件记录最后才删除，所以「已不在 state 里」就意味着卸载已完成。

// ActUninstallRequest 是 uninstall 事务的请求记录（撤销为空操作）；Extra 携带组件与 purge 标志，recover 据此识别这是一条卸载事务。
const ActUninstallRequest = "uninstall_request"

// 卸载步骤号（journal）。
const (
	UStepRequest = 1 // 请求记录
	UStepStop    = 2 // 停止并停用单元
	UStepUnits   = 3 // 删除单元文件 + daemon-reload
	UStepNginx   = 4 // 摘除 conf.d 软链（静态前端）
	UStepRelease = 5 // 删除 /opt/lingedge/<c>
	UStepPurge   = 6 // --purge：配置、数据、备份
	UStepState   = 7 // 从 state.json 移除（提交点）
)

var uninstallStepNames = map[int]string{UStepRequest: "uninstall_request", UStepStop: "stop_disable_units", UStepUnits: "remove_unit_files", UStepNginx: "remove_nginx_link",
	UStepRelease: "remove_release_tree", UStepPurge: "purge_data", UStepState: "forget_state"}

// UninstallRequest 是卸载请求。
type UninstallRequest struct {
	Component string
	Purge     bool
	DryRun    bool
}

// UninstallItem 是计划/结果里的一项。
type UninstallItem struct {
	Kind string `json:"kind"` // unit | unit_file | nginx_link | release | config | data | backup | nginx_snippet | state | user | database | note
	Path string `json:"path,omitempty"`
	// Action：remove（将删除/已删除）| keep（保留）| absent（本来就不存在）| skip（未动，见 Note）。
	Action string `json:"action"`
	Note   string `json:"note,omitempty"`
}

// UninstallResult 是卸载（或 --dry-run 计划）的结果。
type UninstallResult struct {
	Component string          `json:"component"`
	Outcome   string          `json:"outcome"` // planned | uninstalled
	Tx        string          `json:"tx,omitempty"`
	Purge     bool            `json:"purge"`
	DryRun    bool            `json:"dry_run"`
	Items     []UninstallItem `json:"items"`
	Warnings  []Warning       `json:"warnings"`
}

// 卸载结果取值。
const (
	OutcomeUninstalled = "uninstalled"
)

func componentUnits(c string) []service.UnitName {
	switch c {
	case "server":
		return []service.UnitName{service.UnitServer}
	case "node":
		return []service.UnitName{service.UnitNode, service.UnitOpenresty, service.UnitOpenrestyPath, service.UnitUpgradePath, service.UnitUpgradeService}
	}
	return nil
}

// purgeTargets 返回组件 --purge 要删除的目录（固定常量，不取自 state.json——state 被改动不应让 purge 指向别处）。
func purgeTargets(c string) (cfg, data []string) {
	switch c {
	case "server":
		return []string{service.ServerConfigDir}, []string{service.ServerDataDir}
	case "node":
		return []string{service.NodeConfigDir}, []string{service.NodeDataDir, service.NodeUpgradeDir, service.NodeLogDir}
	}
	return nil, nil
}

func (in *Installer) hostP(p string) string { return in.d.hostPath(p) }

func (in *Installer) backupDir() string { return in.hostP(backup.DefaultBackupDir) }

var reBackupOfComponent = func(c string) *regexp.Regexp {
	return regexp.MustCompile(`^lingedge-backup-.+-` + regexp.QuoteMeta(c) + `-[0-9]{8}T[0-9]{6}Z(-[0-9]+)?\.tar\.gz(\.enc)?$`)
}

func (in *Installer) listBackupsOf(c string) []string {
	ents, err := os.ReadDir(in.backupDir())
	if err != nil {
		return nil
	}
	re := reBackupOfComponent(c)
	var out []string
	for _, e := range ents {
		if re.MatchString(e.Name()) && e.Type().IsRegular() {
			out = append(out, filepath.Join(in.backupDir(), e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

func dirNonEmpty(p string) bool {
	ents, err := os.ReadDir(p)
	return err == nil && len(ents) > 0
}

func pathKind(p string) string {
	fi, err := os.Lstat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "absent"
	case err != nil:
		return "unknown"
	case fi.Mode()&fs.ModeSymlink != 0:
		return "symlink"
	case fi.IsDir():
		return "dir"
	}
	return "file"
}

// PlanUninstall 只读求出将删除与保留的内容（也是 --dry-run 的输出）。组件不在 state.json 里返回 KindNotInstalled。
func (in *Installer) PlanUninstall(ctx context.Context, req UninstallRequest) (*UninstallResult, error) {
	c := req.Component
	if !registry.ValidComponent(c) {
		return nil, newErr(KindSettings, "组件名 %q 不合法", c)
	}
	st, _, err := in.d.Reg.LoadState()
	if err != nil {
		return nil, wrap(KindStepFailed, err, "读取 state.json 失败")
	}
	rec, ok := st.Components[c]
	if !ok {
		return nil, &Error{Kind: KindNotInstalled, Msg: "组件 " + c + " 未安装", Hint: "lingedge list 查看已安装的组件", SystemState: "unchanged"}
	}
	_ = rec
	res := &UninstallResult{Component: c, Outcome: "planned", Purge: req.Purge, DryRun: req.DryRun, Items: []UninstallItem{}, Warnings: []Warning{}}
	add := func(kind, path, action, note string) {
		res.Items = append(res.Items, UninstallItem{Kind: kind, Path: path, Action: action, Note: note})
	}
	for _, u := range componentUnits(c) {
		fp := in.hostP(service.UnitDir + "/" + string(u))
		if pathKind(fp) == "absent" {
			continue
		}
		add("unit", string(u), "remove", "停止并停用")
		add("unit_file", service.UnitDir+"/"+string(u), "remove", "")
	}
	if IsStaticComponent(c) {
		switch ls, _ := in.linkStateOf(c); ls {
		case linkOurs:
			add("nginx_link", StaticLinkPath(c), "remove", "摘除软链后 nginx -t && reload")
		case linkForeign:
			add("nginx_link", StaticLinkPath(c), "skip", "不是指向 Tools 片段的软链，未动")
		}
	}
	rel := "/opt/lingedge/" + c
	if pathKind(in.hostP(rel)) != "absent" {
		add("release", rel, "remove", "releases 与 current 链接（二进制不是用户数据）")
	}
	cfgs, datas := purgeTargets(c)
	if IsStaticComponent(c) {
		for _, p := range []string{StaticConfPath(c), StaticHeadersPath(c)} {
			if pathKind(in.hostP(p)) == "absent" {
				continue
			}
			if req.Purge {
				add("nginx_snippet", p, "remove", "")
			} else {
				add("nginx_snippet", p, "keep", "配置默认保留；若已在 Nginx/宝塔里 include 它，请自行移除该 include")
			}
		}
		add("data", "", "keep", "静态前端没有数据目录")
	}
	for _, p := range cfgs {
		in.planDir(add, "config", p, req.Purge)
	}
	for _, p := range datas {
		in.planDir(add, "data", p, req.Purge)
	}
	for _, b := range in.listBackupsOf(c) {
		if req.Purge {
			add("backup", b, "remove", "")
		} else {
			add("backup", b, "keep", "")
		}
	}
	add("state", in.d.Reg.L.StateFile(), "remove", "移除 "+c+" 的组件记录")
	if c == "server" || c == "node" {
		add("user", "", "keep", "运行用户保留（可能被他人使用；需要时手工 userdel）")
	}
	if c == "server" {
		add("database", "", "keep", "Tools 从不删除数据库；如需删除请手工 DROP DATABASE 与 lingedge_* 账号")
		if req.Purge {
			msg := "--purge 将删除 " + service.ServerConfigDir + "/master.key：没有它，已加密的数据库内容与既有备份永久无法解密"
			if ks, err := backup.LoadKeySet(in.hostP(backup.ServerMasterKey), nil); err == nil {
				msg += "（主密钥指纹 " + backup.ShortFingerprint(ks.Overall) + "）"
			}
			res.Warnings = append(res.Warnings, Warning{Code: "uninstall.purge_master_key", Component: c, Message: msg + "。请确认你已按「与数据库备份分开保管」的要求另存了 master.key"})
		}
	}
	return res, nil
}

func (in *Installer) planDir(add func(kind, path, action, note string), kind, p string, purge bool) {
	k := pathKind(in.hostP(p))
	if k == "absent" {
		return
	}
	if purge {
		add(kind, p, "remove", "")
		return
	}
	note := "默认保留"
	if kind == "data" && k == "dir" && !dirNonEmpty(in.hostP(p)) {
		note = "默认保留（目录为空）"
	}
	add(kind, p, "keep", note)
}

func (in *Installer) failUninstall(j Journal, step int, cause error, begun []int) error {
	for i := len(begun) - 1; i >= 0; i-- {
		_ = j.Undone(begun[i])
	}
	_ = j.Abort(fmt.Sprintf("uninstall failed at step %d (%s)", step, uninstallStepNames[step]))
	_ = j.Archive()
	return &Error{Kind: KindStepFailed, Step: 0, Msg: fmt.Sprintf("卸载在第 %d 步（%s）失败，部分内容可能已被移除", step, uninstallStepNames[step]), Cause: cause,
		Hint: "卸载的每一步都是幂等的：排除原因后重新运行 lingedge uninstall 即可完成（组件记录仍在 state.json 里，说明尚未完成）", SystemState: "partial"}
}

// Uninstall 执行卸载。task 是写命令入口 registry.Begin 得到的任务（持有组件锁）。
func (in *Installer) Uninstall(ctx context.Context, task *registry.Task, req UninstallRequest) (*UninstallResult, error) {
	plan, err := in.PlanUninstall(ctx, req)
	if err != nil {
		return nil, err
	}
	if req.DryRun {
		return plan, nil
	}
	c := req.Component
	jr, err := task.OpenTx(ctx)
	if err != nil {
		return nil, err
	}
	var j Journal = jr
	if in.d.WrapJournal != nil {
		j = in.d.WrapJournal(j)
	}
	var begun []int
	begin := func(step int, undo registry.Undo) error {
		if err := j.Begin(step, uninstallStepNames[step], undo); err != nil {
			return err
		}
		begun = append(begun, step)
		return nil
	}
	hook := func(point string, step int) {
		if in.d.Hook != nil {
			in.d.Hook(fmt.Sprintf("%s:%d", point, step))
		}
	}
	run := func(step int, undo registry.Undo, fn func() error) error {
		hook("before", step)
		if err := begin(step, undo); err != nil {
			return in.failUninstall(j, step, err, begun)
		}
		hook("begun", step)
		if err := fn(); err != nil {
			return in.failUninstall(j, step, err, begun)
		}
		hook("effect", step)
		if err := j.Done(step); err != nil {
			return in.failUninstall(j, step, err, begun)
		}
		hook("done", step)
		return nil
	}
	if err := run(UStepRequest, registry.Undo{Actions: []registry.UndoAction{{Action: ActUninstallRequest, Extra: map[string]string{"component": c, "purge": fmt.Sprint(req.Purge)}}}}, func() error { return nil }); err != nil {
		return nil, err
	}
	var warns []Warning
	warn := func(code, msg string) {
		w := Warning{Code: code, Component: c, Message: msg}
		warns = append(warns, w)
		in.ev.emit(Event{Type: EventWarning, Code: code, Message: msg})
	}
	units := componentUnits(c)

	// 2) 停止并停用单元（单元文件不存在的跳过：systemctl 对未知单元报错）。
	if err := run(UStepStop, registry.Undo{}, func() error {
		var present []service.UnitName
		for _, u := range units {
			if pathKind(in.hostP(service.UnitDir+"/"+string(u))) != "absent" {
				present = append(present, u)
			}
		}
		if len(present) == 0 {
			return nil
		}
		for _, u := range present { // 逐个：一个停不掉也要看清是哪一个
			if err := in.sys.Stop(ctx, u); err != nil {
				return fmt.Errorf("停止 %s 失败: %w", u, err)
			}
			if err := in.sys.Disable(ctx, u); err != nil {
				return fmt.Errorf("停用 %s 失败: %w", u, err)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}

	// 3) 删除单元文件并 daemon-reload。
	if err := run(UStepUnits, registry.Undo{}, func() error {
		changed := false
		for _, u := range units {
			p := in.hostP(service.UnitDir + "/" + string(u))
			switch pathKind(p) {
			case "absent":
			case "file", "symlink":
				if err := os.Remove(p); err != nil {
					return err
				}
				changed = true
			default:
				return fmt.Errorf("%s 不是普通文件，拒绝删除", service.UnitDir+"/"+string(u))
			}
		}
		if changed {
			if err := in.sys.DaemonReload(ctx); err != nil {
				warn("uninstall.daemon_reload", "单元已删除，但 systemctl daemon-reload 失败："+err.Error()+"；请手工执行")
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}

	// 4) 摘除 conf.d 软链（静态前端）。
	if err := run(UStepNginx, registry.Undo{}, func() error {
		if !IsStaticComponent(c) {
			return nil
		}
		ls, lerr := in.linkStateOf(c)
		if lerr != nil {
			return lerr
		}
		switch ls {
		case linkAbsent:
			return nil
		case linkForeign:
			warn("uninstall.nginx_link_foreign", StaticLinkPath(c)+" 不是指向 Tools 片段的软链，未动")
			return nil
		}
		if err := os.Remove(in.hostP(StaticLinkPath(c))); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		in.reloadWebAfterUnlink(ctx, warn)
		return nil
	}); err != nil {
		return nil, err
	}

	// 5) 删除 /opt/lingedge/<c>。
	if err := run(UStepRelease, registry.Undo{}, func() error {
		rel := in.hostP("/opt/lingedge/" + c)
		switch pathKind(rel) {
		case "absent":
		case "dir":
			if err := os.RemoveAll(rel); err != nil {
				return err
			}
		default:
			return fmt.Errorf("/opt/lingedge/%s 不是真实目录，拒绝删除", c)
		}
		// /opt/lingedge 本身：空了才删（可能还有别的组件）。
		_ = os.Remove(in.hostP("/opt/lingedge"))
		return nil
	}); err != nil {
		return nil, err
	}

	// 6) --purge。
	if req.Purge {
		if err := run(UStepPurge, registry.Undo{}, func() error { return in.purge(c, warn) }); err != nil {
			return nil, err
		}
	}

	// 7) 提交点：从 state.json 移除。
	if err := run(UStepState, registry.Undo{}, func() error {
		return in.d.Reg.UpdateState(ctx, func(s *registry.State) error {
			delete(s.Components, c)
			return nil
		})
	}); err != nil {
		return nil, err
	}
	if err := j.Commit(); err != nil {
		return nil, &Error{Kind: KindRollbackFailed, Msg: "卸载已完成，但事务日志无法提交", Cause: err, SystemState: "needs_manual"}
	}
	if err := j.Archive(); err != nil {
		warn("journal.archive", "journal 归档失败（已提交，下次 recover 会补做）："+err.Error())
	}
	plan.Outcome, plan.Tx = OutcomeUninstalled, task.TxID()
	plan.Warnings = append(plan.Warnings, warns...)
	// 把计划里的 skip/absent 与实际一致化：计划即结果（步骤幂等，未知差异只会是「本来就不存在」）。
	return plan, nil
}

// reloadWebAfterUnlink：摘除软链后让运行中的 Nginx 与磁盘一致（nginx -t 通过才 reload；失败只告警——软链已摘除，磁盘状态正确）。
func (in *Installer) reloadWebAfterUnlink(ctx context.Context, warn func(code, msg string)) {
	run, err := in.webRunner()
	if err != nil {
		warn("uninstall.nginx_unavailable", "无法构造 Nginx 执行器："+err.Error()+"；软链已摘除，请手工 reload Nginx")
		return
	}
	for _, bin := range []string{"nginx", "openresty"} {
		err := webTest(ctx, run, bin)
		var we *webCtlError
		if err != nil && errors.As(err, &we) && (errors.Is(we.Err, safeexec.ErrNotFound) || errors.Is(we.Err, safeexec.ErrNotAllowed)) {
			continue // 没有这个可执行文件，试下一个
		}
		if err != nil {
			warn("uninstall.nginx_test_failed", err.Error()+"；软链已摘除，未 reload（请检查 Nginx 配置后手工 reload）")
			return
		}
		if err := webReload(ctx, run, bin); err != nil {
			warn("uninstall.nginx_reload_failed", err.Error()+"；软链已摘除，请手工 "+bin+" -s reload")
		}
		return
	}
	warn("uninstall.nginx_missing", "没有找到 nginx/openresty：软链已摘除，未 reload")
}

// purge 删除配置、数据、备份与 Nginx 片段。每个目标先确认是真实目录/文件（不跟随符号链接）且落在受管前缀内。
func (in *Installer) purge(c string, warn func(code, msg string)) error {
	rm := func(virtual string) error {
		if !service.IsManagedPath(virtual) && !strings.HasPrefix(virtual, "/etc/lingedge/") {
			return fmt.Errorf("拒绝删除受管目录之外的路径 %s", virtual)
		}
		p := in.hostP(virtual)
		switch pathKind(p) {
		case "absent":
			return nil
		case "dir", "file":
			return os.RemoveAll(p)
		default:
			return fmt.Errorf("%s 不是真实文件/目录，拒绝删除", virtual)
		}
	}
	cfgs, datas := purgeTargets(c)
	for _, p := range append(append([]string{}, cfgs...), datas...) {
		if err := rm(p); err != nil {
			return err
		}
	}
	if IsStaticComponent(c) {
		for _, p := range []string{StaticConfPath(c), StaticHeadersPath(c)} {
			if err := rm(p); err != nil {
				return err
			}
		}
	}
	for _, b := range in.listBackupsOf(c) {
		if err := os.Remove(b); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	// 空的父目录（/etc/lingedge、/var/lib/lingedge）不动：别的组件可能在用；registry 的状态目录在 /var/lib/lingedge/tools 下，更不能碰。
	return nil
}
