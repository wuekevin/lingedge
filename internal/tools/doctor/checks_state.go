package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/backup"
	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

// ---- cfg.validate ----

func runCfg(ctx context.Context, w *World, f Filter) []Finding {
	if w.ConfigVerify == nil {
		return []Finding{fnd("cfg.validate", LevelSkip, "配置校验不可用", "")}
	}
	var targets []string
	if f.Component == "" {
		targets = append(targets, "tools")
	}
	for _, c := range w.installed(f) {
		targets = append(targets, c.Name)
	}
	x := Finding{ID: "cfg.validate", Details: map[string]any{}}
	if f.Component != "" {
		x.Component = f.Component
	}
	var bad, partial, skipped []string
	var detail []map[string]any
	for _, t := range targets {
		v := w.ConfigVerify(ctx, t)
		switch {
		case v.Unavailable != "":
			skipped = append(skipped, t+"："+v.Unavailable)
		case len(v.Problems) > 0:
			for _, p := range v.Problems {
				bad = append(bad, t+" "+p.String())
			}
			detail = append(detail, map[string]any{"target": t, "file": v.File, "problems": v.Problems})
		case v.Partial != "":
			partial = append(partial, t+"："+v.Partial)
		}
	}
	if len(detail) > 0 {
		x.Details["targets"] = detail
	}
	if len(skipped) > 0 {
		x.Details["unavailable"] = skipped
	}
	if len(partial) > 0 {
		x.Details["partial"] = partial
	}
	switch {
	case len(bad) > 0:
		x.Level = LevelFail
		x.Message = fmt.Sprintf("配置有 %d 个问题：%s", len(bad), strings.Join(firstN(bad, 3), "；"))
		x.Hint = "lingedge config validate 查看全部（含行列）；lingedge config edit <组件> 编辑并自动校验"
	case len(partial) > 0:
		x.Level = LevelWarn
		x.Message = "配置没有发现问题，但部分只做了结构校验：" + strings.Join(partial, "；")
		x.Hint = "安装 lingedge-server 后重新运行以获得与 Server 启动一致的完整校验"
	case len(skipped) > 0 && len(skipped) == len(targets):
		x.Level, x.Message = LevelSkip, "无法校验配置："+strings.Join(skipped, "；")
		x.Hint = "以 root 运行 lingedge doctor"
	case len(skipped) > 0:
		x.Level = LevelWarn
		x.Message = "已校验的配置没有问题，但有目标无法校验：" + strings.Join(skipped, "；")
	default:
		x.Level, x.Message = LevelPass, fmt.Sprintf("%d 个配置目标通过校验（%s）", len(targets), strings.Join(targets, "、"))
	}
	if len(x.Details) == 0 {
		x.Details = nil
	}
	return []Finding{x}
}

// ---- state.drift / journal.incomplete ----

func runState(ctx context.Context, w *World, f Filter) []Finding {
	var out []Finding
	out = append(out, stateDrift(ctx, w, f))
	if f.Component == "" {
		out = append(out, journalIncomplete(ctx, w, f))
	}
	return out
}

