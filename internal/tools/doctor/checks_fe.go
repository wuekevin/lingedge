package doctor

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/cfgcheck"
	"github.com/wuekevin/lingedge/internal/tools/installer"
)

// 静态前端（admin-web/console/docs/status/website static）的深度检查（Story 1.40 留给 1.42）：
//   - fe.<c>.release：current → releases/<版本>；public/index.html；index.html 引用的本地资源全部存在（含哈希命名的脚本/样式）；
//     预压缩 .gz 副本与原文件逐字节一致（nginx gzip_static 会直接下发 .gz，不一致 = 下发错内容）；
//   - fe.<c>.nginx：站点片段与安全头文件在位且通过静态检查（只读，不执行 nginx）、conf.d 软链状态。
func runFrontends(ctx context.Context, w *World, f Filter) []Finding {
	var out []Finding
	for _, c := range w.installed(f) {
		if !installer.IsStaticComponent(c.Name) {
			continue
		}
		out = append(out, feRelease(w, c), feNginx(w, c))
	}
	return out
}

const maxIndexBytes = 2 << 20

var reAssetRef = regexp.MustCompile(`(?i)\b(?:src|href)\s*=\s*["']([^"'#?]+)`)

// localRefs 提取 index.html 里指向站点内部的绝对路径引用（排除外链、协议相对、锚点、/api）。
func localRefs(html []byte) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range reAssetRef.FindAllSubmatch(html, 2000) {
		r := string(m[1])
		if !strings.HasPrefix(r, "/") || strings.HasPrefix(r, "//") || r == "/" || strings.HasPrefix(r, "/api/") {
			continue
		}
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
		if len(out) >= 500 {
			break
		}
	}
	sort.Strings(out)
	return out
}

func feRelease(w *World, c Component) Finding {
	x := Finding{ID: "fe." + c.Name + ".release", Component: c.Name}
	cur := w.path("/opt/lingedge/" + c.Name + "/current")
	target, err := os.Readlink(cur)
	if err != nil {
		x.Level, x.Message = LevelFail, "current 链接不可读（"+errText(err)+"）"
		x.Hint = "重新安装：lingedge install " + c.Name
		return x
	}
	if want := "releases/" + c.Version; target != want {
		x.Level, x.Message = LevelFail, fmt.Sprintf("current 指向 %q，与 state.json 记录的版本不一致（应为 %s）", target, want)
		x.Hint = "重新安装该版本，或从备份恢复 current 链接"
		return x
	}
	pub := filepath.Join(cur, "public")
	idxPath := filepath.Join(pub, "index.html")
	st, err := os.Stat(idxPath)
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
		x.Level, x.Message = LevelFail, "public/index.html 缺失或为空"
		x.Hint = "重新安装：lingedge install " + c.Name
		return x
	}
	if st.Size() > maxIndexBytes {
		x.Level, x.Message = LevelFail, "public/index.html 异常庞大（>2 MiB）"
		return x
	}
	html, err := os.ReadFile(idxPath)
	if err != nil {
		x.Level, x.Message = LevelFail, "无法读取 public/index.html："+errText(err)
		return x
	}
	var missing, gzBad []string
	refs := localRefs(html)
	checked, gzOK := 0, 0
	check := func(rel string) {
		clean := path.Clean("/" + rel)
		p := filepath.Join(pub, filepath.FromSlash(clean))
		if !strings.HasPrefix(p, pub+string(filepath.Separator)) {
			missing = append(missing, rel+"（越出站点根）")
			return
		}
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() {
			missing = append(missing, rel)
			return
		}
		checked++
		gzp := p + ".gz"
		if gi, err := os.Lstat(gzp); err == nil && gi.Mode().IsRegular() {
			if same, why := gzMatches(p, gzp); same {
				gzOK++
			} else {
				gzBad = append(gzBad, rel+".gz（"+why+"）")
			}
		}
	}
	for _, r := range refs {
		check(r)
	}
	check("/index.html")
	d := map[string]any{"version": c.Version, "assets_checked": checked, "gzip_verified": gzOK}
	x.Details = d
	switch {
	case len(missing) > 0:
		sort.Strings(missing)
		d["missing"] = firstN(missing, 10)
		x.Level, x.Message = LevelFail, fmt.Sprintf("index.html 引用的 %d 个资源缺失：%s", len(missing), strings.Join(firstN(missing, 5), "、"))
		x.Hint = "发行包不完整或被改动：重新安装该组件（会重新验签）"
	case len(gzBad) > 0:
		d["gzip_mismatch"] = firstN(gzBad, 10)
		x.Level, x.Message = LevelFail, "预压缩副本与原文件不一致："+strings.Join(firstN(gzBad, 5), "、")
		x.Hint = "nginx gzip_static 会直接下发 .gz；重新安装该组件"
	default:
		x.Level, x.Message = LevelPass, fmt.Sprintf("%s %s：current 正确，index.html 及其引用的 %d 个资源齐全，%d 个预压缩副本与原文件一致", c.Name, c.Version, len(refs), gzOK)
	}
	return x
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

