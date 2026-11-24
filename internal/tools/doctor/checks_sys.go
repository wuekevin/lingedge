package doctor

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/resolver"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

func builtinGroups() []Group {
	return []Group{
		{Name: "sys", Category: "system", Prefixes: []string{"sys."}, Run: runSys},
		{Name: "port", Category: "port", Prefixes: []string{"port."}, PerComponent: true, Run: runPorts},
		{Name: "baota", Category: "baota", Prefixes: []string{"baota."}, Run: runBaota},
		{Name: "dep", Category: "dependency", Prefixes: []string{"dep."}, PerComponent: true, Run: runDeps},
		{Name: "svc", Category: "service", Prefixes: []string{"svc.", "service."}, PerComponent: true, Run: runSvc},
		{Name: "health", Category: "health", Prefixes: []string{"health."}, PerComponent: true, Run: runHealth},
		{Name: "fs", Category: "files", Prefixes: []string{"fs."}, PerComponent: true, Run: runFS},
		{Name: "fe", Category: "frontend", Prefixes: []string{"fe."}, PerComponent: true, Run: runFrontends},
		{Name: "cert", Category: "certificate", Prefixes: []string{"cert."}, PerComponent: true, Run: runCerts},
		{Name: "cfg", Category: "config", Prefixes: []string{"cfg."}, PerComponent: true, Run: runCfg},
		{Name: "state", Category: "install", Prefixes: []string{"state.", "journal."}, PerComponent: true, Run: runState},
		{Name: "backup", Category: "backup", Prefixes: []string{"backup."}, PerComponent: true, Run: runBackup},
		{Name: "ver", Category: "version", Prefixes: []string{"ver.", "trust."}, PerComponent: true, Run: runVer},
	}
}

func fnd(id string, lv Level, msg, hint string) Finding {
	return Finding{ID: id, Level: lv, Message: msg, Hint: hint}
}

// requirements 返回已装组件在清单里声明的系统要求的最大值（清单可用时）；src 说明来源。
func (w *World) requirements(ctx context.Context) (memMB, diskMB int64, ok bool) {
	m, err := w.manifestOnce(ctx)
	if err != nil || m == nil {
		return 0, 0, false
	}
	for _, c := range w.Components {
		mc, found := m.Components[c.Name]
		if !found {
			continue
		}
		for _, v := range mc.Versions {
			if v.Version != c.Version {
				continue
			}
			reqs := []*manifest.Requires{v.Requires}
			for _, va := range v.Variants {
				reqs = append(reqs, va.Requires)
			}
			for _, r := range reqs {
				if r != nil && r.System != nil {
					memMB, diskMB = max(memMB, r.System.MinMemoryMB), max(diskMB, r.System.MinDiskMB)
					ok = true
				}
			}
		}
	}
	return memMB, diskMB, ok
}

func runSys(ctx context.Context, w *World, f Filter) []Finding {
	cfg := w.cfg()
	var out []Finding
	res, perr := w.probe(ctx)
	if perr != nil {
		for _, id := range []string{"sys.os", "sys.arch", "sys.mem", "sys.disk"} {
			out = append(out, fnd(id, LevelSkip, "系统探测不可用："+perr.Error(), ""))
		}
	} else {
		sys := res.System
		// sys.os / sys.arch
		if sys.OS == "linux" {
			out = append(out, fnd("sys.os", LevelPass, "操作系统 linux", ""))
		} else {
			out = append(out, fnd("sys.os", LevelFail, "操作系统 "+sys.OS+" 不受支持（Tools 仅支持 Linux + systemd）", "在受支持的 Debian/Ubuntu/RHEL 系 Linux 上运行"))
		}
		if sys.Arch == "amd64" || sys.Arch == "arm64" {
			out = append(out, fnd("sys.arch", LevelPass, "架构 "+sys.Arch, ""))
		} else {
			out = append(out, fnd("sys.arch", LevelFail, "架构 "+sys.Arch+" 不受支持（支持 amd64、arm64）", ""))
		}
		reqMem, reqDisk, fromManifest := w.requirements(ctx)
		out = append(out, memFinding(sys, cfg, reqMem, fromManifest))
		out = append(out, diskFinding(sys, cfg, reqDisk, fromManifest))
	}
	out = append(out, nofileFinding(w, cfg))
	out = append(out, timeFinding(ctx, w, cfg))
	return out
}

func memFinding(sys resolver.System, cfg Config, reqMB int64, fromManifest bool) Finding {
	if sys.MemoryAvailableMB <= 0 {
		return fnd("sys.mem", LevelWarn, "无法读取可用内存（/proc/meminfo 不可用）", "")
	}
	avail := sys.MemoryAvailableMB
	d := map[string]any{"available_mb": avail, "total_mb": sys.MemoryTotalMB}
	f := Finding{ID: "sys.mem", Details: d}
	need, src := cfg.MinMemoryMB, "tools.yaml doctor.min_memory_mb（缺省 256）"
	if fromManifest && reqMB > 0 {
		need, src = reqMB, "清单声明的系统要求"
	}
	d["required_mb"], d["required_source"] = need, src
	switch {
	case avail < need && fromManifest && reqMB > 0:
		f.Level, f.Message = LevelFail, fmt.Sprintf("可用内存 %d MiB 低于清单要求的 %d MiB", avail, need)
		f.Hint = "释放内存或扩容；Tools 与组件的资源要求以清单为准"
	case avail < need/2:
		f.Level, f.Message = LevelFail, fmt.Sprintf("可用内存 %d MiB 远低于下限 %d MiB（来源：%s）", avail, need, src)
		f.Hint = "释放内存或扩容"
	case avail < need:
		f.Level, f.Message = LevelWarn, fmt.Sprintf("可用内存 %d MiB 低于下限 %d MiB（来源：%s）", avail, need, src)
		f.Hint = "释放内存或扩容"
	default:
		f.Level, f.Message = LevelPass, fmt.Sprintf("可用内存 %d MiB（要求 ≥ %d MiB）", avail, need)
	}
	return f
}

