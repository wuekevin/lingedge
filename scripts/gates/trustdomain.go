package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"strings"
)

// 三个信任域门禁（Story 1.23 AC5、ADR-012 §2、ADR-002 §3、ADR-003 §3）：
//
// 信任域①发行清单签名（Tools 验签，**私钥离线**）、②配置修订签名、③节点 mTLS CA 互相独立。Server 只持有②③的私钥（signing_keys，信封加密）；
// **Server 代码路径里不得存在任何加载发行清单私钥的逻辑**——Server 被攻陷时，攻击者不应能签发「合法的 Server/Tools/Agent 发行清单」。
//
// 扫描范围（Server 代码路径）：internal/ 下除 internal/tools（Tools 只验签、不持私钥，且与 Server 无编译依赖）与 internal/testdouble 之外的全部非测试 Go 源码、
// cmd/lingedge-server、以及 pkg/（pkg 被 Server 链接，任何加载函数都会进入 Server 二进制）；另外扫描 configs/ 的 Server 示例配置里非注释行。
//
//	trust-release-key-name     函数/方法/变量/常量/调用的标识符名表示「加载/持有发行清单私钥」（release|manifest 与 private key|secret key|signing key|signer 同名出现）；
//	trust-release-key-literal  字符串字面量引用发行私钥文件/键名（release.key、release_signing_key、manifest-private-key …）或发行清单签名域前缀 "LINGEDGE-MANIFEST-V1"
//	                           （该前缀只属于 Tools 的验签与离线签名流水线；Server 既不签也不验发行清单）；
//	trust-release-key-config   Server 示例配置的非注释行出现发行私钥路径/键名。
//
// 二进制符号层面的同类检查见 tests/security/trustdomain（对真实编译出的 lingedge-server 做 `go tool nm`）。门禁自身的负向样本在 trustdomain_test.go。

var (
	// 标识符：「release/manifest」与「私钥」类词同名出现（任一顺序）。
	reTrustName = regexp.MustCompile(`(?i)((release|manifest)[a-z0-9_]*(priv(ate)?_?key|secret_?key|signing_?key|signer|privkey)|(priv(ate)?_?key|secret_?key|signing_?key|privkey)[a-z0-9_]*(release|manifest))`)
	// 字面量：发行私钥文件/键名，或发行清单签名域前缀。
	reTrustLiteral = regexp.MustCompile(`(?i)(release|manifest)[-_. ]?(signing[-_. ]?)?(priv(ate)?|secret)[-_. ]?key|release[-_.]?signing[-_.]?key|release[-_.]key\b|LINGEDGE-MANIFEST-V1`)
)

// trustDomainScope 报告相对路径是否属于 Server 代码路径。
func trustDomainScope(rel string) bool {
	if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
		return false
	}
	switch {
	case strings.HasPrefix(rel, "internal/tools/"), strings.HasPrefix(rel, "internal/testdouble/"):
		return false
	case strings.HasPrefix(rel, "internal/"), strings.HasPrefix(rel, "pkg/"), strings.HasPrefix(rel, "cmd/lingedge-server/"):
		return true
	}
	return false
}

// checkTrustDomain 检查 Go 源码（rel → 内容）与 Server 示例配置（rel → 内容）。
func checkTrustDomain(goFiles, configs map[string]string) []Finding {
	var out []Finding
	add := func(file string, line int, rule, f string, a ...any) {
		out = append(out, Finding{File: file, Line: line, Rule: rule, Msg: fmt.Sprintf(f, a...)})
	}
	for rel, src := range goFiles {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, rel, src, 0)
		if err != nil {
			add(rel, 0, "trust-parse", "无法解析 Go 文件: %v", err)
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				if reTrustName.MatchString(x.Name) {
					add(rel, fset.Position(x.Pos()).Line, "trust-release-key-name",
						"标识符 %q 表示加载/持有发行清单私钥：发行签名私钥离线，Server 代码路径中不得存在此类逻辑（ADR-012 §2）", x.Name)
				}
			case *ast.BasicLit:
				if x.Kind == token.STRING {
					if v, err := strconv.Unquote(x.Value); err == nil && reTrustLiteral.MatchString(v) {
						add(rel, fset.Position(x.Pos()).Line, "trust-release-key-literal",
							"字符串字面量 %q 引用发行清单私钥或发行清单签名域：Server 不得持有、读取或使用（ADR-012 §2）", trimTo(v, 40))
					}
				}
			}
			return true
		})
	}
	for rel, text := range configs {
		for i, ln := range strings.Split(text, "\n") {
			if t := strings.TrimSpace(ln); t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			if reTrustLiteral.MatchString(ln) || reTrustName.MatchString(ln) {
				add(rel, i+1, "trust-release-key-config", "Server 配置不得引用发行清单私钥（发行私钥离线，ADR-012 §2）: %s", trimTo(strings.TrimSpace(ln), 60))
			}
		}
	}
	return out
}

func trimTo(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func runTrustDomain(root string, _ options) ([]Finding, error) {
	goFiles := map[string]string{}
	configs := map[string]string{}
	err := walkText(root, nil, func(f textFile) {
		switch {
		case trustDomainScope(f.Rel):
			goFiles[f.Rel] = string(f.Content)
		case strings.HasPrefix(f.Rel, "configs/") && (strings.HasSuffix(f.Rel, ".yaml") || strings.HasSuffix(f.Rel, ".yml")):
			configs[f.Rel] = string(f.Content)
		}
	})
	if err != nil {
		return nil, err
	}
	out := checkTrustDomain(goFiles, configs)
	if len(goFiles) < 50 { // 门禁不能在扫描范围为空时放行
		out = append(out, Finding{File: "internal", Rule: "trust-scan-empty", Msg: fmt.Sprintf("只扫描到 %d 个 Server 源文件，扫描范围异常（门禁不得在扫描为空时放行）", len(goFiles))})
	}
	return out, nil
}
