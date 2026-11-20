package httpx

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
)

// Auth 是路由的认证要求（对应 openapi 的 security 与 x-public）。
type Auth string

const (
	// AuthPublic：无需认证（security: []、x-public: true）。
	AuthPublic Auth = "public"
	// AuthRequired：需要已认证主体（Admin/Console 会话或 API Token，由认证钩子判定）。
	AuthRequired Auth = "required"
)

// TenantScope 对应 openapi 的 x-tenant-scope。
type TenantScope string

const (
	ScopePlatform TenantScope = "platform"
	ScopeTenant   TenantScope = "tenant"
	ScopeEither   TenantScope = "either"
	ScopePublic   TenantScope = "public"
)

// RouteScope 把 TenantScope 转为 tenantscope.RouteScope；public 无对应值，ok=false。
func (s TenantScope) RouteScope() (tenantscope.RouteScope, bool) {
	switch s {
	case ScopePlatform:
		return tenantscope.RoutePlatform, true
	case ScopeTenant:
		return tenantscope.RouteTenant, true
	case ScopeEither:
		return tenantscope.RouteEither, true
	}
	return "", false
}

// RouteMeta 是一条路由的必填元数据（ADR-017 §2），与 openapi.yaml 的 operationId、x-permissions、x-tenant-scope、x-audit、
// x-reauth、x-phase 一一对应。Method/Path 取自 openapi 的 paths（相对 servers[0].url，即 /api/v1 之下），用于把 handler 挂到路由树。
type RouteMeta struct {
	OperationID string
	Method      string // GET|POST|PUT|PATCH|DELETE
	Path        string // OpenAPI 路径模板，如 /sites/{siteId}/rule-validations；不得含 ':' 或 '*'
	Auth        Auth
	// Permissions 是 x-permissions：必须显式声明（nil 视为缺失；无权限要求时用 NoPermissions()）。
	Permissions []string
	TenantScope TenantScope
	Audit       bool // x-audit
	Reauth      bool // x-reauth（L4 step-up，由授权钩子强制）
	// Idempotent 表示该 operation 声明了 Idempotency-Key 头（openapi 的 IdempotencyKey 参数）；仅 POST。
	Idempotent bool
	Phase      string // x-phase，如 M1、P2
}

// NoPermissions 返回「显式声明无权限要求」的空列表（与 nil 的「缺失」区分）。
func NoPermissions() []string { return []string{} }

var (
	reOperationID = regexp.MustCompile(`^[a-z][A-Za-z0-9]*$`)
	rePermission  = regexp.MustCompile(`^[a-z][a-z0-9_-]*(:[a-z][a-z0-9_-]*)+$`)
	rePhase       = regexp.MustCompile(`^[MP][0-9]+$`)
	reLiteralSeg  = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)
	reParamName   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)
)

var methods = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}

// Validate 返回元数据的全部问题（空表示合法）。
func (m RouteMeta) Validate() []string {
	var p []string
	add := func(f string, a ...any) { p = append(p, fmt.Sprintf(f, a...)) }
	where := m.OperationID
	if where == "" {
		where = "(缺少 operationId)"
		add("缺少 OperationID")
	} else if !reOperationID.MatchString(m.OperationID) {
		add("%s: OperationID 非法（须为 lowerCamelCase）", where)
	}
	if !methods[m.Method] {
		add("%s: Method %q 非法（GET|POST|PUT|PATCH|DELETE）", where, m.Method)
	}
	p = append(p, m.validatePath(where)...)
	switch m.Auth {
	case AuthPublic, AuthRequired:
	case "":
		add("%s: 缺少 Auth", where)
	default:
		add("%s: Auth %q 非法（public|required）", where, m.Auth)
	}
	switch m.TenantScope {
	case ScopePlatform, ScopeTenant, ScopeEither, ScopePublic:
	case "":
		add("%s: 缺少 TenantScope", where)
	default:
		add("%s: TenantScope %q 非法（platform|tenant|either|public）", where, m.TenantScope)
	}
	if m.Auth != "" && m.TenantScope != "" {
		if (m.Auth == AuthPublic) != (m.TenantScope == ScopePublic) {
			add("%s: Auth=public 当且仅当 TenantScope=public（实际 %s/%s）", where, m.Auth, m.TenantScope)
		}
	}
	if m.Permissions == nil {
		add("%s: 缺少 Permissions（无权限要求请显式使用 NoPermissions()）", where)
	}
	seen := map[string]bool{}
	for _, perm := range m.Permissions {
		switch {
		case !rePermission.MatchString(perm):
			add("%s: 权限串 %q 非法（resource:action）", where, perm)
		case seen[perm]:
			add("%s: 权限串 %q 重复", where, perm)
		}
		seen[perm] = true
	}
	if m.Auth == AuthPublic {
		if len(m.Permissions) > 0 {
			add("%s: 公开路由不得声明权限", where)
		}
		if m.Reauth {
			add("%s: 公开路由不得声明 Reauth", where)
		}
	}
	if m.Idempotent && m.Method != "POST" {
		add("%s: Idempotent 仅适用于 POST", where)
	}
	if !rePhase.MatchString(m.Phase) {
		add("%s: Phase %q 非法（如 M1、P2）", where, m.Phase)
	}
	return p
}

func (m RouteMeta) validatePath(where string) []string {
	var p []string
	add := func(f string, a ...any) { p = append(p, fmt.Sprintf("%s: "+f, append([]any{where}, a...)...)) }
	if m.Path == "" || m.Path[0] != '/' {
		return []string{where + ": Path 必须以 / 开头"}
	}
	if strings.Contains(m.Path, ":") { // IMP-21：Gin 把 ':' 之后当通配参数，/sites/S1/rulesXXX 也会命中
		add("Path %q 含 ':'（IMP-21：自定义动作请用名词子资源，如 /rule-validations）", m.Path)
	}
	if strings.ContainsAny(m.Path, "*?#") || strings.ContainsAny(m.Path, " \t\r\n") {
		add("Path %q 含非法字符", m.Path)
	}
	if m.Path == "/" || strings.HasSuffix(m.Path, "/") || strings.Contains(m.Path, "//") {
		add("Path %q 不得为根、以 / 结尾或含空段", m.Path)
		return p
	}
	names := map[string]bool{}
	for _, seg := range strings.Split(m.Path[1:], "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			n := seg[1 : len(seg)-1]
			switch {
			case !reParamName.MatchString(n):
				add("路径参数 %q 非法", seg)
			case names[n]:
				add("路径参数 %q 重复", seg)
			}
			names[n] = true
			continue
		}
		if !reLiteralSeg.MatchString(seg) {
			add("路径段 %q 非法（路径参数必须占满整段 {name}，字面段仅允许 A-Za-z0-9._~-）", seg)
		}
	}
	return p
}

// ginPath 把 OpenAPI 路径模板转为 gin 路径（{name} → :name）；调用前须已通过 Validate。
func ginPath(tmpl string) string {
	segs := strings.Split(tmpl, "/")
	for i, s := range segs {
		if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
			segs[i] = ":" + s[1:len(s)-1]
		}
	}
	return strings.Join(segs, "/")
}
