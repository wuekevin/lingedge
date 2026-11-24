package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/manifest"
)

const (
	modulePath       = "github.com/wuekevin/lingedge"
	builtinKeysRel   = "internal/tools/manifest/builtin/keys.json"
	testPubKeysRel   = "scripts/gates/data/test-release-pubkeys.txt"
	gateArgsRequired = "release-keys"
)

type treeFlags map[string]string // platform → 目录

func (t treeFlags) String() string { return fmt.Sprint(map[string]string(t)) }
func (t treeFlags) Set(v string) error {
	plat, dir, ok := strings.Cut(v, "=")
	if !ok || !rePlatformFlag.MatchString(plat) || dir == "" {
		return fmt.Errorf("格式应为 <platform>=<目录>，收到 %q", v)
	}
	t[plat] = dir
	return nil
}

type buildOpts struct {
	src, out, version string
	platforms         []string
	repro, fresh      bool
	allowDirty        bool
	lockSHA           string // frontend/pnpm-lock.yaml 的 SHA256（仅提供 -frontend-dist 时）
	minUpgradeFrom    string
	openrestyVersion  string
	openrestyTrees    treeFlags
	frontendDirs      treeFlags // 组件 → 前端生产构建目录（-frontend-dist；键是组件名而不是平台）
	stdout            io.Writer
}

// buildRecord 是 build-record.json：本次构建的可审计事实。
type buildRecord struct {
	Release       string            `json:"release"`
	Commit        string            `json:"commit"`
	CommitTime    string            `json:"commit_time"`
	Dirty         bool              `json:"dirty"`
	GoVersion     string            `json:"go_version"`
	BuildFlags    []string          `json:"build_flags"`
	LDFlags       string            `json:"ldflags"`
	Tags          string            `json:"build_tags"`
	Platforms     []string          `json:"platforms"`
	Binaries      []binRecord       `json:"binaries"`
	Frontends     []frontendRecord  `json:"frontends,omitempty"`
	LinksResolved []linkRecord      `json:"links_resolved,omitempty"`
	Migrations    *migrationsRecord `json:"migrations,omitempty"`
	NPM           string            `json:"npm"`
	GateKeys      string            `json:"release_keys_gate"`
}

type binRecord struct {
	Name     string `json:"name"`
	Platform string `json:"platform"`
	SHA256   string `json:"sha256"`
}

type migrationsRecord struct {
	Source string `json:"source"`
	manifest.MigrationsNotice
	Count int `json:"count"`
}

func cmdBuild(args []string, stdout, stderr io.Writer) error {
	fl := flag.NewFlagSet("build", flag.ContinueOnError)
	fl.SetOutput(stderr)
	o := buildOpts{openrestyTrees: treeFlags{}, frontendDirs: treeFlags{}, stdout: stdout}
	plats := fl.String("platforms", "linux-amd64,linux-arm64", "目标平台（逗号分隔）")
	fl.StringVar(&o.src, "src", ".", "源仓库根目录")
	fl.StringVar(&o.out, "out", "", "输出目录（必填，须不存在或为空）")
	fl.StringVar(&o.version, "version", "", "发行版本（semver，必填）")
	fl.BoolVar(&o.repro, "repro", true, "同源第二次构建并比对哈希（可复现性验证）")
	fl.BoolVar(&o.fresh, "repro-fresh-cache", true, "第二次构建使用干净 GOCACHE 与另一源目录（更强的复现证明）")
	fl.BoolVar(&o.allowDirty, "allow-dirty", false, "允许工作树有未提交改动（仅限演练；发行必须干净）")
	fl.StringVar(&o.minUpgradeFrom, "min-upgrade-from", "", "min_upgrade_from（缺省 = 本版本）")
	fl.StringVar(&o.openrestyVersion, "openresty-version", "", "提供 OpenResty 发行目录时的版本")
	fl.Var(o.openrestyTrees, "openresty-tree", "OpenResty 发行目录 <platform>=<dir>（可重复）；其中的符号链接在构建期消除")
	fl.Var(frontendFlag{o.frontendDirs}, "frontend-dist", "静态前端生产构建目录 <组件>=<目录>（可重复；组件为 admin-web|console|docs|status|website）；目录须含 index.html 并通过 mock 入口检查，包内布局为 payload/public/**（含预压缩 .gz）")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if o.out == "" || o.version == "" {
		return usageError{"build 需要 -out 与 -version"}
	}
	v, err := manifest.ParseVersion(o.version)
	if err != nil {
		return fmt.Errorf("-version: %w", err)
	}
	o.version = v.String()
	if o.minUpgradeFrom == "" {
		o.minUpgradeFrom = o.version
	}
	for _, p := range strings.Split(*plats, ",") {
		if p == "any" || !rePlatformFlag.MatchString(p) {
			return fmt.Errorf("-platforms 含非法平台 %q（linux-amd64|linux-arm64）", p)
		}
		o.platforms = append(o.platforms, p)
	}
	if len(o.openrestyTrees) > 0 && o.openrestyVersion == "" {
		return errors.New("提供 -openresty-tree 必须同时给 -openresty-version")
	}
	var err2 error
	if o.src, err2 = filepath.Abs(o.src); err2 != nil {
		return err2
	}
	if o.out, err2 = filepath.Abs(o.out); err2 != nil {
		return err2
	}
	return o.build()
}

