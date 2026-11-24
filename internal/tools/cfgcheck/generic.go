package cfgcheck

import (
	"sort"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/installer"
)

// CheckSyntaxOnly 只校验语法：YAML 语法、单文档、顶层为映射、无重复键。用于 schema 尚未交付的组件配置（node/dns/reporter，Epic 3）。
func CheckSyntaxOnly(b []byte) []Problem {
	doc, probs := ParseYAML(b)
	_ = doc
	return sortProblems(probs)
}

// CheckNginxSite 检查静态前端的站点片段与安全头文件（installer 的片段静态检查，只读、不执行 nginx）。
// component 用于核对片段首部的 `# lingedge-site:` 标记属于该组件。
func CheckNginxSite(component string, conf, headers []byte) []Problem {
	var probs []Problem
	c, _, ok := installer.ParseSiteMarker(conf)
	switch {
	case !ok:
		probs = append(probs, Problem{Reason: "站点片段缺少 `# lingedge-site:` 标记（不是 Tools 生成的片段，或被改坏）"})
	case c != component:
		probs = append(probs, Problem{Reason: "站点片段标记的组件是 " + c + "，不是 " + component})
	}
	for _, m := range installer.LintNginxSnippet(string(conf), installer.StaticHeadersPath(component)) {
		probs = append(probs, Problem{Reason: m})
	}
	for _, m := range installer.LintSecurityHeaders(string(headers)) {
		probs = append(probs, Problem{Reason: m})
	}
	return probs
}

func sortProblems(ps []Problem) []Problem {
	sort.SliceStable(ps, func(i, j int) bool {
		if ps[i].Line != ps[j].Line {
			return ps[i].Line < ps[j].Line
		}
		return strings.Compare(ps[i].Path, ps[j].Path) < 0
	})
	return ps
}
