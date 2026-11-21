package audit

import (
	"context"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/audit/repository"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/redisx"
	"github.com/wuekevin/lingedge/internal/platform/requestid"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
	"github.com/wuekevin/lingedge/internal/platform/uid"
)

// 被拒绝类审计（Story 1.22b；database/platform-identity §3.1 `result=denied`）。设计要点——
//
//  1. 只记「已认证主体」的授权类拒绝：403 auth.forbidden、auth.reauth_required、feature.disabled（httpx 路由器在已认证之后才会调用 DeniedRecorder）。
//     **未认证请求（401）、校验失败（400/422）、CSRF 失败、登录失败一律不写 audit_logs**：匿名者可以零成本制造无限多条，会被用来刷表/淹没真实审计；
//     登录失败仍只写 login_logs（它有按 IP/账号的节流与专用保留策略），不进 audit_logs。
//  2. 防刷：同一 (主体, operation) 在一个窗口内只记一条（去重，默认 60 秒），并且每个主体每个窗口最多记 N 条（默认 20）——已认证主体最多制造
//     「operation 数 × 1」条/窗口，再被主体级上限截断；计数经 Redis（跨实例一致），Redis 不可用时降级为进程内计数（可能略宽松，多实例各记一条，可接受）。
//     没有注入 Redis 时整个被拒绝类审计关闭（不是「不去重地写」）。
//  3. 与写操作审计的区别：不要求与业务事务同提交（被拒绝的请求没有业务变更，不存在「业务已提交而审计没写」的不一致）；**写入失败只记日志与计数，
//     绝不影响已确定的 403 响应**（写操作审计则相反，是 fail-closed：审计写不进去，业务整体回滚）。
//  4. 内容：action=access.denied，resource_type=operation，resource_uid=operationId，result=denied；changes 只含拒绝环节、错误码、方法、路径模板、路由声明的权限串
//     与功能名（路由元数据，非秘密），不含请求体、查询串、头、令牌。tenant_id = 主体所属租户（平台主体为空）——租户管理员能在自己的审计里看到本租户成员的越权尝试。

// 默认防刷参数 [ASSUMPTION]：基线未规定，工程取值；可经 Options 调整。
const (
	DefaultDeniedWindow   = 60 * time.Second
	DefaultDeniedActorCap = 20
)

var _ httpx.DeniedRecorder = auditor{}

// DeniedRecorded / DeniedSuppressed / DeniedWriteFailures 返回被拒绝类审计的累计计数：已写入、被去重/上限抑制、写入失败（仅日志与计数，不影响响应）。
func (w *Writer) DeniedRecorded() int64 { return w.deniedWritten.Load() }

// DeniedSuppressed 见 DeniedRecorded。
func (w *Writer) DeniedSuppressed() int64 { return w.deniedSuppressed.Load() }

// DeniedWriteFailures 见 DeniedRecorded。
func (w *Writer) DeniedWriteFailures() int64 { return w.deniedFailed.Load() }

type deniedState struct {
	limit    *redisx.Limiter
	ks       redisx.Keyspace
	window   time.Duration
	actorCap int
}

func newDeniedState(c *redisx.Client, window time.Duration, actorCap int) *deniedState {
	if c == nil {
		return nil
	}
	if window <= 0 {
		window = DefaultDeniedWindow
	}
	if actorCap <= 0 {
		actorCap = DefaultDeniedActorCap
	}
	return &deniedState{limit: redisx.NewLimiter(c, nil), ks: c.Keyspace(), window: window, actorCap: actorCap}
}

// RecordDenied 实现 httpx.DeniedRecorder。所有错误在此吞掉（见包说明第 3 点）。
func (a auditor) RecordDenied(r *http.Request, meta httpx.RouteMeta, d httpx.Denial) {
	a.w.recordDenied(r, meta, d)
}