func stateDrift(ctx context.Context, w *World, f Filter) Finding {
	x := Finding{ID: "state.drift"}
	if w.StateErr != nil {
		x.Level, x.Message = LevelFail, "state.json 无法读取或已损坏："+w.StateErr.Error()
		x.Hint = "不要手工编辑；从备份恢复 /var/lib/lingedge/tools/state.json，或联系支持（lingedge doctor --json 输出）"
		return x
	}
	var drift []string
	for _, c := range w.installed(f) {
		if want := "/opt/lingedge/" + c.Name; c.ReleaseDir != "" {
			if _, err := os.Lstat(w.path(c.ReleaseDir)); err != nil {
				drift = append(drift, fmt.Sprintf("%s：release 目录 %s 不存在", c.Name, c.ReleaseDir))
			}
			if t, err := os.Readlink(w.path(want + "/current")); err != nil {
				drift = append(drift, fmt.Sprintf("%s：%s/current 链接缺失", c.Name, want))
			} else if t != "releases/"+c.Version {
				drift = append(drift, fmt.Sprintf("%s：current 指向 %s，state.json 记录版本 %s", c.Name, t, c.Version))
			}
		}
		if c.Unit != "" {
			if _, err := os.Lstat(w.path(service.UnitDir + "/" + c.Unit)); err != nil {
				drift = append(drift, fmt.Sprintf("%s：单元文件 %s/%s 不存在", c.Name, service.UnitDir, c.Unit))
			}
		}
		for _, d := range []struct{ what, p string }{{"配置目录", c.ConfigDir}, {"数据目录", c.DataDir}} {
			if d.p == "" {
				continue
			}
			if _, err := os.Lstat(w.path(d.p)); err != nil {
				drift = append(drift, fmt.Sprintf("%s：%s %s 不存在", c.Name, d.what, d.p))
			}
		}
		if w.Own != nil && c.RunUser != "" {
			if _, _, err := w.Own.Resolve(c.RunUser, c.RunUser); err != nil {
				drift = append(drift, fmt.Sprintf("%s：运行用户 %s 不存在", c.Name, c.RunUser))
			}
		}
	}
	if f.Component == "" {
		// 反向：磁盘上有 Tools 的 release 布局（/opt/lingedge/<c>/current），state.json 却没有记录。
		ents, err := os.ReadDir(w.path("/opt/lingedge"))
		if err == nil {
			for _, e := range ents {
				if !e.IsDir() {
					continue
				}
				if _, ok := w.component(e.Name()); ok {
					continue
				}
				if _, err := os.Lstat(w.path("/opt/lingedge/" + e.Name() + "/current")); err == nil {
					drift = append(drift, fmt.Sprintf("/opt/lingedge/%s 存在安装布局，但 state.json 没有该组件的记录", e.Name()))
				}
			}
		}
	}
	if len(drift) > 0 {
		sort.Strings(drift)
		x.Level = LevelFail
		x.Message = fmt.Sprintf("state.json 与磁盘/systemd 实际不一致（%d 处）：%s", len(drift), strings.Join(firstN(drift, 3), "；"))
		x.Hint = "手工改动过安装目录？重新安装该组件，或从备份恢复；先运行 lingedge doctor --json 留存全部差异"
		x.Details = map[string]any{"drift": firstN(drift, 50)}
		return x
	}
	if len(w.installed(f)) == 0 {
		x.Level, x.Message = LevelPass, "没有已装组件需要核对"
		return x
	}
	x.Level, x.Message = LevelPass, "state.json 与磁盘、单元文件一致"
	return x
}

func journalIncomplete(ctx context.Context, w *World, f Filter) Finding {
	x := Finding{ID: "journal.incomplete"}
	if w.Journals == nil {
		x.Level, x.Message = LevelSkip, "无法读取事务日志"
		return x
	}
	txs, err := w.Journals(ctx)
	if err != nil {
		x.Level, x.Message = LevelSkip, "无法读取事务日志（"+errText(err)+"）"
		x.Hint = "以 root 运行 lingedge doctor"
		return x
	}
	var dead, live []string
	for _, t := range txs {
		if t.Status == "committed" || t.Status == "aborted" {
			continue
		}
		label := t.ID
		if t.Command != "" {
			label += "（" + t.Command + "）"
		}
		if t.Live {
			live = append(live, label)
		} else {
			dead = append(dead, label)
		}
	}
	switch {
	case len(dead) > 0:
		x.Level = LevelFail
		x.Message = fmt.Sprintf("存在 %d 个被中断且未结束的事务：%s", len(dead), strings.Join(firstN(dead, 3), "、"))
		x.Hint = "lingedge recover --resume 继续，或 lingedge recover --rollback 回滚（在此之前写命令会以退出码 13 拒绝）"
		x.Details = map[string]any{"transactions": dead}
	case len(live) > 0:
		x.Level, x.Message = LevelPass, fmt.Sprintf("有 %d 个事务正在进行（持有者进程仍在运行）", len(live))
	default:
		x.Level, x.Message = LevelPass, "没有未完成的事务"
	}
	return x
}

