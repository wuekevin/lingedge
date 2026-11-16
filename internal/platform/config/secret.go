package config

import (
	"encoding/json"
	"fmt"

	"go.yaml.in/yaml/v3"

	"github.com/wuekevin/lingedge/pkg/redact"
)

// Secret 是配置里的秘密值（口令等）。任何格式化、序列化、日志输出都只得到 ***；只有显式调用 Reveal 才能取得明文。
//
// 内部用指针保存明文：即便某处以未导出字段持有 Secret 并被 fmt 反射打印，输出的也只是指针地址而不是内容。
type Secret struct{ p *string }

// NewSecret 包装明文。
func NewSecret(s string) Secret { return Secret{p: &s} }

// Reveal 返回明文。只应在建立连接等必须使用明文的地方调用，且结果不得写入日志、错误或响应。
func (s Secret) Reveal() string {
	if s.p == nil {
		return ""
	}
	return *s.p
}

// IsSet 报告是否已设置非空值。
func (s Secret) IsSet() bool { return s.p != nil && *s.p != "" }

func (s Secret) masked() string {
	if s.IsSet() {
		return redact.Mask
	}
	return ""
}

// String 实现 fmt.Stringer（zap.Stringer / zap.Any 经此得到 ***）。
func (s Secret) String() string { return s.masked() }

// GoString 实现 fmt.GoStringer（%#v）。
func (s Secret) GoString() string { return `config.Secret("` + s.masked() + `")` }

// Format 实现 fmt.Formatter：所有动词（%v %+v %s %q %x %d …）一律输出掩码文本。
func (s Secret) Format(f fmt.State, verb rune) {
	switch {
	case verb == 'q':
		_, _ = fmt.Fprintf(f, "%q", s.masked())
	case verb == 'v' && f.Flag('#'):
		_, _ = fmt.Fprint(f, s.GoString())
	default:
		_, _ = fmt.Fprint(f, s.masked())
	}
}

// MarshalText 实现 encoding.TextMarshaler。
func (s Secret) MarshalText() ([]byte, error) { return []byte(s.masked()), nil }

// MarshalJSON 实现 json.Marshaler。
func (s Secret) MarshalJSON() ([]byte, error) { return json.Marshal(s.masked()) }

// MarshalYAML 实现 yaml.Marshaler。
func (s Secret) MarshalYAML() (any, error) { return s.masked(), nil }

// UnmarshalYAML 接受标量（字符串或数字写法的口令）；映射/序列报错且不回显内容。
func (s *Secret) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("must be a scalar string")
	}
	v := n.Value
	s.p = &v
	return nil
}
