package contracttest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/idempotency"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
)

// 租户 Scope 覆盖检查（Story 1.20 AC4、ADR-009 §7）：x-tenant-scope 为 tenant|either 的路由必须经 tenantscope（Scope 钩子）——
// 缺失则失败（CI 经 make tenantscope-check 运行；负向样本见 scopecoverage_test.go）。
//
// 做法（动态、对真实路由表）：用探针钩子装配真实的路由集合——认证钩子放行一个固定主体、授权钩子放行、Scope 钩子是计数探针且**一律拒绝**、
// 审计钩子放行——然后对每条需认证的 tenant|either（顺带 platform）路由发一个请求，断言：该路由的 operationId 恰好经过一次 Scope 钩子，
// 且响应正是探针的拒绝（因而 handler 没有执行）。任何不经 Scope 钩子就能到达 handler 的路由（链被改坏、旁路 mux、自带宽松钩子）都会被发现。

// BuildFunc 用给定的钩子装配「真实的路由集合」，返回对外的 handler 与已注册路由的元数据。
type BuildFunc func(h httpx.Hooks) (http.Handler, []httpx.RouteMeta, error)

// probeMarker 是探针拒绝响应里的消息，用于区分「探针拒绝」与其他 403。
const probeMarker = "scope-probe-deny"

type probePrincipal struct{}

func (probePrincipal) ActorID() uint64           { return 1 }
func (probePrincipal) IsPlatform() bool          { return true }
func (probePrincipal) TenantID() uint64          { return 0 }
func (probePrincipal) HasPermission(string) bool { return true }

type probeHooks struct {
	mu    sync.Mutex
	calls map[string]int
}

func (p *probeHooks) Authenticate(*http.Request, httpx.RouteMeta) (tenantscope.Principal, error) {
	return probePrincipal{}, nil
}
func (p *probeHooks) Authorize(*http.Request, httpx.RouteMeta, tenantscope.Principal) error {
	return nil
}
func (p *probeHooks) ResolveScope(_ *http.Request, meta httpx.RouteMeta, _ tenantscope.Principal) (tenantscope.Scope, error) {
	p.mu.Lock()
	p.calls[meta.OperationID]++
	p.mu.Unlock()
	return tenantscope.Scope{}, apierr.New(apierr.AuthForbidden, probeMarker)
}
func (p *probeHooks) Begin(*http.Request, httpx.RouteMeta) (func(httpx.AuditOutcome), error) {
	return nil, nil
}

// NopIdempotencyStore 是仅用于装配探针路由的幂等存储：探针的 Scope 钩子先拒绝，请求不会走到幂等中间件，任何方法被调用都是错误。
type NopIdempotencyStore struct{}

var errNopStore = errors.New("contracttest: NopIdempotencyStore 不应被调用")

// Begin 实现 idempotency.Store。
func (NopIdempotencyStore) Begin(context.Context, idempotency.Request) (idempotency.Outcome, error) {
	return idempotency.Outcome{}, errNopStore
}

// Complete 实现 idempotency.Store。
func (NopIdempotencyStore) Complete(context.Context, uint64, int, []byte, uint64) error {
	return errNopStore
}

// Abandon 实现 idempotency.Store。
func (NopIdempotencyStore) Abandon(context.Context, uint64) error { return errNopStore }

// DeleteExpired 实现 idempotency.Store。
func (NopIdempotencyStore) DeleteExpired(context.Context, int) (int64, error) { return 0, errNopStore }

// recorder 是最小的 http.ResponseWriter（非测试代码不得 import net/http/httptest，NFR-1）。
type recorder struct {
	header http.Header
	code   int
	body   strings.Builder
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) WriteHeader(code int)        { r.code = code }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }

const sampleUID = "01M7QP5V346STC0AT56NME5NR3"

// samplePath 把 `{param}` 换成一个合法的 uid 样例。
func samplePath(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		if p[i] == '{' {
			j := strings.IndexByte(p[i:], '}')
			if j > 0 {
				b.WriteString(sampleUID)
				i += j
				continue
			}
		}
		b.WriteByte(p[i])
	}
	return b.String()
}

// CheckScopeCoverage 对 build 装配的真实路由集合做租户 Scope 覆盖检查，返回全部问题（空表示通过）。
// 检查对象：Auth=required 且 TenantScope 为 tenant|either|platform 的路由（公开路由不经主体，不在其列）。
func CheckScopeCoverage(build BuildFunc) []string {
	probe := &probeHooks{calls: map[string]int{}}
	h, routes, err := build(httpx.Hooks{Authenticator: probe, Authorizer: probe, Scope: probe, Auditor: probe})
	if err != nil {
		return []string{"装配路由失败: " + err.Error()}
	}
	var problems []string
	checked := 0
	for _, m := range routes {
		if m.Auth != httpx.AuthRequired || m.TenantScope == httpx.ScopePublic {
			continue
		}
		checked++
		before := probe.calls[m.OperationID]
		rec := &recorder{header: http.Header{}, code: http.StatusOK}
		req, err := http.NewRequest(m.Method, "http://probe.invalid"+httpx.APIPrefix+samplePath(m.Path), nil)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: 无法构造探针请求: %v", m.OperationID, err))
			continue
		}
		h.ServeHTTP(rec, req)
		got := probe.calls[m.OperationID] - before
		switch {
		case got != 1:
			problems = append(problems, fmt.Sprintf("%s %s %s（x-tenant-scope=%s）：未经 tenantscope（Scope 钩子被调用 %d 次，状态 %d）——路由不得绕过 Scope 钩子",
				m.OperationID, m.Method, m.Path, m.TenantScope, got, rec.code))
		case rec.code != http.StatusForbidden || !strings.Contains(rec.body.String(), probeMarker):
			problems = append(problems, fmt.Sprintf("%s %s %s：Scope 钩子拒绝后请求仍继续（状态 %d）——handler 可能在 Scope 拒绝后执行", m.OperationID, m.Method, m.Path, rec.code))
		}
	}
	if checked == 0 {
		problems = append(problems, "没有任何需认证的路由被检查（路由集合为空？）：覆盖检查不能空转通过")
	}
	sort.Strings(problems)
	return problems
}
