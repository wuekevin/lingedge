package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// readLines 读取逐行清单（忽略空行与 # 注释）。
func readLines(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		out = append(out, l)
	}
	return out, nil
}

func printFindings(fs []finding) {
	for _, f := range fs {
		fmt.Fprintln(os.Stderr, "  "+f.String())
	}
}

func cmdOpenAPICheck(args []string) int {
	fl := flag.NewFlagSet("openapi-check", flag.ContinueOnError)
	file := fl.String("file", "docs/api/openapi.yaml", "OpenAPI 文件")
	reauth := fl.String("reauth", "scripts/contract/data/x-reauth.txt", "固定的 x-reauth operationId 清单")
	routes := fl.String("routes", "", "运行时路由清单 JSON（lingedge-server routes 的输出）；给出时校验路由集合 ⊆ openapi 与元数据一致，并输出「M1 未实现 operation 清单」")
	m1Exit := fl.Bool("require-m1-complete", false, "M1 阶段出口：M1 未实现 operation 清单必须为空，否则失败（NFR-2，不得以 501 假实现充数）")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	data, err := os.ReadFile(*file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取失败:", err)
		return 2
	}
	doc, err := loadDoc(data, *file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "openapi-check 失败:", err)
		return 1
	}
	pinned, err := readLines(*reauth)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取 x-reauth 固定清单失败:", err)
		return 2
	}
	fs := runAllChecks(doc, pinned)
	if len(fs) > 0 {
		fmt.Fprintf(os.Stderr, "openapi-check 失败: %d 处违规\n", len(fs))
		printFindings(fs)
		return 1
	}
	if *routes != "" {
		if code := reportRoutes(doc, *routes, *m1Exit); code != 0 {
			return code
		}
	} else if *m1Exit {
		fmt.Fprintln(os.Stderr, "-require-m1-complete 需要同时给出 -routes")
		return 2
	}
	st := statsOf(doc)
	fmt.Printf("openapi-check OK: %s（OpenAPI %s）路径 %d、operation %d、x-reauth %d；各阶段 %s\n",
		*file, doc.OpenAPI, st.Paths, st.Ops, len(reauthOps(doc)), st.PhaseSummary())
	return 0
}

// runAllChecks 汇总 AC1 与 AC8 的全部静态规则。
func runAllChecks(doc *openapi3T, pinnedReauth []string) []finding {
	var fs []finding
	fs = append(fs, checkMetadata(doc)...)
	fs = append(fs, checkNoColonPaths(doc)...)
	fs = append(fs, checkPathParamNames(doc)...)
	fs = append(fs, checkIdempotencyResponses(doc)...)
	fs = append(fs, checkTenantLeak(doc)...)
	fs = append(fs, checkReauthSet(doc, pinnedReauth)...)
	fs = append(fs, checkListPagination(doc)...)
	fs = append(fs, checkNoInternalID(doc)...)
	return fs
}

