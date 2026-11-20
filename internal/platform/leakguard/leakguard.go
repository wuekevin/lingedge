package leakguard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"
)

// 键名规则：与 scripts/contract 的 tenantLeakRe 同源（node_*、cluster_*、egress_node*、targets、驼峰形态），另外禁止裸的
// node/cluster/nodes/clusters 键（节点/集群列表或对象）。
var forbiddenKeyRe = regexp.MustCompile(`^(node_.*|cluster_.*|egress_node.*|targets|nodes?|clusters?|(node|cluster|egressNode)[A-Z].*)$`)

// ForbiddenKey 报告 JSON 键名是否属于 R-T1 禁止的内部标识（大小写敏感，与 OpenAPI 静态规则一致）。
func ForbiddenKey(k string) bool { return forbiddenKeyRe.MatchString(k) }

// 内部错误全文的特征：数据库驱动/SQL、Go 运行时、源码路径、网络层错误。租户可见响应只允许统一错误码与脱敏消息。
var internalErrorRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\berror \d{4}\b`),                                                    // MySQL: Error 1062
	regexp.MustCompile(`(?i)\b(sql|mysql|redis): `),                                              // 驱动错误前缀
	regexp.MustCompile(`(?i)\b(dial|read|write) tcp\b`),                                          // 网络层
	regexp.MustCompile(`goroutine \d+ \[`),                                                       // panic 栈
	regexp.MustCompile(`\.go:\d+`),                                                               // 源码位置
	regexp.MustCompile(`(?i)\b(select|insert into|update|delete from)\b.+\b(from|set|values)\b`), // SQL 文本
}

// Options 是一次扫描的参数。
type Options struct {
	// Internal 是调用方登记的内部取值：响应的任何字符串值（含键名）包含其中之一（大小写不敏感的子串）即违规。
	// 矩阵用它登记真实种入数据库的节点/集群的 uid、名称、主机名、IP。空串被忽略。
	Internal []string
	// ForbidIPValues 为 true 时，任何字符串值若含 IPv4/IPv6 字面量即违规（适用于不应出现任何地址的资源，如租户/配额/成员）。
	ForbidIPValues bool
}

// Violation 是一条违规。Path 是 JSON 路径（如 `$.items[0].node_id`）。
type Violation struct {
	Path   string
	Rule   string // key | internal-value | ip | internal-error | not-json
	Detail string
}

func (v Violation) String() string { return fmt.Sprintf("%s [%s] %s", v.Path, v.Rule, v.Detail) }

var ipCandidateRe = regexp.MustCompile(`[0-9A-Fa-f:.]{3,}`)

func containsIP(s string) bool {
	for _, c := range ipCandidateRe.FindAllString(s, -1) {
		if _, err := netip.ParseAddr(strings.Trim(c, ".:")); err == nil && strings.ContainsAny(c, ".:") {
			return true
		}
	}
	return false
}

// Scan 扫描一个 JSON 响应体并返回全部违规（按路径、规则排序，稳定）。空响应体（如 204）无违规；非 JSON 响应体按原文做内部取值与错误全文检查。
func Scan(body []byte, o Options) []Violation {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil
	}
	var root any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		vs := []Violation{{Path: "$", Rule: "not-json", Detail: "响应体不是 JSON，按原文检查"}}
		return append(vs, checkString("$", string(body), o)...)
	}
	var vs []Violation
	walk("$", root, o, &vs)
	sort.SliceStable(vs, func(i, j int) bool {
		if vs[i].Path != vs[j].Path {
			return vs[i].Path < vs[j].Path
		}
		return vs[i].Rule < vs[j].Rule
	})
	return vs
}

func walk(path string, v any, o Options, out *[]Violation) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			p := path + "." + k
			if ForbiddenKey(k) {
				*out = append(*out, Violation{Path: p, Rule: "key", Detail: "键名属于节点/集群内部标识（R-T1）"})
			}
			*out = append(*out, checkString(p+"(key)", k, Options{Internal: o.Internal})...)
			walk(p, val, o, out)
		}
	case []any:
		for i, val := range x {
			walk(fmt.Sprintf("%s[%d]", path, i), val, o, out)
		}
	case string:
		*out = append(*out, checkString(path, x, o)...)
	}
}

func checkString(path, s string, o Options) []Violation {
	var vs []Violation
	low := strings.ToLower(s)
	for _, in := range o.Internal {
		if in != "" && strings.Contains(low, strings.ToLower(in)) {
			vs = append(vs, Violation{Path: path, Rule: "internal-value", Detail: "含已登记的内部取值 " + quoteShort(in)})
		}
	}
	for _, re := range internalErrorRes {
		if re.MatchString(s) {
			vs = append(vs, Violation{Path: path, Rule: "internal-error", Detail: "疑似内部错误全文（" + re.String() + "）"})
			break
		}
	}
	if o.ForbidIPValues && containsIP(s) {
		vs = append(vs, Violation{Path: path, Rule: "ip", Detail: "含 IP 地址字面量"})
	}
	return vs
}

func quoteShort(s string) string {
	if len(s) > 40 {
		s = s[:40] + "…"
	}
	return fmt.Sprintf("%q", s)
}

// ForbiddenKeysIn 递归返回 JSON 值里所有违规键的路径（业务写入自由 JSON 时用于校验；不检查取值）。
func ForbiddenKeysIn(v any) []string {
	var out []string
	var rec func(path string, v any)
	rec = func(path string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, val := range x {
				if ForbiddenKey(k) {
					out = append(out, path+"."+k)
				}
				rec(path+"."+k, val)
			}
		case []any:
			for i, val := range x {
				rec(fmt.Sprintf("%s[%d]", path, i), val)
			}
		}
	}
	rec("$", v)
	sort.Strings(out)
	return out
}