func (o *buildOpts) logf(f string, a ...any) { _, _ = fmt.Fprintf(o.stdout, f, a...) }

func (o *buildOpts) build() error {
	// 1. 源码状态：发行必须可追溯到一个提交；工作树必须干净（演练除外）。
	commit, err := gitRun(o.src, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return fmt.Errorf("源目录不是带提交的 git 仓库，发行必须可追溯到提交: %w", err)
	}
	dirtyOut, err := gitRun(o.src, "status", "--porcelain")
	if err != nil {
		return err
	}
	dirty := dirtyOut != ""
	if dirty && !o.allowDirty {
		return fmt.Errorf("工作树有未提交改动，发行必须来自干净提交（演练可加 -allow-dirty）:\n%s", dirtyOut)
	}
	ct, err := gitRun(o.src, "show", "-s", "--format=%ct", "HEAD")
	if err != nil {
		return err
	}
	sec, err := strconv.ParseInt(ct, 10, 64)
	if err != nil {
		return fmt.Errorf("提交时间非法: %q", ct)
	}
	commitTime := time.Unix(sec, 0).UTC()
	if err := requireEmptyOut(o.out); err != nil {
		return err
	}

	// 2. 内置公钥守卫（NFR-1、Story 1.27 AC5）：发行构建缺失/含测试公钥 → 失败。与 make release 的前置检查同一门禁，此处再检一次（纵深）。
	if out, err := goRun(o.src, nil, "run", "./scripts/gates", gateArgsRequired, "-require", "-root", o.src); err != nil {
		return fmt.Errorf("内置发行公钥守卫失败（发行构建必须注入真实发行公钥 keys.json，且不得含测试公钥）:\n%v\n%s", err, out)
	}

	// 3. 选定组件与二进制。
	var defs []compDef
	for _, c := range catalog {
		if c.Name == "openresty" && len(o.openrestyTrees) > 0 {
			c.Status = manifest.CompAvailable
			c.Description = "OpenResty 发行组件"
			c.Requires = &manifest.Requires{System: &manifest.SystemReq{OS: []string{"linux"}}}
		}
		if _, ok := o.frontendDirs[c.Name]; ok {
			c.Status = manifest.CompAvailable
			c.Description = "LingEdge 静态前端（" + c.Name + "；平台 any，解压到 releases/<v>/public/ 由 Nginx 托管）"
			c.Variant = frontendComponents[c.Name]
			c.Requires = &manifest.Requires{System: &manifest.SystemReq{OS: []string{"linux"}}}
		}
		defs = append(defs, c)
	}
	var jobs []buildJob
	seen := map[string]bool{}
	for _, c := range defs {
		if c.Status != manifest.CompAvailable {
			continue
		}
		for _, plat := range o.platforms {
			for _, b := range c.Binaries {
				if !seen[b+"/"+plat] {
					seen[b+"/"+plat] = true
					jobs = append(jobs, buildJob{cmd: b, plat: plat})
				}
			}
		}
	}
	// 静态前端：目录必须是真实的生产构建（含 index.html、通过 mock 入口检查），且可追溯到 pnpm-lock.yaml。
	var lockSHA string
	if len(o.frontendDirs) > 0 {
		if lockSHA, err = pnpmLockSHA256(o.src); err != nil {
			return err
		}
		names := make([]string, 0, len(o.frontendDirs))
		for n := range o.frontendDirs {
			names = append(names, n)
		}
		sort.Strings(names)
		// 先快照、再校验快照、之后只用快照打包（根因与理由见 snapshotFrontend）。
		for _, n := range names {
			shown := o.frontendDirs[n]
			snap, err := snapshotFrontend(o.out, n, shown)
			if err != nil {
				return err
			}
			if err := checkFrontendDist(o.src, n, shown, snap); err != nil {
				return err
			}
			o.frontendDirs[n] = snap
		}
	}
	o.lockSHA = lockSHA
	ldflags := fmt.Sprintf("-s -w -X %s/pkg/version.Version=%s -X %s/pkg/version.Commit=%s", modulePath, o.version, modulePath, commit)

	// 4. 构建（A），无 lingedge_testdouble 标签（从不传 -tags）。
	buildA := filepath.Join(o.out, "work", "build-a")
	o.logf("构建 %d 个二进制（-trimpath -buildvcs=false，无测试替身标签）…\n", len(jobs))
	if err := goBuildJobs(o.src, buildA, jobs, ldflags, ""); err != nil {
		return err
	}
	for _, j := range jobs {
		if err := scanBinaryForTestKeys(o.src, filepath.Join(buildA, j.plat, j.cmd)); err != nil {
			return err
		}
	}

	rec := &buildRecord{Release: o.version, Commit: commit, CommitTime: commitTime.Format(time.RFC3339), Dirty: dirty,
		BuildFlags: []string{"-trimpath", "-buildvcs=false", "CGO_ENABLED=0", "GOFLAGS="}, LDFlags: ldflags, Tags: "(none)", Platforms: o.platforms,
		NPM: "本次发行不含前端包（未提供 -frontend-dist）", GateKeys: "gates release-keys -require 通过"}
	gv, err := goRun(o.src, buildEnv(), "env", "GOVERSION")
	if err != nil {
		return err
	}
	rec.GoVersion = strings.TrimSpace(string(gv))
	if lockSHA != "" {
		rec.NPM = pnpmLockRel + " sha256=" + lockSHA + "（pnpm install --frozen-lockfile；前端包的构建输出由 -frontend-dist 提供并经 mock 入口检查）"
	}

	// 5. 可复现性：同源第二次构建，逐文件比对。
	hashA := map[string]string{}
	for _, j := range jobs {
		h, _, err := sha256File(filepath.Join(buildA, j.plat, j.cmd))
		if err != nil {
			return err
		}
		hashA[j.plat+"/"+j.cmd] = h
		rec.Binaries = append(rec.Binaries, binRecord{Name: j.cmd, Platform: j.plat, SHA256: h})
	}
	if o.repro {
		if err := o.reproCheck(jobs, hashA, ldflags, rec); err != nil {
			return err
		}
	}

	// 6. 迁移预告：取自 lingedge-server migrate plan（离线模式，列出全部内嵌迁移）。
	var mig *manifest.MigrationsNotice
	for _, c := range defs {
		if c.Status == manifest.CompAvailable && c.Migrations {
			m, count, err := o.migrationNotice(ldflags)
			if err != nil {
				return err
			}
			mig = m
			rec.Migrations = &migrationsRecord{Source: "lingedge-server migrate plan --offline", MigrationsNotice: *m, Count: count}
		}
	}

	// 7. 组装各组件包的 stage 树，生成第 1 轮待签清单。
	plan := &releasePlan{Schema: planSchema, Release: o.version, Commit: commit, BuiltAt: commitTime.Format(time.RFC3339)}
	boms := map[string]*bomDoc{}
	ts := toSign{Schema: toSignSchema, Release: o.version, Commit: commit, Round: 1}
	for _, c := range defs {
		pc := planComponent{Name: c.Name, Status: c.Status, Description: c.Description, Internal: c.Internal, BuiltinIn: c.BuiltinIn}
		if c.Status == manifest.CompAvailable {
			entry := &manifest.VersionEntry{Version: o.version, ReleasedAt: commitTime.Format(time.RFC3339), MinUpgradeFrom: o.minUpgradeFrom,
				Requires: c.Requires, Ports: c.Ports, ConfigSchemaVersion: c.ConfigSchemaVersion,
				Variants: map[string]manifest.Variant{c.variant(): {Artifacts: map[string]manifest.Artifact{}}}}
			if c.Migrations {
				entry.Migrations = mig
			}
			pc.Version = entry
			plats := o.platforms
			if c.Name == "openresty" {
				plats = nil
				for p := range o.openrestyTrees {
					plats = append(plats, p)
				}
				sort.Strings(plats)
			}
			if _, ok := o.frontendDirs[c.Name]; ok {
				plats = []string{"any"} // 静态资源与 CPU 架构无关
			}
			for _, plat := range plats {
				pkg, bom, links, err := o.stagePackage(c, plat, buildA, commitTime, rec)
				if err != nil {
					return fmt.Errorf("组装 %s %s 失败: %w", c.Name, plat, err)
				}
				rec.LinksResolved = append(rec.LinksResolved, links...)
				plan.Packages = append(plan.Packages, *pkg)
				boms[pkg.ID] = bom
				sums := filepath.Join(pkg.Stage, "META", "SHA256SUMS")
				b, err := os.ReadFile(filepath.Join(o.out, filepath.FromSlash(sums)))
				if err != nil {
					return err
				}
				ts.Items = append(ts.Items, toSignItem{ID: pkg.ID, Kind: kindSums, File: filepath.ToSlash(sums), SHA256: sha256Hex(b),
					Sig: "signatures/" + pkg.ID + ".sig", Component: pkg.Component, Version: pkg.Version, Platform: pkg.Platform,
					FilesCount: pkg.FilesCount, UnpackedSize: pkg.UnpackedSize})
			}
		}
		plan.Components = append(plan.Components, pc)
	}
	if len(ts.Items) == 0 {
		return errors.New("没有任何可发行的包")
	}
	sort.Slice(ts.Items, func(i, j int) bool { return ts.Items[i].ID < ts.Items[j].ID })
	sort.Slice(plan.Packages, func(i, j int) bool { return plan.Packages[i].ID < plan.Packages[j].ID })
	if _, err := writeJSON(filepath.Join(o.out, "plan.json"), plan); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(o.out, "signatures"), 0o755); err != nil {
		return err
	}
	tsBytes, err := writeJSON(filepath.Join(o.out, "to-sign-1.json"), ts)
	if err != nil {
		return err
	}

	// 8. 汇总 SBOM 与构建记录。
	agg, err := mergeBOMs(commitTime.Format(time.RFC3339), "lingedge", o.version, boms).finish()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(o.out, "SBOM.cdx.json"), agg, 0o644); err != nil {
		return err
	}
	if _, err := writeJSON(filepath.Join(o.out, "build-record.json"), rec); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(o.out, "work")); err != nil {
		return err
	}
	o.logf("\n第 1 轮待签清单已生成：%s\n  to-sign-1.json sha256=%s\n  把 %s 与其引用的 stage/ 带到离线机，运行：\n    release-sign sign -key <私钥> -to-sign %s/to-sign-1.json -expect-digest %s\n  然后把 signatures/*.sig 带回，执行 make release-finalize。\n",
		o.out, sha256Hex(tsBytes), o.out, o.out, sha256Hex(tsBytes))
	return nil
}

