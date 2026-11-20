package tenantisolation

import (
	"fmt"
	"sort"
	"strings"

	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/httpx/contracttest"
)

// 隔离矩阵骨架：由 openapi.yaml 的 x-tenant-scope 自动生成（资源 × 操作 × 身份 × 目标租户情形），不手写 operation 清单。
//
// 期望值只来自「路由类型 × 身份 × X-Tenant-ID 情形」的授权语义（ADR-009 §4/§5、ADR-018 §10），与具体资源无关——因此对每个新增的
// 租户资源自动成立；资源自身的功能性隔离（列表只含本租户、他租户 id → 404 逐字节一致、响应字段 R-T1）由覆盖登记里的 Driven 驱动逐资源断言。

// 身份（矩阵的列）。
const (
	IdTenantA            = "tenantA"            // 租户 A 的 Console 会话（tenant_owner）
	IdTenantB            = "tenantB"            // 租户 B 的 Console 会话
	IdTokenA             = "tokenA"             // 租户 A 的 API Token
	IdTokenB             = "tokenB"             // 租户 B 的 API Token
	IdPlatformAdmin      = "platformAdmin"      // 平台管理员（platform_admin：含 tenant:impersonate）→ acting-as
	IdPlatformNoImp      = "platformNoImp"      // 平台审计员（只读，无 tenant:impersonate）
	IdPlatformToken      = "platformToken"      // 平台令牌（带 tenant:impersonate 与路由权限）→ acting-as
	IdPlatformTokenNoImp = "platformTokenNoImp" // 平台令牌（有路由权限，但 scopes 不含 tenant:impersonate）
	IdAnonymous          = "anonymous"          // 未认证
	IdPlatformActing     = "platformActing"     // Driven 驱动里的统称：平台主体 acting-as（管理员或令牌）
)

// 目标租户情形（X-Tenant-ID）。
const (
	SitNone      = "none"      // 不带 X-Tenant-ID
	SitOwn       = "own"       // 自己的租户（租户主体）/ 租户 A（平台主体）
	SitOther     = "other"     // 他租户 B
	SitMissing   = "missing"   // 不存在的租户 uid
	SitSuspended = "suspended" // 存在但状态不可用（suspended）
)

// Row 是矩阵的一行。
type Row struct {
	OperationID string
	Method      string
	Path        string
	Scope       httpx.TenantScope
	Identity    string
	Situation   string
	Want        int    // 期望状态码
	NotFoundSet bool   // 期望为「不泄漏存在性」的 404：同一矩阵内所有这类响应体必须逐字节一致
	Why         string // 期望的依据（失败时输出）
}

// scopeKind 是行生成使用的路由类型。
func isTenantFacing(s httpx.TenantScope) bool {
	return s == httpx.ScopeTenant || s == httpx.ScopeEither
}

// Skeleton 由 OpenAPI 的 operation 元数据生成全部行（含尚未实现的 operation：执行时按「已注册」过滤，未注册的只统计）。
func Skeleton(spec *contracttest.Spec) []Row {
	var rows []Row
	for _, id := range spec.OperationIDs() {
		op, _ := spec.Operation(id)
		m := op.Meta
		if m.Auth == httpx.AuthPublic || m.TenantScope == httpx.ScopePublic {
			continue
		}
		add := func(identity, sit string, want int, nf bool, why string) {
			rows = append(rows, Row{OperationID: id, Method: m.Method, Path: m.Path, Scope: m.TenantScope, Identity: identity, Situation: sit, Want: want, NotFoundSet: nf, Why: why})
		}
		for _, s := range []string{SitNone, SitOwn, SitOther, SitMissing, SitSuspended} {
			add(IdAnonymous, s, 401, false, "未认证一律 401")
		}
		switch m.TenantScope {
		case httpx.ScopePlatform:
			for _, s := range []string{SitNone, SitOwn, SitOther, SitMissing} {
				// 租户 Console 会话：realm 隔离，platform 路由只认 Admin 会话 → 在认证层 401（ADR-018 §1）；租户令牌：授权层 403（NFR-14）。
				add(IdTenantA, s, 401, false, "平台级路由：租户 Console 会话在路由层被拒绝（realm 隔离，ADR-018 §1）")
				add(IdTenantB, s, 401, false, "平台级路由：租户 Console 会话在路由层被拒绝（realm 隔离，ADR-018 §1）")
				add(IdTokenA, s, 403, false, "平台级路由：租户令牌访问平台资源被拒绝（NFR-14）")
			}
			for _, s := range []string{SitOwn, SitMissing} {
				add(IdPlatformAdmin, s, 400, false, "平台级资源不接受目标租户 X-Tenant-ID")
			}
		case httpx.ScopeTenant, httpx.ScopeEither:
			for _, ident := range []string{IdTenantA, IdTokenA} {
				for _, s := range []string{SitOther, SitMissing, SitSuspended} {
					add(ident, s, 404, true, "租户主体指定他租户/不存在的租户：404，不泄漏存在性（ADR-009 §4）")
				}
			}
			for _, s := range []string{SitOwn, SitOther, SitMissing, SitSuspended} {
				add(IdPlatformNoImp, s, 403, false, "平台主体无 tenant:impersonate 不得代租户操作（ADR-009 §5）")
				add(IdPlatformTokenNoImp, s, 403, false, "平台令牌 acting-as 同样受 tenant:impersonate 约束（令牌 scopes 不含它即 403，ADR-018 §10）")
			}
			for _, ident := range []string{IdPlatformAdmin, IdPlatformToken} {
				for _, s := range []string{SitMissing, SitSuspended} {
					add(ident, s, 404, true, "acting-as 的目标租户须真实存在且状态可用：否则 404（与他租户资源逐字节一致）")
				}
			}
			if m.TenantScope == httpx.ScopeTenant {
				add(IdPlatformAdmin, SitNone, 400, false, "tenant 路由：平台主体必须以 X-Tenant-ID 指定目标租户")
			}
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.OperationID != b.OperationID {
			return a.OperationID < b.OperationID
		}
		if a.Identity != b.Identity {
			return a.Identity < b.Identity
		}
		return a.Situation < b.Situation
	})
	return rows
}

// samplePath 把 `{param}` 换成一个合法 uid 样例（资源不存在）。
func samplePath(p string) string {
	const sample = "01M7QP5V346STC0AT56NME5NR3"
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		if p[i] == '{' {
			if j := strings.IndexByte(p[i:], '}'); j > 0 {
				b.WriteString(sample)
				i += j
				continue
			}
		}
		b.WriteByte(p[i])
	}
	return b.String()
}

func (r Row) String() string {
	return fmt.Sprintf("%s %s %s [%s × %s] → %d", r.OperationID, r.Method, r.Path, r.Identity, r.Situation, r.Want)
}
