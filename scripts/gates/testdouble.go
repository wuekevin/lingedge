package main

import (
	"bytes"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

//go:embed data/*.txt
var dataFS embed.FS

func loadLines(name string) []string {
	b, err := dataFS.ReadFile("data/" + name)
	if err != nil {
		panic(err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

const testdoubleTag = "lingedge_testdouble"

var (
	reBuildLine = regexp.MustCompile(`(?m)^//go:build\s+(.+)$`)
	reEmbedFix  = regexp.MustCompile(`(?m)^\s*//go:embed\s+.*(?:fixtures|testdata)`)
	reTesting   = regexp.MustCompile(`\btesting\.Testing\(`)
)

// requiresTestdoubleTag 判断文件是否「仅在带 lingedge_testdouble 标签时才参与构建」：
// 约束在标签为真时成立、为假时不成立。`!lingedge_testdouble`、`lingedge_testdouble || linux` 等形态不算。
func requiresTestdoubleTag(content []byte) bool {
	m := reBuildLine.FindSubmatch(content)
	if m == nil {
		return false
	}
	expr, err := constraint.Parse("//go:build " + strings.TrimSpace(string(m[1])))
	if err != nil {
		return false
	}
	eval := func(tagOn bool) bool {
		// 其余标签（平台等）一律视为满足，只考察 lingedge_testdouble 是否决定结果。
		return expr.Eval(func(tag string) bool { return tag != testdoubleTag || tagOn })
	}
	return eval(true) && !eval(false)
}

// isTestPath：测试/夹具位置（任意深度的 tests、__tests__、testdata 目录，以及 *_test.go、*.test.ts、*.spec.* 文件）。
func isTestPath(rel string) bool {
	segs := strings.Split(rel, "/")
	for _, s := range segs[:len(segs)-1] {
		if s == "tests" || s == "__tests__" || s == "testdata" {
			return true
		}
	}
	base := segs[len(segs)-1]
	return isTestFile(rel) || strings.Contains(base, ".test.") || strings.Contains(base, ".spec.")
}

// 对外发行目标矩阵（与 Makefile 的 build 目标一致）：依赖图须对每个目标检查，防止测试替身藏在 GOOS/GOARCH 约束之后。
var buildTargets = []struct{ goos, goarch string }{{"linux", "amd64"}, {"linux", "arm64"}, {"windows", "amd64"}}

// runTestdouble 实现 G-11（NFR-1、ADR-023 §A）：生产构建不得含测试替身与固定假数据。
// 源码层：替身位置/构建标签/生产代码引用；构建层：go list -deps、产物符号与字符串扫描。
func runTestdouble(root string, opts options) ([]Finding, error) {
	markers := append(loadLines("fake-data-markers.txt"), testPubKeyMarkers()...)
	var out []Finding
	fset := token.NewFileSet()

	err := walkText(root, nil, func(f textFile) {
		rel := f.Rel
		segs := strings.Split(rel, "/")
		dirSegs := segs[:len(segs)-1]
		inTestdouble := contains(dirSegs, "testdouble")
		inTests := isTestPath(rel)
		isGo := strings.HasSuffix(rel, ".go")
		tagged := isGo && requiresTestdoubleTag(f.Content)

		if isGo {
			if tagged && !isTestFile(rel) && !inTests && (!inTestdouble || segs[0] != "internal") {
				out = append(out, Finding{File: rel, Rule: "testdouble-tag-location",
					Msg: "带 " + testdoubleTag + " 标签的文件只允许位于 _test.go、tests/ 或 internal/**/testdouble/（ADR-023 §A.1）"})
			}
			if inTestdouble && segs[0] != "internal" && !inTests {
				out = append(out, Finding{File: rel, Rule: "testdouble-location",
					Msg: "testdouble 包只允许位于 internal/**/testdouble/（ADR-023 §A.1）"})
			}
			if inTestdouble && !tagged && !isTestFile(rel) && !inTests {
				out = append(out, Finding{File: rel, Rule: "testdouble-missing-tag",
					Msg: "testdouble 包内的文件必须带 //go:build " + testdoubleTag + "（否则会进入生产构建，ADR-023 §A.1）"})
			}
		}

		production := !inTests && !inTestdouble && segs[0] != "docs" && !strings.HasSuffix(rel, ".md")
		if !production {
			return
		}

		if isGo {
			if pf, err := parser.ParseFile(fset, rel, f.Content, parser.ImportsOnly); err == nil {
				for _, imp := range pf.Imports {
					p, _ := strconv.Unquote(imp.Path.Value)
					if strings.Contains(strings.ToLower(p), "testdouble") {
						out = append(out, Finding{File: rel, Line: fset.Position(imp.Pos()).Line, Rule: "testdouble-import",
							Msg: "生产代码不得 import 测试替身包 " + p + "（G-11、NFR-1）"})
					}
				}
			} else {
				out = append(out, Finding{File: rel, Rule: "testdouble-unparseable", Msg: "无法解析 Go 文件，门禁无法检查其导入: " + err.Error()})
			}
			if loc := reEmbedFix.FindIndex(f.Content); loc != nil {
				out = append(out, Finding{File: rel, Line: lineOf(f.Content, loc[0]), Rule: "testdouble-fixture-embed",
					Msg: "生产代码不得 embed fixtures（ADR-023 §A.3）"})
			}
			if loc := reTesting.FindIndex(f.Content); loc != nil {
				out = append(out, Finding{File: rel, Line: lineOf(f.Content, loc[0]), Rule: "testdouble-test-branch",
					Msg: "生产代码不得出现「检测到测试环境」分支 testing.Testing()（ADR-023 §A.6）"})
			}
		}
		lower := bytes.ToLower(f.Content)
		for _, m := range markers {
			if i := bytes.Index(lower, []byte(strings.ToLower(m))); i >= 0 {
				out = append(out, Finding{File: rel, Line: lineOf(f.Content, i), Rule: "testdouble-fake-data",
					Msg: fmt.Sprintf("生产文件含固定假数据标记 %q（G-11、NFR-1）", m)})
			}
		}
	})
	if err != nil {
		return nil, err
	}

	if opts.skipBuild {
		return out, nil
	}
	bf, err := scanBuiltBinaries(root, markers)
	if err != nil {
		return nil, err
	}
	return append(out, bf...), nil
}

func goCmd(root string, args ...string) *exec.Cmd {
	c := exec.Command("go", args...)
	c.Dir = root
	c.Env = append(os.Environ(), "GOFLAGS=", "CGO_ENABLED=0", "GOWORK=off")
	return c
}

// scanBuiltBinaries：对每个 cmd/* 不带任何 -tags 做反向检查——依赖图（go list -deps）、
// 未剥离产物的符号表（go tool nm）、嵌入的构建设置（go version -m）与字符串中的假数据标记。
func scanBuiltBinaries(root string, markers []string) ([]Finding, error) {
	var out []Finding
	entries, err := os.ReadDir(filepath.Join(root, "cmd"))
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "lingedge-testdouble-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	var cmds []string
	for _, e := range entries {
		if e.IsDir() {
			cmds = append(cmds, e.Name())
		}
	}
	sort.Strings(cmds)
	for _, c := range cmds {
		pkg := "./cmd/" + c
		rel := "cmd/" + c
		for _, tg := range buildTargets {
			if tg.goos == "windows" && c != "lingedge-reporter" {
				continue // 与 Makefile 一致：windows 仅 reporter
			}
			cmd := goCmd(root, "list", "-deps", "-f", "{{.ImportPath}}", pkg)
			cmd.Env = append(cmd.Env, "GOOS="+tg.goos, "GOARCH="+tg.goarch)
			deps, err := cmd.CombinedOutput()
			if err != nil {
				out = append(out, Finding{File: rel, Rule: "testdouble-deps", Msg: fmt.Sprintf("go list -deps 失败（%s/%s，生产构建依赖图无法解析）: %s", tg.goos, tg.goarch, strings.TrimSpace(string(deps)))})
				continue
			}
			for _, p := range strings.Fields(string(deps)) {
				if strings.Contains(strings.ToLower(p), "testdouble") {
					out = append(out, Finding{File: rel, Rule: "testdouble-deps", Msg: fmt.Sprintf("生产依赖图（%s/%s）含测试替身包 %s（G-11、NFR-1）", tg.goos, tg.goarch, p)})
				}
			}
		}

		bin := filepath.Join(tmp, c)
		if b, err := goCmd(root, "build", "-trimpath", "-buildvcs=false", "-o", bin, pkg).CombinedOutput(); err != nil {
			out = append(out, Finding{File: rel, Rule: "testdouble-build", Msg: "生产构建失败: " + strings.TrimSpace(string(b))})
			continue
		}
		info, err := goCmd(root, "version", "-m", bin).CombinedOutput()
		if err != nil {
			out = append(out, Finding{File: rel, Rule: "testdouble-artifact", Msg: "go version -m 失败，无法检查产物构建设置: " + strings.TrimSpace(string(info))})
		} else if bytes.Contains(info, []byte(testdoubleTag)) {
			out = append(out, Finding{File: rel, Rule: "testdouble-artifact", Msg: "产物构建设置含 " + testdoubleTag + " 标签（G-11）"})
		}
		syms, err := goCmd(root, "tool", "nm", bin).Output()
		if err != nil {
			out = append(out, Finding{File: rel, Rule: "testdouble-artifact", Msg: "go tool nm 失败，无法检查产物符号表: " + err.Error()})
		}
		for _, l := range strings.Split(string(syms), "\n") {
			if strings.Contains(strings.ToLower(l), "testdouble") {
				out = append(out, Finding{File: rel, Rule: "testdouble-artifact", Msg: "产物符号表含 testdouble：" + strings.TrimSpace(l)})
				break
			}
		}
		raw, err := os.ReadFile(bin)
		if err != nil {
			return nil, err
		}
		lower := bytes.ToLower(raw)
		for _, m := range append([]string{"testdouble"}, markers...) {
			if bytes.Contains(lower, []byte(strings.ToLower(m))) {
				out = append(out, Finding{File: rel, Rule: "testdouble-artifact", Msg: fmt.Sprintf("产物字符串含假数据/替身标记 %q（G-11、NFR-1）", m)})
			}
		}
	}
	return out, nil
}

// testPubKeyMarkers：测试发行公钥（data/test-release-pubkeys.txt，Story 1.27）的 base64 与 hex 表示也是固定假数据——
// 其私钥种子公开在测试里，生产源码、配置与生产产物里不得出现（NFR-1）。门禁自身目录与测试/文档位置不在扫描范围内。
func testPubKeyMarkers() []string {
	var out []string
	for raw := range testReleasePubKeys() {
		out = append(out, base64.StdEncoding.EncodeToString([]byte(raw)), hex.EncodeToString([]byte(raw)))
	}
	sort.Strings(out)
	return out
}