type buildJob struct{ cmd, plat string }

// buildEnv 保持 GOTOOLCHAIN 的默认（auto）：toolchain 由 go.mod 的 toolchain 行固定，两次构建用同一个。
func buildEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "GOFLAGS", "CGO_ENABLED", "GOOS", "GOARCH", "GOCACHE":
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GOFLAGS=", "CGO_ENABLED=0")
}

func goBuildJobs(src, outDir string, jobs []buildJob, ldflags, gocache string) error {
	for _, j := range jobs {
		env := append(buildEnv(), "GOOS="+strings.SplitN(j.plat, "-", 2)[0], "GOARCH="+strings.SplitN(j.plat, "-", 2)[1])
		if gocache != "" {
			env = append(env, "GOCACHE="+gocache)
		}
		dst := filepath.Join(outDir, j.plat, j.cmd)
		if _, err := goRun(src, env, "build", "-trimpath", "-buildvcs=false", "-ldflags", ldflags, "-o", dst, "./cmd/"+j.cmd); err != nil {
			return fmt.Errorf("构建 %s %s 失败: %w", j.cmd, j.plat, err)
		}
	}
	return nil
}

// reproCheck 把源树（git ls-files + 注入的内置公钥）复制到另一目录，用干净 GOCACHE 再构建一遍并逐文件比对 SHA-256。
func (o *buildOpts) reproCheck(jobs []buildJob, hashA map[string]string, ldflags string, rec *buildRecord) error {
	work := filepath.Join(o.out, "work")
	// 第二次构建总是在另一个目录里进行（-trimpath 必须使产物与源目录路径无关）；-repro-fresh-cache 再叠加干净 GOCACHE（不复用任何编译缓存）。
	srcB := filepath.Join(work, "src-b")
	cache := ""
	if o.fresh {
		cache = filepath.Join(work, "gocache-b")
	}
	o.logf("可复现性：复制源树到另一目录%s，重新构建…\n", map[bool]string{true: "并使用干净 GOCACHE", false: ""}[o.fresh])
	if err := copyTracked(o.src, srcB); err != nil {
		return err
	}
	buildB := filepath.Join(work, "build-b")
	if err := goBuildJobs(srcB, buildB, jobs, ldflags, cache); err != nil {
		return err
	}
	hashB := map[string]string{}
	for _, j := range jobs {
		hb, _, err := sha256File(filepath.Join(buildB, j.plat, j.cmd))
		if err != nil {
			return err
		}
		hashB[j.plat+"/"+j.cmd] = hb
	}
	rows, bad := compareBuilds(jobs, hashA, hashB)
	doc := map[string]any{"release": o.version, "commit": rec.Commit, "go_version": rec.GoVersion, "flags": rec.BuildFlags, "ldflags": rec.LDFlags,
		"second_build_source_dir_differs": true, "second_build_fresh_gocache": o.fresh, "all_equal": bad == 0, "binaries": rows}
	if _, err := writeJSON(filepath.Join(o.out, "repro.json"), doc); err != nil {
		return err
	}
	if bad > 0 {
		return fmt.Errorf("可复现性验证失败：%d 个二进制两次构建哈希不一致（见 %s/repro.json）", bad, o.out)
	}
	o.logf("可复现性：%d 个二进制两次构建哈希完全一致（repro.json）。\n", len(rows))
	return nil
}

