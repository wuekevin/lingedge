package cfgcheck

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Problem 是一个配置问题。Path 为点分键路径（列表项为 [i]；无法归属到键时为空）；Line/Column 从 1 开始（0 = 未知）。Reason 不含任何取值。
type Problem struct {
	Path   string `json:"path,omitempty"`
	Line   int    `json:"line,omitempty"`
	Column int    `json:"column,omitempty"`
	Reason string `json:"reason"`
}

// String 返回 `行:列 路径: 原因` 形式。
func (p Problem) String() string {
	var b strings.Builder
	if p.Line > 0 {
		b.WriteString("第 " + strconv.Itoa(p.Line) + " 行")
		if p.Column > 0 {
			b.WriteString(" 第 " + strconv.Itoa(p.Column) + " 列")
		}
		b.WriteString("：")
	}
	if p.Path != "" {
		b.WriteString(p.Path + "：")
	}
	b.WriteString(p.Reason)
	return b.String()
}

// MaxBytes 是接受的配置文件上限。
const MaxBytes = 1 << 20

type pos struct{ line, col int }

// Doc 是解析后的 YAML 文档（带键路径 → 位置索引）。
type Doc struct {
	Root *yaml.Node
	idx  map[string]pos
}

var (
	reTypeLine = regexp.MustCompile(`^line (\d+): (.*)$`)
	reBacktick = regexp.MustCompile("(?s)`.*`")
	reQuoted   = regexp.MustCompile(`"[^"]*"`)
	reYAMLLine = regexp.MustCompile(`(?:^|[^0-9])line (\d+)`)
)

// sanitize 去掉 yaml 错误文本里可能回显的标量内容（反引号/引号包裹的值），并截断。
func sanitize(s string) string {
	s = strings.TrimPrefix(s, "yaml: ")
	s = reBacktick.ReplaceAllString(s, "<值已隐去>")
	s = reQuoted.ReplaceAllString(s, "<值已隐去>")
	if r := []rune(s); len(r) > 160 {
		s = string(r[:160]) + "…"
	}
	return s
}