const maxGzBytes = 16 << 20

// gzMatches 报告 gz 解压后是否与原文件逐字节一致（两边都限长，防压缩炸弹）。
func gzMatches(orig, gz string) (bool, string) {
	f, err := os.Open(gz)
	if err != nil {
		return false, "无法读取"
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return false, "不是合法的 gzip"
	}
	defer func() { _ = zr.Close() }()
	got, err := io.ReadAll(io.LimitReader(zr, maxGzBytes+1))
	if err != nil || len(got) > maxGzBytes {
		return false, "解压失败或过大"
	}
	of, err := os.Open(orig)
	if err != nil {
		return false, "原文件不可读"
	}
	defer func() { _ = of.Close() }()
	want, err := io.ReadAll(io.LimitReader(of, maxGzBytes+1))
	if err != nil {
		return false, "原文件不可读"
	}
	if !bytes.Equal(got, want) {
		return false, "内容不同"
	}
	return true, ""
}

func feNginx(w *World, c Component) Finding {
	x := Finding{ID: "fe." + c.Name + ".nginx", Component: c.Name}
	conf, cerr := readCapped(w.path(installer.StaticConfPath(c.Name)))
	hdr, herr := readCapped(w.path(installer.StaticHeadersPath(c.Name)))
	if cerr != nil || herr != nil {
		x.Level, x.Message = LevelFail, "站点片段或安全头文件缺失/不可读："+errText(firstErr(cerr, herr))
		x.Hint = "重新安装该组件会重新生成（已存在的文件不会被覆盖，新内容写入 .new）"
		return x
	}
	if probs := cfgcheck.CheckNginxSite(c.Name, conf, hdr); len(probs) > 0 {
		var ms []string
		for _, p := range probs {
			ms = append(ms, p.Reason)
		}
		x.Level, x.Message = LevelFail, "站点片段未通过静态检查："+strings.Join(firstN(ms, 3), "；")
		x.Hint = "片段被手工改动过？比对 " + installer.StaticConfPath(c.Name) + ".new（如有），或重新安装"
		x.Details = map[string]any{"problems": ms}
		return x
	}
	_, host, _ := installer.ParseSiteMarker(conf)
	d := map[string]any{"host": host}
	x.Details = d
	link := w.path(installer.StaticLinkPath(c.Name))
	lst, err := os.Lstat(link)
	switch {
	case err != nil && os.IsNotExist(err):
		d["conf_d_link"] = "absent"
		x.Level, x.Message = LevelPass, "站点片段与安全头文件正常（"+host+"）；未创建 conf.d 软链，需在 Nginx 配置里 include "+installer.StaticConfPath(c.Name)
	case err != nil:
		x.Level, x.Message = LevelWarn, "无法检查 conf.d 软链："+errText(err)
	case lst.Mode()&fs.ModeSymlink != 0:
		if t, rerr := os.Readlink(link); rerr == nil && t == installer.StaticConfPath(c.Name) {
			d["conf_d_link"] = "ours"
			x.Level, x.Message = LevelPass, "站点片段与安全头文件正常（"+host+"），conf.d 软链指向 Tools 片段"
			break
		}
		fallthrough
	default:
		d["conf_d_link"] = "foreign"
		x.Level, x.Message = LevelWarn, installer.StaticLinkPath(c.Name)+" 已存在但不是指向 Tools 片段的软链（站点可能没有用 Tools 的配置）"
		x.Hint = "手工核对；Tools 不会覆盖它"
	}
	return x
}

func readCapped(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, cfgcheck.MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > cfgcheck.MaxBytes {
		return nil, fmt.Errorf("文件过大")
	}
	return b, nil
}

func firstErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}