// ---- backup.* ----

func runBackup(ctx context.Context, w *World, f Filter) []Finding {
	var out []Finding
	if f.Component == "" || f.Component == "server" || f.Component == "node" {
		out = append(out, backupRecent(w, f))
	}
	if f.Component == "" || f.Component == "server" {
		out = append(out, backupMasterKey(ctx, w))
	}
	return out
}

// backupComps 是有备份语义的已装组件（server/node）。
func backupComps(w *World, f Filter) []Component {
	var out []Component
	for _, c := range w.installed(f) {
		if c.Name == "server" || c.Name == "node" {
			out = append(out, c)
		}
	}
	return out
}

func backupAge(e backup.ListEntry) (time.Time, bool) {
	if e.Meta != nil {
		if t, err := time.Parse(time.RFC3339, e.Meta.CreatedAt); err == nil {
			return t, true
		}
	}
	if t, err := time.Parse(time.RFC3339, e.ModTime); err == nil {
		return t, true
	}
	return time.Time{}, false
}

func backupRecent(w *World, f Filter) Finding {
	const id = "backup.recent"
	cfg := w.cfg()
	comps := backupComps(w, f)
	if len(comps) == 0 {
		return fnd(id, LevelSkip, "没有需要备份的已装组件（server/node）", "")
	}
	if w.Backups == nil {
		return fnd(id, LevelSkip, "无法列出备份", "")
	}
	entries, err := w.Backups()
	if err != nil {
		x := fnd(id, LevelSkip, "无法读取备份目录（需要 root）："+errText(err), "以 root 运行 lingedge doctor")
		return x
	}
	now := w.now()
	var stale, never []string
	d := map[string]any{"max_age_hours": int64(cfg.BackupMaxAge.Hours())}
	latest := map[string]string{}
	for _, c := range comps {
		var best time.Time
		for _, e := range entries {
			if e.Status != "ok" || e.Meta == nil || e.Meta.Component != c.Name || e.Meta.Purpose == "restore_safety" {
				continue
			}
			if t, ok := backupAge(e); ok && t.After(best) {
				best = t
			}
		}
		switch {
		case best.IsZero():
			// server 向导未完成时没有数据，不要求备份。
			if c.Name == "server" && w.serverInitialized() == initNo {
				continue
			}
			never = append(never, c.Name)
		case now.Sub(best) > cfg.BackupMaxAge:
			stale = append(stale, fmt.Sprintf("%s（最近一次 %s，距今 %s）", c.Name, best.UTC().Format("2006-01-02 15:04 UTC"), fmtDays(now.Sub(best))))
			latest[c.Name] = best.UTC().Format(time.RFC3339)
		default:
			latest[c.Name] = best.UTC().Format(time.RFC3339)
		}
	}
	d["latest"] = latest
	x := Finding{ID: id, Details: d}
	switch {
	case len(never) > 0:
		x.Level, x.Message = LevelWarn, "从未备份过："+strings.Join(never, "、")
		x.Hint = "lingedge backup create --component server --encrypt（主密钥必须与数据库备份分开保管）"
	case len(stale) > 0:
		x.Level, x.Message = LevelWarn, "备份已过期（超过 "+fmtDays(cfg.BackupMaxAge)+"）："+strings.Join(stale, "；")
		x.Hint = "lingedge backup create；阈值可在 tools.yaml 的 backup.recent_max_age 调整（缺省 168h 是 [ASSUMPTION]，待产品确认）"
	case len(latest) == 0:
		x.Level, x.Message = LevelSkip, "Server 尚未完成安装向导，暂无数据需要备份"
	default:
		x.Level, x.Message = LevelPass, "最近备份在 "+fmtDays(cfg.BackupMaxAge)+" 之内"
	}
	return x
}