// 与 Tools 落盘相关的路径（用于定位挂载点）。
var diskPaths = []string{"/var/lib/lingedge", "/opt/lingedge", "/etc/lingedge", "/var/log/lingedge"}

func mountFor(disks []resolver.Disk, p string) (resolver.Disk, bool) {
	best, found := resolver.Disk{}, false
	for _, d := range disks {
		m := path.Clean(d.Mount)
		if m == "/" || p == m || strings.HasPrefix(p, m+"/") {
			if !found || len(m) > len(path.Clean(best.Mount)) {
				best, found = d, true
			}
		}
	}
	return best, found
}

func diskFinding(sys resolver.System, cfg Config, reqMB int64, fromManifest bool) Finding {
	f := Finding{ID: "sys.disk"}
	if len(sys.Disks) == 0 {
		f.Level, f.Message = LevelWarn, "无法读取磁盘空间（statfs/挂载信息不可用）"
		return f
	}
	need, src := cfg.MinDiskMB, "tools.yaml doctor.min_disk_mb（缺省 512）"
	strictReq := fromManifest && reqMB > 0
	if strictReq {
		need, src = reqMB, "清单声明的系统要求"
	}
	type row struct {
		Mount  string   `json:"mount"`
		FreeMB int64    `json:"free_mb"`
		Paths  []string `json:"paths"`
	}
	rows := map[string]*row{}
	var order []string
	var ro []string
	for _, p := range diskPaths {
		d, ok := mountFor(sys.Disks, p)
		if !ok {
			continue
		}
		r := rows[d.Mount]
		if r == nil {
			r = &row{Mount: d.Mount, FreeMB: d.FreeMB}
			rows[d.Mount] = r
			order = append(order, d.Mount)
			if d.ReadOnly {
				ro = append(ro, d.Mount)
			}
		}
		r.Paths = append(r.Paths, p)
	}
	sort.Strings(order)
	var list []row
	worst := int64(-1)
	for _, m := range order {
		list = append(list, *rows[m])
		if worst < 0 || rows[m].FreeMB < worst {
			worst = rows[m].FreeMB
		}
	}
	f.Details = map[string]any{"mounts": list, "required_mb": need, "required_source": src}
	switch {
	case len(ro) > 0:
		f.Level, f.Message = LevelFail, "挂载点 "+strings.Join(ro, "、")+" 是只读的，Tools 无法写入"
		f.Hint = "检查文件系统错误或重新以读写方式挂载"
	case worst < 0:
		f.Level, f.Message = LevelWarn, "找不到 /var/lib/lingedge 等路径所在的挂载点"
	case worst < need && strictReq, worst < need/4:
		f.Level, f.Message = LevelFail, fmt.Sprintf("最小剩余空间 %d MiB 低于要求 %d MiB（来源：%s）", worst, need, src)
		f.Hint = "清理磁盘或扩容；备份、发行包与日志都落在这些目录下"
	case worst < need:
		f.Level, f.Message = LevelWarn, fmt.Sprintf("最小剩余空间 %d MiB 低于 %d MiB（来源：%s）", worst, need, src)
		f.Hint = "清理磁盘或扩容"
	default:
		f.Level, f.Message = LevelPass, fmt.Sprintf("磁盘空间充足（最小剩余 %d MiB，要求 ≥ %d MiB）", worst, need)
	}
	return f
}

func nofileFinding(w *World, cfg Config) Finding {
	if w.Nofile == nil {
		return fnd("sys.ulimit_nofile", LevelSkip, "无法读取 ulimit -n", "")
	}
	n, err := w.Nofile()
	if err != nil {
		return fnd("sys.ulimit_nofile", LevelSkip, "无法读取 ulimit -n："+err.Error(), "")
	}
	f := Finding{ID: "sys.ulimit_nofile", Details: map[string]any{"soft_limit": n, "required": cfg.NofileMin}}
	if n < cfg.NofileMin {
		f.Level, f.Message = LevelWarn, fmt.Sprintf("Tools 进程的文件描述符软限制 %d 低于 %d", n, cfg.NofileMin)
		f.Hint = "提高 ulimit -n（组件自身的限制由其 systemd 单元的 LimitNOFILE 决定）"
	} else {
		f.Level, f.Message = LevelPass, fmt.Sprintf("文件描述符软限制 %d", n)
	}
	return f
}

func timeFinding(ctx context.Context, w *World, cfg Config) Finding {
	var info TimeInfo
	var err error
	if w.TimeSync != nil {
		info, err = w.TimeSync(ctx)
	}
	var marks []ClockMark
	if w.ClockMarks != nil {
		marks = w.ClockMarks()
	}
	return evalTimeSync(w.now(), cfg, info, err, marks)
}

// ---- service 包预置的检查项 ----

func fromServiceFinding(sf service.Finding) Finding {
	return Finding{ID: sf.ID, Category: sf.Category, Level: Level(sf.Level), Message: sf.Message, Hint: sf.Hint}
}