func cmdOpenAPIBreaking(args []string) int {
	fl := flag.NewFlagSet("openapi-breaking", flag.ContinueOnError)
	file := fl.String("file", "docs/api/openapi.yaml", "当前 OpenAPI 文件")
	readme := fl.String("readme", "docs/api/README.md", "当前 API README（错误码索引）")
	baseRef := fl.String("base-ref", "contract-v1", "基线 git ref（冻结标签）")
	baseFile := fl.String("base-file", "", "直接指定基线 OpenAPI 文件（覆盖 -base-ref，供自测/离线使用）")
	baseReadme := fl.String("base-readme-file", "", "直接指定基线 README 文件（与 -base-file 配套）")
	oasdiff := fl.String("oasdiff", "go run github.com/oasdiff/oasdiff@v1.38.0", "oasdiff 命令（空白分隔，不经 shell；版本固定见 Makefile）")
	severity := fl.String("severity-levels", "scripts/contract/data/oasdiff-severity-levels.txt", "oasdiff 严重级别覆盖文件")
	note := fl.String("note", "", "变更说明文件（放行破坏性变更；须通过 validateChangeNote）")
	if err := fl.Parse(args); err != nil {
		return 2
	}

	cur, err := os.ReadFile(*file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取失败:", err)
		return 2
	}
	var base, baseRM []byte
	var policyChanged []string
	baseLabel := *baseRef
	if *baseFile != "" {
		baseLabel = *baseFile
		if base, err = os.ReadFile(*baseFile); err != nil {
			fmt.Fprintln(os.Stderr, "读取基线失败:", err)
			return 2
		}
		if *baseReadme != "" {
			if baseRM, err = os.ReadFile(*baseReadme); err != nil {
				fmt.Fprintln(os.Stderr, "读取基线 README 失败:", err)
				return 2
			}
		}
	} else {
		var ok bool
		if base, ok, err = gitShow(*baseRef, *file); err != nil {
			fmt.Fprintln(os.Stderr, "openapi-breaking 失败:", err)
			return 1
		} else if !ok {
			fmt.Fprintf(os.Stderr, "openapi-breaking 失败: 基线 %s 中不存在 %s\n", *baseRef, *file)
			return 1
		}
		var rmOK bool
		if baseRM, rmOK, err = gitShow(*baseRef, *readme); err != nil || !rmOK {
			fmt.Fprintf(os.Stderr, "openapi-breaking 失败: 基线 %s 中不存在 %s，无法比较错误码索引\n", *baseRef, *readme)
			return 1
		}
		// 门禁策略文件相对基线不得改动：否则同一变更里改策略即可绕过检测。
		for _, pf := range []string{*severity, "proto/buf.yaml"} {
			if pf == "" {
				continue
			}
			baseData, ok, gerr := gitShow(*baseRef, filepath.ToSlash(pf))
			if gerr != nil || !ok {
				continue // 基线尚无该策略文件（例如 contract-v1 创建前的比对）
			}
			curData, rerr := os.ReadFile(pf)
			if rerr != nil || !bytes.Equal(baseData, curData) {
				policyChanged = append(policyChanged, pf)
			}
		}
	}
	if *baseFile != "" && baseRM == nil {
		fmt.Fprintln(os.Stderr, "openapi-breaking 失败: -base-file 模式须同时给出 -base-readme-file（错误码索引比较不可跳过）")
		return 2
	}

	baseDoc, err := loadDoc(base, "基线 "+baseLabel)
	if err != nil {
		fmt.Fprintln(os.Stderr, "openapi-breaking 失败:", err)
		return 1
	}
	curDoc, err := loadDoc(cur, *file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "openapi-breaking 失败:", err)
		return 1
	}

	tmp, err := os.MkdirTemp("", "contract-base-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer os.RemoveAll(tmp)
	basePath := tmp + "/base-openapi.yaml"
	if err := os.WriteFile(basePath, base, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	// oasdiff 的严重级别文件不允许注释：从带注释的数据文件剥离后写入临时文件。
	sevPath := ""
	if *severity != "" {
		lines, lerr := readLines(*severity)
		if lerr != nil {
			fmt.Fprintln(os.Stderr, "读取严重级别文件失败:", lerr)
			return 2
		}
		sevPath = tmp + "/severity-levels.txt"
		if err := os.WriteFile(sevPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}

	var changes []change
	oc, err := runOasdiff(strings.Fields(*oasdiff), basePath, *file, sevPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "openapi-breaking 失败:", err)
		return 2
	}
	changes = append(changes, oc...)
	changes = append(changes, contractChanges(baseDoc, curDoc)...)
	for _, pf := range policyChanged {
		changes = append(changes, change{"contract", "policy-file-changed", pf, "门禁策略文件相对基线被改动（可能放宽检测，须评审与变更说明）"})
	}
	if baseRM != nil {
		curRM, rerr := os.ReadFile(*readme)
		if rerr != nil {
			fmt.Fprintln(os.Stderr, "读取失败:", rerr)
			return 2
		}
		if len(parseErrorCodes(string(baseRM))) == 0 {
			fmt.Fprintln(os.Stderr, "openapi-breaking 失败: 基线 README 未解析出任何错误码（索引缺失或格式变更），比较不可信")
			return 1
		}
		changes = append(changes, errorCodeChanges(string(baseRM), string(curRM))...)
	}

	if len(changes) == 0 {
		fmt.Printf("openapi-breaking OK: 相对 %s 无破坏性变更（oasdiff %s + operationId/x- 扩展 + 错误码索引）\n", baseLabel, "breaking")
		return 0
	}
	fmt.Fprintf(os.Stderr, "相对 %s 检出 %d 处破坏性变更:\n", baseLabel, len(changes))
	for i, c := range changes {
		if i >= 50 {
			fmt.Fprintf(os.Stderr, "  … 其余 %d 处省略\n", len(changes)-50)
			break
		}
		fmt.Fprintln(os.Stderr, "  "+c.String())
	}
	if *note == "" {
		fmt.Fprintln(os.Stderr, "openapi-breaking 失败: 破坏性变更须附 ADR/变更说明（-note docs/bmad/contract-changes/<说明>.md，模板见 _template.md；ADR-017 §10）")
		return 1
	}
	baseHasNote := func(rel string) bool {
		if *baseFile != "" {
			return false
		}
		_, ok, _ := gitShow(*baseRef, rel)
		return ok
	}
	if err := validateChangeNote(*note, changes, baseHasNote); err != nil {
		fmt.Fprintln(os.Stderr, "openapi-breaking 失败:", err)
		return 1
	}
	fmt.Printf("openapi-breaking: %d 处破坏性变更已由变更说明 %s 放行（须评审确认）\n", len(changes), *note)
	return 0
}

// reportRoutes 校验运行时路由清单并输出「M1 未实现 operation 清单」；m1Exit 为真时该清单非空即失败。
func reportRoutes(doc *openapi3T, routesFile string, m1Exit bool) int {
	rs, err := readRoutes(routesFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取路由清单失败:", err)
		return 2
	}
	fs, todo := checkRoutes(doc, rs)
	if len(fs) > 0 {
		fmt.Fprintf(os.Stderr, "openapi-check 失败: 运行时路由与 openapi.yaml 不一致，%d 处违规\n", len(fs))
		printFindings(fs)
		return 1
	}
	fmt.Printf("路由清单 OK: 运行时已注册 %d 条路由，均在 openapi.yaml 中且元数据一致\n", len(rs))
	fmt.Printf("M1 未实现 operation 清单（%d 个；阶段出口时必须为空，不以 501 假实现充数）:\n", len(todo))
	for _, t := range todo {
		fmt.Println("  - " + t)
	}
	if m1Exit && len(todo) > 0 {
		fmt.Fprintf(os.Stderr, "openapi-check 失败: M1 阶段出口要求未实现清单为空，仍有 %d 个\n", len(todo))
		return 1
	}
	return 0
}