// serverInitialized 读取 install.lock 判定向导是否完成（不探测 HTTP）。
func (w *World) serverInitialized() initState {
	c, ok := w.component("server")
	if !ok {
		return initUnknown
	}
	dir := c.DataDir
	if dir == "" {
		dir = service.ServerDataDir
	}
	_, err := os.Lstat(w.path(dir + "/install.lock"))
	switch {
	case err == nil:
		return initYes
	case errors.Is(err, fs.ErrNotExist):
		return initNo
	}
	return initUnknown
}

func backupMasterKey(ctx context.Context, w *World) Finding {
	const id = "backup.master_key_separate"
	if _, ok := w.component("server"); !ok {
		return fnd(id, LevelSkip, "未安装 server", "")
	}
	x := Finding{ID: id, Component: "server"}
	if w.serverInitialized() == initNo {
		x.Level, x.Message = LevelSkip, "Server 尚未完成安装向导，还没有需要主密钥保护的数据"
		return x
	}
	if w.MasterKeyFingerprint == nil {
		x.Level, x.Message = LevelSkip, "无法读取主密钥指纹"
		return x
	}
	fp, err := w.MasterKeyFingerprint()
	if err != nil {
		x.Level, x.Message = LevelSkip, "无法读取主密钥指纹（需要 root）："+errText(err)
		x.Hint = "以 root 运行 lingedge doctor"
		return x
	}
	short := backup.ShortFingerprint(fp)
	d := map[string]any{"fingerprint": short}
	x.Details = d
	// 备份文件里内置了主密钥 = 密钥与数据放在同一处（虽然是加密备份，但 F-7 要求分开保管）。
	if w.Backups != nil {
		if entries, berr := w.Backups(); berr == nil {
			var together []string
			for _, e := range entries {
				if e.Status == "ok" && e.Meta != nil && e.Meta.Component == "server" && e.Meta.MasterKeyIncluded {
					together = append(together, e.Name)
				}
			}
			if len(together) > 0 {
				sort.Strings(together)
				d["co_located_backups"] = firstN(together, 10)
				x.Level = LevelWarn
				x.Message = fmt.Sprintf("%d 个备份把主密钥与数据库放在同一个文件里（%s）：主密钥应与数据库备份分开保管", len(together), strings.Join(firstN(together, 2), "、"))
				x.Hint = "另存 master.key 到独立介质，并用不含 --include-master-key 的备份；备份文件与口令同时泄露即全部泄露"
				return x
			}
		}
	}
	rec := w.cfg().CustodyFingerprint
	switch {
	case rec == "":
		x.Level = LevelWarn
		x.Message = "尚未记录主密钥（指纹 " + short + "）已独立保管：丢失 master.key 意味着加密数据无法解密，备份也无法恢复"
		x.Hint = "把 master.key 另存到独立介质后，在 tools.yaml 写入 backup.custody_fingerprint: " + short + "（本项仅提示，不读取、不校验保管位置）"
	case strings.HasPrefix(fp, rec):
		x.Level, x.Message = LevelPass, "已记录主密钥（指纹 "+short+"）独立保管；备份不含主密钥"
	default:
		x.Level = LevelWarn
		x.Message = "tools.yaml 记录的主密钥指纹与当前 master.key 不一致（当前 " + short + "）：主密钥已被轮换或替换"
		x.Hint = "重新备份当前 master.key 并更新 backup.custody_fingerprint"
	}
	return x
}

// ---- ver.* / trust.* ----

