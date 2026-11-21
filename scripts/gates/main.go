// Command gates 实现 Story 1.2 的静态质量门禁（依赖方向配置生成、结构检查、测试替身隔离、固定口令、品牌、命令注入）。
//
// 用法：go run ./scripts/gates <子命令> [-root 仓库根目录]
//
// 子命令：gen-lint（生成/校验 .golangci.yml）、arch、testdouble、passwords、brand、shell、repo（Repository 的 Scope 首参，Story 1.5）、tasktype（任务类型白名单，Story 1.14）、perms（权限注册表 ⇄ 种子 ⇄ OpenAPI x-permissions 一致性，Story 1.18）、audit（写 operation 必须声明 x-audit、声明的审计动作必须有记录者，Story 1.21）、trustdomain（Server 代码路径不得存在加载发行清单私钥的逻辑，Story 1.23）、flags（旧 CLI 标志名 0 命中，Story 1.25 AC7）、all（除 gen-lint 写入外全部运行）。
// 任一发现即以退出码 1 结束；参数或运行环境错误以退出码 2 结束。仅依赖标准库（perms 门禁另引用同模块的 internal/platform/rbac 权限注册表）。
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Finding 是一条门禁发现。
type Finding struct {
	File string
	Line int
	Rule string
	Msg  string
}

func (f Finding) String() string {
	if f.Line > 0 {
		return fmt.Sprintf("%s:%d: [%s] %s", f.File, f.Line, f.Rule, f.Msg)
	}
	return fmt.Sprintf("%s: [%s] %s", f.File, f.Rule, f.Msg)
}

// 所有门禁共同跳过的目录名（缓存、第三方工具渲染物；任意深度）。
var skipDirNames = map[string]bool{
	".git": true, "_bmad": true, "_bmad-output": true,
	"node_modules": true, ".pnpm-store": true, ".nuxt": true, ".output": true,
}

// 构建产物目录只在仓库根与前端/站点工程根下跳过；其他位置（如 deploy/install/bin）是源码，必须受检。
func isBuildOutputDir(rel string) bool {
	switch rel {
	case "dist", "bin":
		return true
	}
	for _, p := range []string{"frontend/", "docs-site/", "status-site/"} {
		if strings.HasPrefix(rel, p) && (strings.HasSuffix(rel, "/dist") || strings.HasSuffix(rel, "/bin")) {
			return true
		}
	}
	return false
}

// 超过该大小的文件无法逐行扫描，作为发现报告而不是静默跳过（避免 fail-open）。
const maxScanBytes = 32 << 20

// gateSelfDir 是门禁自身（规则定义、词表）所在目录；扫描类门禁不检查它，否则规则文本会命中自身。
const gateSelfDir = "scripts/gates"

type textFile struct {
	Rel     string // 以 / 分隔的相对路径
	Content []byte
}

// walkText 遍历 root 下的文本文件（跳过二进制与超大文件）。skip 可按相对路径进一步跳过目录或文件。
func walkText(root string, skip func(rel string, isDir bool) bool, fn func(f textFile)) error {
	var oversize []Finding
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if skipDirNames[d.Name()] || isBuildOutputDir(rel) || rel == gateSelfDir || (skip != nil && skip(rel, true)) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || (skip != nil && skip(rel, false)) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > maxScanBytes {
			oversize = append(oversize, Finding{File: rel, Rule: "gate-skipped", Msg: "文件超过 32MiB 无法扫描，须拆分或移出仓库（不得静默跳过门禁）"})
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if bytes.IndexByte(b, 0) >= 0 { // 二进制
			return nil
		}
		fn(textFile{Rel: rel, Content: b})
		return nil
	})
	if err == nil && len(oversize) > 0 {
		err = &oversizeError{oversize}
	}
	return err
}

// oversizeError 把「无法扫描的超大文件」作为错误向上传递，使门禁以运行错误（退出码 2）失败而不是放行。
type oversizeError struct{ fs []Finding }

func (e *oversizeError) Error() string {
	var parts []string
	for _, f := range e.fs {
		parts = append(parts, f.String())
	}
	return strings.Join(parts, "; ")
}

func isTestFile(rel string) bool { return strings.HasSuffix(rel, "_test.go") }

func lineOf(content []byte, offset int) int {
	return bytes.Count(content[:offset], []byte{'\n'}) + 1
}

func sortFindings(list []Finding) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].File != list[j].File {
			return list[i].File < list[j].File
		}
		if list[i].Line != list[j].Line {
			return list[i].Line < list[j].Line
		}
		return list[i].Rule < list[j].Rule
	})
}

type gate struct {
	name string
	run  func(root string, opts options) ([]Finding, error)
}

type options struct {
	skipBuild bool // testdouble：跳过 go list / 产物扫描（仅做源码层检查，供快速自测）
	check     bool // gen-lint：仅校验不写入
}

var gates = []gate{
	{"arch", runArch},
	{"testdouble", runTestdouble},
	{"passwords", runPasswords},
	{"brand", runBrand},
	{"shell", runShell},
	{"repo", runRepo},
	{"tasktype", runTasktype},
	{"perms", runPerms},
	{"audit", runAudit},
	{"trustdomain", runTrustDomain},
	{"flags", runFlags},
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法: gates <gen-lint|arch|testdouble|passwords|brand|shell|repo|tasktype|perms|audit|trustdomain|flags|all> [-root dir] [-skip-build] [-check]")
		return 2
	}
	cmd := args[0]
	fl := flag.NewFlagSet(cmd, flag.ContinueOnError)
	root := fl.String("root", ".", "仓库根目录")
	var opts options
	fl.BoolVar(&opts.skipBuild, "skip-build", false, "testdouble：跳过依赖图与产物扫描")
	fl.BoolVar(&opts.check, "check", false, "gen-lint：仅校验 .golangci.yml 是否与规格一致")
	if err := fl.Parse(args[1:]); err != nil {
		return 2
	}
	abs, err := filepath.Abs(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	if cmd == "gen-lint" {
		found, err := runGenLint(abs, opts)
		return report("gen-lint", found, err)
	}
	selected := gates
	if cmd != "all" {
		selected = nil
		for _, g := range gates {
			if g.name == cmd {
				selected = []gate{g}
			}
		}
		if selected == nil {
			fmt.Fprintf(os.Stderr, "未知子命令 %q\n", cmd)
			return 2
		}
	}
	code := 0
	for _, g := range selected {
		found, err := g.run(abs, opts)
		if c := report(g.name, found, err); c > code {
			code = c
		}
	}
	return code
}

func report(name string, found []Finding, err error) int {
	if err != nil {
		fmt.Fprintf(os.Stderr, "gates %s: 运行错误: %v\n", name, err)
		return 2
	}
	if len(found) == 0 {
		fmt.Printf("gates %s: 通过\n", name)
		return 0
	}
	sortFindings(found)
	for _, f := range found {
		fmt.Fprintln(os.Stderr, f)
	}
	fmt.Fprintf(os.Stderr, "gates %s: 失败（%d 处）\n", name, len(found))
	return 1
}