func (w *Writer) recordDenied(r *http.Request, meta httpx.RouteMeta, d httpx.Denial) {
	if w.denied == nil {
		return
	}
	ctx := r.Context()
	reqID, _ := requestid.FromContext(ctx)
	actor, err := actorOf(ctx, Event{}, tenantscope.Scope{}, false)
	if err != nil {
		w.log.Debug("被拒绝类审计：无法确定操作者，跳过", zap.String("operation_id", meta.OperationID), zap.String("request_id", reqID))
		return
	}
	actorKey := "u" + uitoa(actor.UserID)
	if actor.TokenID > 0 {
		actorKey = "k" + uitoa(actor.TokenID)
	}
	// 去重与上限用独立的短超时上下文：Redis 慢不能拖住响应；Allow 本身在 Redis 故障时降级为进程内计数。
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	k1, err1 := w.denied.ks.Key("audit", "denied", actorKey, meta.OperationID)
	k2, err2 := w.denied.ks.Key("audit", "deniedcap", actorKey)
	if err1 != nil || err2 != nil {
		w.deniedFailed.Add(1)
		w.log.Warn("被拒绝类审计：键构造失败，跳过", zap.String("operation_id", meta.OperationID), zap.String("request_id", reqID))
		return
	}
	// 先去重（同一 (主体, operation, 窗口) 只记一条），通过后才占用主体级配额：重复的同类请求不会耗尽该主体的上限。
	if dec, err := w.denied.limit.Allow(lctx, k1, 1, w.denied.window); err != nil || !dec.Allowed {
		w.deniedSuppressed.Add(1)
		return
	}
	if dec, err := w.denied.limit.Allow(lctx, k2, w.denied.actorCap, w.denied.window); err != nil || !dec.Allowed {
		w.deniedSuppressed.Add(1)
		return
	}

	changes := map[string]any{"stage": d.Stage, "code": string(d.Code), "method": meta.Method, "path": meta.Path}
	if len(meta.Permissions) > 0 {
		changes["permissions"] = meta.Permissions
	}
	if meta.Feature != "" {
		changes["feature"] = meta.Feature
	}
	row := repository.Row{
		CreatedAt: w.clk.Now(), ActorType: actor.Type, ActorLabel: trunc(actor.Label, 255), Action: string(ActionAccessDenied),
		ResourceType: ResourceOperation, Result: string(ResultDenied),
	}
	if actor.UserID > 0 {
		row.ActorUserID = &actor.UserID
	}
	if actor.TokenID > 0 {
		row.ActorTokenID = &actor.TokenID
	}
	if actor.TenantID > 0 {
		row.ActorTenantID = &actor.TenantID
		row.TenantID = &actor.TenantID
	}
	op := meta.OperationID
	row.ResourceUID = &op
	if row.UID, err = w.ids.New(); err != nil {
		w.deniedFailed.Add(1)
		return
	}
	if reqID != "" && uid.Valid(reqID) {
		row.RequestID = reqID
	} else if row.RequestID, err = w.ids.New(); err != nil {
		w.deniedFailed.Add(1)
		return
	}
	row.IP = ipBytes(httpx.ClientIPFrom(ctx))
	if ua := httpx.UserAgentFrom(ctx); ua != "" {
		ua = trunc(ua, 512)
		row.UserAgent = &ua
	}
	if row.ChangesJSON, err = redactedJSON(changes); err != nil {
		w.deniedFailed.Add(1)
		return
	}
	// 单条 INSERT（自动提交，不开业务事务）；失败只记日志与计数。
	wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer wcancel()
	if err := w.repo.Insert(wctx, tenantscope.Scope{}, row); err != nil {
		w.deniedFailed.Add(1)
		w.log.Warn("被拒绝类审计写入失败（不影响响应）", zap.String("operation_id", meta.OperationID), zap.String("request_id", reqID), zap.Error(err))
		return
	}
	w.deniedWritten.Add(1)
}

func uitoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
