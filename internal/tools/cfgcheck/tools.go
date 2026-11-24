package cfgcheck

import (
	"reflect"

	"github.com/wuekevin/lingedge/internal/tools/installer"
)

// CheckToolsYAML 校验 tools.yaml 的内容：语法/重复键 → 形状（未知键、种类）→ 类型 → 取值（installer.ToolsConfig.Problems，与 install 读取时同一份规则）。
// 前一层有问题时不再进入下一层（避免在无意义的值上产生噪声）。
func CheckToolsYAML(b []byte) []Problem {
	doc, probs := ParseYAML(b)
	if doc == nil {
		return probs
	}
	probs = append(probs, doc.CheckShape(reflect.TypeOf(installer.ToolsConfig{}))...)
	if len(probs) > 0 {
		return sortProblems(probs)
	}
	var cfg installer.ToolsConfig
	if ps := DecodeStrict(b, &cfg); len(ps) > 0 {
		return sortProblems(ps)
	}
	for _, p := range cfg.Problems() {
		probs = append(probs, doc.At(Problem{Path: p.Path, Reason: p.Reason}))
	}
	return sortProblems(probs)
}
