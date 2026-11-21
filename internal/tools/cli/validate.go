package cli

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 可安装/管理的组件名（tools-design §3，install 组件枚举）。
var components = []string{"server", "admin-web", "console", "website", "node", "dns", "reporter", "docs", "status"}

// scheduler / worker 已内置于 Server（D-16）：install/update 它们退出码 8。
var builtinComponents = []string{"scheduler", "worker"}

func inList(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// checkComponent 校验组件名枚举。allowBuiltin 为 true 时 scheduler/worker 返回退出码 8 而不是 2。
// 用户输入只作为字面值回显/记录（shell 元字符不会被解释，也从不进入命令执行）。
func checkComponent(name string, allowBuiltin bool) *Error {
	if inList(components, name) {
		return nil
	}
	if allowBuiltin && inList(builtinComponents, name) {
		return &Error{
			Exit: ExitBuiltin, Code: "component.builtin",
			Message: fmt.Sprintf("组件 %s 已内置于 Server（D-16），无需单独安装", name),
			Hint:    "安装 server 即包含 scheduler 与 worker 角色；用 lingedge-server 的 roles 配置启用",
			Details: map[string]any{"component": name},
		}
	}
	return &Error{
		Exit: ExitUsage, Code: "usage.invalid_component",
		Message: fmt.Sprintf("未知组件 %q", name),
		Hint:    "可选组件：" + strings.Join(components, "|"),
		Details: map[string]any{"value": name, "allowed": components},
	}
}

func checkEnum(flag, value string, allowed ...string) *Error {
	if inList(allowed, value) {
		return nil
	}
	return &Error{
		Exit: ExitUsage, Code: "usage.invalid_value",
		Message: fmt.Sprintf("--%s 的值 %q 非法", flag, value),
		Hint:    "可选：" + strings.Join(allowed, "|"),
		Details: map[string]any{"flag": flag, "value": value, "allowed": allowed},
	}
}

var reSemver = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

func checkSemver(flag, value string) *Error {
	if reSemver.MatchString(value) {
		return nil
	}
	return &Error{
		Exit: ExitUsage, Code: "usage.invalid_version",
		Message: fmt.Sprintf("--%s 的值 %q 不是合法的 semver", flag, value),
		Hint:    "形如 1.2.3 或 1.2.3-rc.1",
		Details: map[string]any{"flag": flag, "value": value},
	}
}

// --set key=value：键为小写点分/连字符标识符，值任意文本（按字面值，经 schema 校验后才会进入配置，不经 shell）。
var reSetKey = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)

func checkSet(items []string) *Error {
	for _, it := range items {
		k, _, ok := strings.Cut(it, "=")
		if !ok || !reSetKey.MatchString(k) {
			// 不回显可能含口令的整项（值部分），只回显键部分。
			shown := k
			if len(shown) > 64 {
				shown = shown[:64] + "…"
			}
			return &Error{
				Exit: ExitUsage, Code: "usage.invalid_set",
				Message: fmt.Sprintf("--set 需要 key=value 形式，键为小写字母开头的标识符（收到键 %q）", shown),
				Hint:    "例：--set listen=127.0.0.1:8080",
				Details: map[string]any{"flag": "set"},
			}
		}
	}
	return nil
}

func checkHostPort(flag, value string) *Error {
	if _, port, err := net.SplitHostPort(value); err == nil {
		if n, perr := strconv.Atoi(port); perr == nil && n >= 1 && n <= 65535 {
			return nil
		}
	}
	return &Error{
		Exit: ExitUsage, Code: "usage.invalid_address",
		Message: fmt.Sprintf("--%s 的值 %q 不是 host:port", flag, value),
		Hint:    "例：127.0.0.1:8080",
		Details: map[string]any{"flag": flag, "value": value},
	}
}

func checkServerURL(value string) *Error {
	u, err := url.Parse(value)
	if err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil {
		return nil
	}
	return &Error{
		Exit: ExitUsage, Code: "usage.invalid_url",
		Message: "--server 必须是 http(s)://host[:port] 形式的地址，且不得内嵌用户名/口令",
		Hint:    "例：--server https://panel.example.com",
		Details: map[string]any{"flag": "server"},
	}
}

func checkDuration(flag, value string) *Error {
	if d, err := time.ParseDuration(value); err == nil && d > 0 {
		return nil
	}
	return &Error{
		Exit: ExitUsage, Code: "usage.invalid_duration",
		Message: fmt.Sprintf("--%s 的值 %q 不是正的时长", flag, value),
		Hint:    "例：10m、2h、90s",
		Details: map[string]any{"flag": flag, "value": value},
	}
}

func conflict(a, b string) *Error {
	return &Error{
		Exit: ExitUsage, Code: "usage.conflicting_flags",
		Message: fmt.Sprintf("--%s 与 --%s 不能同时使用", a, b),
		Details: map[string]any{"flags": []string{a, b}},
	}
}

func missing(what, hint string) *Error {
	return &Error{Exit: ExitUsage, Code: "usage.missing_argument", Message: "缺少" + what, Hint: hint}
}
