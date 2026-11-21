package main

import (
	"regexp"
	"strings"
)

// CLI 旧标志名门禁（Story 1.25 AC7、MIN-04）：全仓只使用 tools-design §3/§4 的标志名。
//
//	不可逆确认 --accept-irreversible（旧 --yes-irreversible）
//	重注册     --force-reenroll    （旧 --reenroll）
//	清单过期   --allow-expired-manifest（旧 --allow-expired）
//	回滚放行   不存在（旧 --allow-rollback：sequence 回退是硬失败，退出码 5，没有绕过开关）
//
// 扫描范围：仓库全部文本文件。唯一的目录级例外是 docs/bmad/：那里是规划/评审/修订记录，
// 需要引述「旧名 → 新名」的历史（如 MIN-04 评审条目），不构成对旧标志的使用；其余位置（代码、配置、脚本、
// 架构文档、ADR、UX、API 文档）0 命中，ADR 里的修订说明也只能用新名或不点名旧名。
// 门禁自身与其自测目录（scripts/gates）由 walkText 统一跳过。

const flagsExemptDir = "docs/bmad/"

type legacyFlag struct {
	re   *regexp.Regexp
	use  string
	skip func(rest string) bool // 命中后的补充排除（Go 的 RE2 没有前瞻）
}

var legacyFlags = []legacyFlag{
	{regexp.MustCompile(`--allow-rollback`), "不存在回滚放行开关：清单 sequence 回退一律硬失败（退出码 5，ADR-012 §1.6）", nil},
	{regexp.MustCompile(`--yes-irreversible`), "请改用 --accept-irreversible（ADR-013、tools-design §3）", nil},
	{regexp.MustCompile(`--reenroll`), "请改用 --force-reenroll（tools-design §3、node-protocol）", nil},
	{regexp.MustCompile(`--allow-expired`), "请改用 --allow-expired-manifest（tools-design §3/§9.2、ADR-012）",
		func(rest string) bool { return strings.HasPrefix(rest, "-manifest") }},
}

func runFlags(root string, _ options) ([]Finding, error) {
	var out []Finding
	err := walkText(root, func(rel string, _ bool) bool {
		return strings.HasPrefix(rel+"/", flagsExemptDir)
	}, func(f textFile) {
		for i, line := range strings.Split(string(f.Content), "\n") {
			for _, lf := range legacyFlags {
				for _, loc := range lf.re.FindAllStringIndex(line, -1) {
					if lf.skip != nil && lf.skip(line[loc[1]:]) {
						continue
					}
					out = append(out, Finding{File: f.Rel, Line: i + 1, Rule: "flags-legacy",
						Msg: "出现旧 CLI 标志名 " + line[loc[0]:loc[1]] + "（MIN-04）：" + lf.use})
				}
			}
		}
	})
	return out, err
}
