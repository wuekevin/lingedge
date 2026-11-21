package manifest

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// semver 库固定为 github.com/Masterminds/semver/v3 v3.5.0（go.mod 锁定；tools-design §10 要求标明版本）。
// 版本号用 StrictNewVersion（只接受严格 SemVer 2.0.0：无前导 v、无前导零、三段齐全）；
// 约束语法是 tools-design §10 的子集：`>=a.b.c <x.y.z`（空格即 AND）、`^`、`~`、`=`、`||`（OR），逐 token 校验后才交给库，
// 因此库额外支持的 `x`/`*` 通配、`!=`、连字符范围、逗号等一律拒绝（语法面保持最小、无歧义）。
//
// 预发布语义（来自库，边界测试固定它）：约束里不含预发布段时，预发布版本（如 1.1.0-rc.1）不满足该约束，
// 即 `>=1.0.0` 不接纳 1.1.0-rc.1；`>=1.1.0-rc.1` 才会接纳预发布。这与「默认选通道内最高稳定版」一致。

// Version 是严格 SemVer 版本。
type Version struct{ v *semver.Version }

// ParseVersion 解析严格 SemVer 版本。
func ParseVersion(s string) (Version, error) {
	v, err := semver.StrictNewVersion(s)
	if err != nil {
		return Version{}, newErr(KindSchema, "版本号 %q 不是严格 SemVer", trimTo(s, 64)).with("version", trimTo(s, 64))
	}
	return Version{v}, nil
}

// String 返回规范形式。
func (v Version) String() string { return v.v.String() }

// Compare 按 SemVer 优先级比较（忽略构建元数据）：-1/0/1。
func (v Version) Compare(o Version) int { return v.v.Compare(o.v) }

// Prerelease 报告是否带预发布段。
func (v Version) Prerelease() bool { return v.v.Prerelease() != "" }

// Constraint 是版本约束。
type Constraint struct {
	c   *semver.Constraints
	src string
}

var reConstraintToken = regexp.MustCompile(`^(>=|<=|>|<|=|\^|~)?(0|[1-9][0-9]*)(\.(0|[1-9][0-9]*)){0,2}(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

const maxConstraintLen = 256

// ParseConstraint 解析约束串（见包注释的语法子集）。
func ParseConstraint(s string) (Constraint, error) {
	bad := func(why string) (Constraint, error) {
		return Constraint{}, newErr(KindSchema, "版本约束 %q 非法：%s", trimTo(s, 64), why).with("constraint", trimTo(s, 64))
	}
	if strings.TrimSpace(s) == "" {
		return bad("为空")
	}
	if len(s) > maxConstraintLen {
		return bad("过长")
	}
	for _, group := range strings.Split(s, "||") {
		toks := strings.Fields(group)
		if len(toks) == 0 {
			return bad("存在空的 || 分支")
		}
		for _, t := range toks {
			if !reConstraintToken.MatchString(t) {
				return bad(fmt.Sprintf("token %q 不在支持的语法子集内", trimTo(t, 32)))
			}
		}
	}
	c, err := semver.NewConstraint(s)
	if err != nil {
		return bad("库拒绝: " + err.Error())
	}
	return Constraint{c: c, src: s}, nil
}

// String 返回原始约束串。
func (c Constraint) String() string { return c.src }

// Check 报告版本是否满足约束。
func (c Constraint) Check(v Version) bool { return c.c != nil && c.c.Check(v.v) }

func trimTo(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
