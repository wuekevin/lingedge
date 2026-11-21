package installer

import (
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/resolver"
)

// Request 是一次安装请求（cli 从标志构造；也是 journal 里记录、recover --resume 据以续跑的内容）。
type Request struct {
	Component string `json:"component"`
	// Version/Variant 是用户固定的版本与变体；续跑时 Pinned 记录已求解出的全部组件版本，保证同一计划。
	Version string `json:"version,omitempty"`
	Variant string `json:"variant,omitempty"`
	Channel string `json:"channel"`
	// Offline 是 --offline 的 bundle 路径（空 = 在线）。
	Offline      string            `json:"offline,omitempty"`
	AllowExpired bool              `json:"allow_expired,omitempty"`
	Settings     map[string]string `json:"settings,omitempty"`
	// PortOverrides 来自 --set port.<组件>.<协议>.<端口>=<新端口>[@<绑定地址>]。
	PortOverrides []resolver.PortOverride `json:"port_overrides,omitempty"`
	SkipStart     bool                    `json:"skip_start,omitempty"`
	NoRollback    bool                    `json:"no_rollback,omitempty"`
	SetupListen   string                  `json:"setup_listen,omitempty"`
	// Accepted 记录安装时用户已确认冲突（既有内容保留、新内容写 .new）；续跑时据此不再重复询问。
	Accepted bool `json:"accepted,omitempty"`
	// Pinned 是求解结果（组件 → 版本/变体）：写入 journal 后供续跑使用。
	Pinned []resolver.Want `json:"pinned,omitempty"`

	// 以下是本次进程的交互设置，不写入 journal。
	DryRun         bool `json:"-"`
	NonInteractive bool `json:"-"`
	// Yes 表示用户已预先同意：被动升级、冲突后保留既有文件并写 .new、端口无法验证时继续。
	Yes bool `json:"-"`
}

// 限额：journal 的一行最多 64KB，请求要整体放进第 1 步 undo 的 extra。
const (
	maxSettings     = 64
	maxSettingValue = 1024
)

var (
	reSetKey  = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)
	rePortKey = regexp.MustCompile(`^port\.([a-z][a-z0-9-]{0,31})\.(tcp|udp)\.([0-9]{1,5})$`)
)

// SetError 是 --set 解析失败：只回显键，值（可能含口令）绝不回显。
type SetError struct {
	Key string
	Msg string
}

func (e *SetError) Error() string { return "--set " + e.Key + "：" + e.Msg }

// ParsedSet 是 --set 的解析结果。
type ParsedSet struct {
	Ports    []resolver.PortOverride
	Settings map[string]string
}

// ParseSet 解析 --set key=value（外部可控入口，有 fuzz）。语法：
//
//	port.<组件>.<tcp|udp>.<原端口>=<新端口>[@<绑定地址>]   换端口（resolver.PortOverride；绑定地址是 IP 字面量，IPv6 用 [::1] 或 ::1 均可）
//	<其他键>=<值>                                            组件初始配置项（交给 ComponentSpec.ValidateSettings 校验）
//
// 键必须匹配 ^[a-z][a-z0-9_.-]{0,127}$；重复的键、以 port. 开头但不符合端口语法的键、值含控制字符或超长一律拒绝。
// 输出顺序与输入无关（按键排序），便于确定性与 journal 比对。
func ParseSet(items []string) (ParsedSet, error) {
	out := ParsedSet{Settings: map[string]string{}}
	if len(items) > maxSettings {
		return out, &SetError{Key: "(多项)", Msg: fmt.Sprintf("最多 %d 项", maxSettings)}
	}
	seen := map[string]bool{}
	shown := func(k string) string {
		if len(k) > 64 {
			return k[:64] + "…"
		}
		return k
	}
	for _, it := range items {
		k, v, ok := strings.Cut(it, "=")
		if !ok || !reSetKey.MatchString(k) {
			return ParsedSet{}, &SetError{Key: shown(k), Msg: "需要 key=value，键为小写字母开头的标识符"}
		}
		if seen[k] {
			return ParsedSet{}, &SetError{Key: k, Msg: "重复的键"}
		}
		seen[k] = true
		if len(v) > maxSettingValue {
			return ParsedSet{}, &SetError{Key: k, Msg: fmt.Sprintf("值超过 %d 字节", maxSettingValue)}
		}
		for _, r := range v {
			if r < 0x20 || r == 0x7f {
				return ParsedSet{}, &SetError{Key: k, Msg: "值不得含控制字符"}
			}
		}
		if strings.HasPrefix(k, "port.") {
			po, err := parsePortSet(k, v)
			if err != nil {
				return ParsedSet{}, err
			}
			out.Ports = append(out.Ports, po)
			continue
		}
		out.Settings[k] = v
	}
	sort.SliceStable(out.Ports, func(i, j int) bool {
		a, b := out.Ports[i], out.Ports[j]
		if a.Component != b.Component {
			return a.Component < b.Component
		}
		if a.Proto != b.Proto {
			return a.Proto < b.Proto
		}
		return a.Port < b.Port
	})
	return out, nil
}

func parsePortSet(k, v string) (resolver.PortOverride, error) {
	m := rePortKey.FindStringSubmatch(k)
	if m == nil {
		return resolver.PortOverride{}, &SetError{Key: k, Msg: "端口覆盖的键必须是 port.<组件>.<tcp|udp>.<原端口>"}
	}
	from, _ := strconv.Atoi(m[3])
	if from < 1 || from > 65535 {
		return resolver.PortOverride{}, &SetError{Key: k, Msg: "原端口不在 1–65535"}
	}
	toS, bind, hasBind := strings.Cut(v, "@")
	to, err := strconv.Atoi(toS)
	if err != nil || to < 1 || to > 65535 || strconv.Itoa(to) != toS {
		return resolver.PortOverride{}, &SetError{Key: k, Msg: "新端口必须是 1–65535 的十进制整数"}
	}
	po := resolver.PortOverride{Component: m[1], Proto: m[2], Port: from, To: to}
	if hasBind {
		b := strings.TrimSuffix(strings.TrimPrefix(bind, "["), "]")
		ip := net.ParseIP(b)
		if ip == nil {
			return resolver.PortOverride{}, &SetError{Key: k, Msg: "绑定地址必须是 IP 字面量"}
		}
		po.Bind = b
	}
	return po, nil
}

// journalRequest 是写入 journal 第 1 步 undo 的载体（action=request，撤销时为空操作）。
const actRequest = "request"

// encodeRequest 把请求序列化进 undo extra；值经 registry 写入前的脱敏校验（疑似密钥会被拒绝写入，请求因此失败而不是泄漏）。
func encodeRequest(r Request) (registry.UndoAction, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return registry.UndoAction{}, err
	}
	if len(b) > 40<<10 {
		return registry.UndoAction{}, newErr(KindSettings, "请求过大，无法写入事务日志")
	}
	return registry.UndoAction{Action: actRequest, Extra: map[string]string{"v": "1", "request": string(b)}}, nil
}

// DecodeRequest 从 journal 的 undo 动作还原请求；不是 request 动作返回 false。
func DecodeRequest(a registry.UndoAction) (Request, bool, error) {
	if a.Action != actRequest {
		return Request{}, false, nil
	}
	if a.Extra["v"] != "1" {
		return Request{}, true, newErr(KindRecover, "journal 中的请求版本不受支持")
	}
	var r Request
	if err := json.Unmarshal([]byte(a.Extra["request"]), &r); err != nil {
		return Request{}, true, wrap(KindRecover, err, "journal 中的请求无法解析")
	}
	return r, true, nil
}