func runVer(ctx context.Context, w *World, f Filter) []Finding {
	if f.Component != "" {
		if _, ok := w.component(f.Component); !ok {
			return nil
		}
	}
	if len(w.Components) == 0 && f.Component == "" {
		return nil
	}
	m, err := w.manifestOnce(ctx)
	skip := func(why string) []Finding {
		return []Finding{fnd("ver.revoked", LevelSkip, why, ""), fnd("ver.update_available", LevelSkip, why, "")}
	}
	if w.Manifest == nil || w.Cfg.Offline {
		return skip("离线模式或没有可用的发行源（tools.yaml 的 release.base_url 未配置 / doctor.offline=true），不检查吊销与更新")
	}
	if err != nil {
		var me *manifest.Error
		if errors.As(err, &me) {
			switch me.Kind {
			case manifest.KindExpired:
				return append(skip("清单已过期"), fnd("trust.manifest_valid", LevelFail, "发行清单已过 valid_until（"+me.Msg+"）", "发行源未更新清单，或本机时钟有误（见 sys.time_sync）；不要绕过：获取最新清单"))
			case manifest.KindSignature, manifest.KindUnknownKey, manifest.KindKeyRevoked, manifest.KindRollback, manifest.KindRotation, manifest.KindKeyInvalid:
				return append(skip("清单验证失败"), fnd("trust.manifest_valid", LevelFail, "发行清单验证失败："+me.Msg, "清单可能被篡改或回滚；核对发行源，不要绕过校验（lingedge update check 会以退出码 5 拒绝同一份清单）"))
			}
		}
		why := "无法获取/验证发行清单：" + errText(err)
		return append(skip(why), fnd("trust.manifest_valid", LevelWarn, why, "检查网络与 tools.yaml 的 release.*；气隙环境请设置 doctor.offline: true"))
	}
	if m == nil {
		return skip("没有清单")
	}
	var revoked, updates []string
	upd := []map[string]string{}
	for _, c := range w.installed(f) {
		for _, rv := range m.RevokedVersions {
			if rv.Component == c.Name && rv.Version == c.Version {
				reason := rv.Reason
				if reason == "" {
					reason = "未说明原因"
				}
				revoked = append(revoked, fmt.Sprintf("%s %s（%s）", c.Name, c.Version, reason))
			}
		}
		if mc, ok := m.Components[c.Name]; ok {
			cur, cerr := manifest.ParseVersion(c.Version)
			if cerr != nil {
				continue
			}
			var best *manifest.Version
			for _, v := range mc.Versions {
				pv, perr := manifest.ParseVersion(v.Version)
				if perr != nil || pv.Prerelease() || pv.Compare(cur) <= 0 {
					continue
				}
				if best == nil || pv.Compare(*best) > 0 {
					p := pv
					best = &p
				}
			}
			if best != nil {
				updates = append(updates, fmt.Sprintf("%s %s → %s", c.Name, c.Version, best.String()))
				upd = append(upd, map[string]string{"component": c.Name, "current": c.Version, "available": best.String()})
			}
		}
	}
	var out []Finding
	if len(revoked) > 0 {
		out = append(out, fnd("ver.revoked", LevelFail, "已装版本被发行方吊销："+strings.Join(revoked, "；"), "尽快升级到未吊销的版本：lingedge update check；lingedge update <组件>"))
	} else {
		out = append(out, fnd("ver.revoked", LevelPass, "已装版本均未被吊销", ""))
	}
	u := fnd("ver.update_available", LevelPass, "已装组件都是通道内的最新稳定版", "")
	if len(updates) > 0 {
		u.Message = "有可用更新：" + strings.Join(updates, "；")
		u.Hint = "lingedge update check 查看迁移预告；lingedge update <组件>"
		u.Details = map[string]any{"updates": upd}
	}
	out = append(out, u)
	rem := m.ValidUntil.Sub(w.now())
	tv := fnd("trust.manifest_valid", LevelPass, "发行清单有效（valid_until "+m.ValidUntil.UTC().Format("2006-01-02")+"）", "")
	if rem < 72*time.Hour {
		tv.Level, tv.Message = LevelWarn, "发行清单即将过期（valid_until "+m.ValidUntil.UTC().Format(time.RFC3339)+"）"
		tv.Hint = "发行方应尽快发布新清单；过期后 install/update 会以退出码 5 拒绝（气隙环境用 --offline 且显式 --allow-expired-manifest）"
	}
	out = append(out, tv)
	return out
}
