package main

import (
	"regexp"
	"strings"
)

// 品牌检查（NFR-29）：仓库不得出现其他产品名残留。
// 例外：docs/ 下的设计/规划文档中，对既有项目的「隔离说明」（上下文各一行内含隔离/不触碰等措辞）。
// 代码、配置、脚本、README 等一律不得出现。词表见 data/brand-forbidden.txt。

var reIsolation = regexp.MustCompile(`隔离|不触碰|禁止触碰|不得触碰|不触及|不碰|不得影响|不得改动|不改动|禁止改动|改动\s*\x60|品牌残留|产品名残留|外部项目名`)

func runBrand(root string, _ options) ([]Finding, error) {
	var res []*regexp.Regexp
	for _, l := range loadLines("brand-forbidden.txt") {
		res = append(res, regexp.MustCompile(`(?i)`+l))
	}
	var out []Finding
	err := walkText(root, nil, func(f textFile) {
		for _, re := range res {
			if re.MatchString(f.Rel) {
				out = append(out, Finding{File: f.Rel, Rule: "brand", Msg: "路径含其他产品名（NFR-29）：" + re.String()})
			}
		}
		lines := strings.Split(string(f.Content), "\n")
		inDocs := strings.HasPrefix(f.Rel, "docs/")
		for i, line := range lines {
			for _, re := range res {
				loc := re.FindStringIndex(line)
				if loc == nil {
					continue
				}
				if inDocs && isolationContext(lines, i) {
					continue
				}
				out = append(out, Finding{File: f.Rel, Line: i + 1, Rule: "brand",
					Msg: "出现其他产品名 " + line[loc[0]:loc[1]] + "（NFR-29；仅 docs/ 中的隔离说明可豁免）"})
			}
		}
	})
	return out, err
}

func isolationContext(lines []string, i int) bool {
	for j := i - 1; j <= i+1; j++ {
		if j >= 0 && j < len(lines) && reIsolation.MatchString(lines[j]) {
			return true
		}
	}
	return false
}