func lineOf(msg string) int {
	if m := reYAMLLine.FindStringSubmatch(msg); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

// ParseYAML 解析 b：语法错误、多文档、顶层非映射、重复键都转为带行号的 Problem。返回的 Doc 在语法失败时为 nil。
func ParseYAML(b []byte) (*Doc, []Problem) {
	if len(b) > MaxBytes {
		return nil, []Problem{{Reason: fmt.Sprintf("配置文件过大（>%d 字节）", MaxBytes)}}
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var root yaml.Node
	if err := dec.Decode(&root); err != nil {
		if errors.Is(err, io.EOF) { // 空文件：等价于空映射
			return &Doc{Root: &yaml.Node{Kind: yaml.MappingNode}, idx: map[string]pos{}}, nil
		}
		return nil, []Problem{{Line: lineOf(err.Error()), Reason: "YAML 语法错误：" + sanitize(err.Error())}}
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		line := 0
		if err == nil {
			line = extra.Line
		} else {
			line = lineOf(err.Error())
		}
		return nil, []Problem{{Line: line, Reason: "只允许单个 YAML 文档"}}
	}
	n := &root
	if n.Kind == yaml.DocumentNode && len(n.Content) == 1 {
		n = n.Content[0]
	}
	d := &Doc{Root: n, idx: map[string]pos{}}
	var probs []Problem
	if n.Kind != yaml.MappingNode {
		if n.Kind == yaml.ScalarNode && n.Tag == "!!null" {
			return &Doc{Root: &yaml.Node{Kind: yaml.MappingNode}, idx: map[string]pos{}}, nil
		}
		return nil, []Problem{{Line: n.Line, Column: n.Column, Reason: "顶层必须是键值映射"}}
	}
	d.index(n, "", &probs)
	return d, probs
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func resolve(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

// index 建立键路径 → 位置索引，并报告重复键。
func (d *Doc) index(n *yaml.Node, path string, probs *[]Problem) {
	n = resolve(n)
	switch n.Kind {
	case yaml.MappingNode:
		seen := map[string]int{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			p := join(path, k.Value)
			if first, dup := seen[k.Value]; dup {
				*probs = append(*probs, Problem{Path: p, Line: k.Line, Column: k.Column, Reason: "重复的键（首次出现在第 " + strconv.Itoa(first) + " 行）"})
				continue
			}
			seen[k.Value] = k.Line
			d.idx[p] = pos{k.Line, k.Column}
			d.index(v, p, probs)
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			p := path + "[" + strconv.Itoa(i) + "]"
			d.idx[p] = pos{c.Line, c.Column}
			d.index(c, p, probs)
		}
	}
}

// Locate 返回键路径的位置；路径不存在时退到最深的已存在前缀，都没有则 (0,0)。
func (d *Doc) Locate(path string) (line, col int) {
	if d == nil {
		return 0, 0
	}
	for path != "" {
		if p, ok := d.idx[path]; ok {
			return p.line, p.col
		}
		if i := strings.LastIndexAny(path, ".["); i >= 0 {
			path = path[:i]
		} else {
			break
		}
	}
	return 0, 0
}

// At 给 Problem 补上位置（已有行号的不改）。
func (d *Doc) At(p Problem) Problem {
	if p.Line == 0 {
		p.Line, p.Column = d.Locate(p.Path)
	}
	return p
}

// CheckShape 按 t（结构体，yaml 标签即键名）检查文档形状：未知键、节点种类（映射/序列/标量）。不检查取值。
func (d *Doc) CheckShape(t reflect.Type) []Problem {
	var out []Problem
	walkShape(d.Root, t, "", &out)
	return out
}

func yamlKeys(t reflect.Type) map[string]reflect.Type {
	m := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if !f.IsExported() || tag == "" || tag == "-" {
			continue
		}
		m[tag] = f.Type
	}
	return m
}

func walkShape(n *yaml.Node, t reflect.Type, path string, out *[]Problem) {
	n = resolve(n)
	if n == nil || (n.Kind == yaml.ScalarNode && n.Tag == "!!null") {
		return
	}
	bad := func(why string) {
		*out = append(*out, Problem{Path: path, Line: n.Line, Column: n.Column, Reason: why})
	}
	switch t.Kind() {
	case reflect.Struct:
		if n.Kind != yaml.MappingNode {
			bad("必须是键值映射")
			return
		}
		keys := yamlKeys(t)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			ft, ok := keys[k.Value]
			if !ok {
				*out = append(*out, Problem{Path: join(path, k.Value), Line: k.Line, Column: k.Column, Reason: "未知的键（拼写错误或该版本不支持）"})
				continue
			}
			walkShape(v, ft, join(path, k.Value), out)
		}
	case reflect.Slice:
		if n.Kind != yaml.SequenceNode {
			bad("必须是列表")
			return
		}
		for i, c := range n.Content {
			walkShape(c, t.Elem(), path+"["+strconv.Itoa(i)+"]", out)
		}
	default:
		if n.Kind != yaml.ScalarNode {
			bad("必须是单个值（不是映射或列表）")
		}
	}
}

// DecodeStrict 把文档严格解码进 v（未知键报错），类型不符转为带行号的 Problem。仅在 CheckShape 无问题时调用。
func DecodeStrict(b []byte, v any) []Problem {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		var te *yaml.TypeError
		if errors.As(err, &te) {
			var out []Problem
			for _, m := range te.Errors {
				p := Problem{Reason: "类型不符：" + sanitize(m)}
				if mm := reTypeLine.FindStringSubmatch(m); mm != nil {
					p.Line, _ = strconv.Atoi(mm[1])
					p.Reason = "类型不符：" + sanitize(mm[2])
				}
				out = append(out, p)
			}
			return out
		}
		return []Problem{{Line: lineOf(err.Error()), Reason: sanitize(err.Error())}}
	}
	return nil
}

// LineFromReason 从 yaml/config 的问题文本里取出「line N」的行号（没有则 0）。
func LineFromReason(reason string) int { return lineOf(reason) }