// copyTracked 复制 git 已跟踪文件（以工作树当前内容为准）与注入的内置公钥到 dst。
func copyTracked(src, dst string) error {
	out, err := execCmd(src, nil, "git", "ls-files", "-z")
	if err != nil {
		return err
	}
	names := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
	if _, err := os.Stat(filepath.Join(src, filepath.FromSlash(builtinKeysRel))); err == nil {
		names = append(names, builtinKeysRel)
	}
	for _, n := range names {
		p := filepath.Join(src, filepath.FromSlash(n))
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		to := filepath.Join(dst, filepath.FromSlash(n))
		if fi.Mode()&os.ModeSymlink != 0 {
			tgt, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(tgt, to); err != nil {
				return err
			}
			continue
		}
		if !fi.Mode().IsRegular() {
			continue
		}
		if err := copyFile(p, to, fi.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

// migrationNotice 构建宿主平台的 lingedge-server 并取其迁移计划（tools-design §9.2：预告取自 migrate plan 的实际结果）。
func (o *buildOpts) migrationNotice(ldflags string) (*manifest.MigrationsNotice, int, error) {
	hostDir := filepath.Join(o.out, "work", "host")
	bin := filepath.Join(hostDir, "lingedge-server")
	env := buildEnv()
	if _, err := goRun(o.src, env, "build", "-trimpath", "-buildvcs=false", "-ldflags", ldflags, "-o", bin, "./cmd/lingedge-server"); err != nil {
		return nil, 0, fmt.Errorf("构建宿主平台 lingedge-server 失败: %w", err)
	}
	out, err := execCmd(o.src, nil, bin, "migrate", "plan", "--offline")
	if err != nil {
		return nil, 0, fmt.Errorf("lingedge-server migrate plan 失败: %w", err)
	}
	return parseMigratePlan(out)
}

// parseMigratePlan 把 `migrate plan` 的 JSON 转成清单的迁移预告。
func parseMigratePlan(b []byte) (*manifest.MigrationsNotice, int, error) {
	var p struct {
		Mode            string `json:"mode"`
		LatestVersion   int64  `json:"latest_version"`
		Count           int    `json:"count"`
		HasIrreversible bool   `json:"has_irreversible"`
		Migrations      []struct {
			Version int64  `json:"version"`
			Class   string `json:"class"`
		} `json:"migrations"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, 0, fmt.Errorf("migrate plan 输出不是 JSON: %w", err)
	}
	if p.Mode != "all" || p.Count != len(p.Migrations) || p.Count == 0 {
		return nil, 0, fmt.Errorf("migrate plan（离线）输出不合预期: mode=%q count=%d", p.Mode, p.Count)
	}
	classes := map[string]bool{}
	irrev := 0
	for _, m := range p.Migrations {
		switch m.Class {
		case "expand", "reversible", "irreversible":
		default:
			return nil, 0, fmt.Errorf("迁移 %d 的分类 %q 未知", m.Version, m.Class)
		}
		classes[m.Class] = true
		if m.Class == "irreversible" {
			irrev++
		}
	}
	if (irrev > 0) != p.HasIrreversible {
		return nil, 0, errors.New("migrate plan 的 has_irreversible 与迁移分类矛盾")
	}
	var cl []string
	for c := range classes {
		cl = append(cl, c)
	}
	sort.Strings(cl)
	// backup_required [ASSUMPTION]：含任何改动既有结构的迁移（reversible/irreversible）即要求备份；纯 expand 不要求。以 migrate plan 实际结果为准（取更严格者）。
	backup := classes["reversible"] || classes["irreversible"]
	return &manifest.MigrationsNotice{SchemaFrom: "0000", SchemaTo: fmt.Sprintf("%04d", p.LatestVersion), Classes: cl, IrreversibleCount: irrev, BackupRequired: backup}, p.Count, nil
}

type reproRow struct {
	Name     string `json:"name"`
	Platform string `json:"platform"`
	A        string `json:"sha256_build_a"`
	B        string `json:"sha256_build_b"`
	Equal    bool   `json:"equal"`
}

// compareBuilds 逐个二进制比对两次构建的哈希；缺失（一侧没有）视为不一致。
func compareBuilds(jobs []buildJob, a, b map[string]string) (rows []reproRow, bad int) {
	for _, j := range jobs {
		k := j.plat + "/" + j.cmd
		ha, hb := a[k], b[k]
		eq := ha != "" && ha == hb
		rows = append(rows, reproRow{Name: j.cmd, Platform: j.plat, A: ha, B: hb, Equal: eq})
		if !eq {
			bad++
		}
	}
	return rows, bad
}

// frontendFlag 实现 flag.Value：-frontend-dist <组件>=<目录>。
type frontendFlag struct{ m treeFlags }

func (f frontendFlag) String() string { return "" }

func (f frontendFlag) Set(v string) error {
	comp, dir, ok := strings.Cut(v, "=")
	if _, known := frontendComponents[comp]; !ok || !known || dir == "" {
		return fmt.Errorf("格式应为 <组件>=<目录>，组件为 admin-web|console|docs|status|website，收到 %q", v)
	}
	if _, dup := f.m[comp]; dup {
		return fmt.Errorf("-frontend-dist %s 重复", comp)
	}
	f.m[comp] = dir
	return nil
}
