package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/audit/repository"
	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/requestid"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
	"github.com/wuekevin/lingedge/internal/platform/uid"
	"github.com/wuekevin/lingedge/pkg/redact"
)

// Result 是操作结果（audit_logs.result）。
type Result string

// 结果取值。
const (
	ResultSuccess Result = "success"
	ResultFailure Result = "failure"
	ResultDenied  Result = "denied"
)

// 操作者类型（audit_logs.actor_type）。
const (
	ActorUser     = "user"
	ActorAPIToken = "api_token"
	ActorSystem   = "system"
	ActorNode     = "node"
)

// Actor 是操作者快照。认证主体经 ActorProvider 提供；登录这类「主体尚未进入请求上下文」的操作用 Event.Actor 显式给出。
type Actor struct {
	Type    string // user | api_token | system | node
	UserID  uint64 // 0 = 无（system/node）；api_token 取其拥有者
	TokenID uint64 // 仅 api_token
	Label   string // 邮箱 / 令牌前缀 / 节点名 / 系统组件名
	// TenantID 是操作者所属租户（平台用户为 0）。
	TenantID uint64
}

// ActorProvider 由认证主体实现：让审计记录器无需依赖认证模块即可取得操作者快照（返回基本类型，使领域包不必 import 本包）。
// 返回：类型（user/api_token）、操作者用户 id、令牌 id（仅 api_token）、标签（邮箱/令牌前缀）、操作者所属租户（平台为 0）。
type ActorProvider interface {
	AuditActor() (typ string, userID, tokenID uint64, label string, tenantID uint64)
}

// SystemActor 是系统/后台作业的操作者。
func SystemActor(label string) Actor { return Actor{Type: ActorSystem, Label: label} }

// Event 是一次审计记录的内容。操作者、acting_as、request_id、来源 IP 与 User-Agent 取自请求上下文，调用方不必（也不能）自己填。
type Event struct {
	// ResourceType/ResourceUID：对象类型与 uid（如 user + 用户 uid）。ResourceType 必填。
	ResourceType string
	ResourceUID  string
	// TenantID 是被操作对象所属租户（代租户操作时与操作者所属租户不同）。0 = 取请求 Scope 的租户（租户范围 Scope），平台范围为空。
	TenantID uint64
	// Result 缺省 success。
	Result Result
	// Changes 是变更摘要：写入前经 pkg/redact 脱敏（敏感键整体替换为 ***）。只放「改了什么」（字段名与新旧非敏感取值），
	// 绝不放口令、令牌、TOTP 种子、恢复码——调用方不应依赖脱敏兜底。
	Changes map[string]any
	// Meta 是附加信息（同样脱敏）。
	Meta map[string]any
	// Actor 非空时覆盖上下文里的认证主体（登录成功、MFA 校验等主体尚未进入上下文的操作）。
	Actor *Actor
}

// Recorder 是业务 Service 使用的审计记录器接口。
//
// Record 必须在业务事务内调用（db.Pool.WithTx 的 ctx）：审计行与业务写入同一事务，Record 返回错误时调用方必须让事务回滚
// （fail-closed：审计写不进去，业务操作就不成立）。不在事务内调用返回 ErrNotInTx。
type Recorder interface {
	Record(ctx context.Context, action Action, ev Event) error
}

// 错误。
var (
	// ErrNotInTx：Record 在事务之外调用——审计行不能与业务变更同事务，拒绝（IMP-19）。
	ErrNotInTx = errors.New("audit: Record 必须在业务事务内调用（审计与业务变更同一事务，IMP-19）")
	// ErrUnknownAction：动作未在 Catalog 登记，或与请求的 operation 登记的动作不符。
	ErrUnknownAction = errors.New("audit: 动作未登记或与 operation 不符")
	// ErrNoActor：既没有认证主体也没有显式 Actor——审计必须能归因到操作者。
	ErrNoActor = errors.New("audit: 无法确定操作者")
	// ErrInvalidEvent：事件字段不合法。
	ErrInvalidEvent = errors.New("audit: 事件不合法")
)

// IDGenerator 生成 audit_logs.uid / request_id（*uid.Generator 满足）。
type IDGenerator interface{ New() (string, error) }

// Options 是 New 的参数。
type Options struct {
	// Pool 必须是 app 连接池（与业务模块同一个池：事务经 ctx 传播，键含池标识）。
	Pool  db.Pool
	IDs   IDGenerator
	Clock clock.Clock
	Log   *zap.Logger
	// ExtraOps 是 Catalog 之外额外登记的 operation（仅测试/探针路由使用；生产装配不传）。
	ExtraOps []Op
	// OnMissing 在「声明了审计的成功请求却没有审计行」时被调用（测试与告警接线用）；可为 nil。
	OnMissing func(operationID string)
}

// Writer 是审计记录器（Recorder）与审计钩子（Auditor）的真实实现。
type Writer struct {
	pool      db.Pool
	repo      *repository.Repo
	ids       IDGenerator
	clk       clock.Clock
	log       *zap.Logger
	onMissing func(string)
	extra     []Op
	missing   atomic.Int64
}

var _ Recorder = (*Writer)(nil)

// New 创建 Writer。
func New(o Options) (*Writer, error) {
	if o.Pool == nil || o.IDs == nil {
		return nil, errors.New("audit: 缺少必需依赖（Pool、IDs）")
	}
	repo, err := repository.New(o.Pool)
	if err != nil {
		return nil, err
	}
	if o.Clock == nil {
		o.Clock = clock.System()
	}
	if o.Log == nil {
		o.Log = zap.NewNop()
	}
	return &Writer{pool: o.Pool, repo: repo, ids: o.IDs, clk: o.Clock, log: o.Log.Named("audit"), onMissing: o.OnMissing, extra: o.ExtraOps}, nil
}

// Repo 返回审计 Repository（读取 API 使用）。
func (w *Writer) Repo() *repository.Repo { return w.repo }

// maxChangesBytes 是变更摘要序列化后的上限；超限时整体替换为 {"truncated":true}（不截断半个 JSON，也不让审计写入因体积失败）。
const maxChangesBytes = 16 << 10

// Record 实现 Recorder。
func (w *Writer) Record(ctx context.Context, action Action, ev Event) error {
	if !w.pool.Executor(ctx).InTx() {
		return ErrNotInTx
	}
	if !knownAction(action) && !w.extraAction(action) {
		return fmt.Errorf("%w: %q", ErrUnknownAction, action)
	}
	if meta, ok := httpx.MetaFrom(ctx); ok && meta.Audit && !hasAction(w.actionsFor(meta.OperationID), action) {
		return fmt.Errorf("%w: operation %s 未登记动作 %q", ErrUnknownAction, meta.OperationID, action)
	}
	if ev.ResourceType == "" || len(ev.ResourceType) > 48 || len(ev.ResourceUID) > 64 {
		return fmt.Errorf("%w: resource_type/resource_uid", ErrInvalidEvent)
	}
	res := ev.Result
	switch res {
	case "":
		res = ResultSuccess
	case ResultSuccess, ResultFailure, ResultDenied:
	default:
		return fmt.Errorf("%w: result %q", ErrInvalidEvent, res)
	}

	sc, hasScope := tenantscope.FromContext(ctx)
	actor, err := actorOf(ctx, ev, sc, hasScope)
	if err != nil {
		return err
	}
	row := repository.Row{
		ActorType: actor.Type, ActorLabel: trunc(actor.Label, 255), Action: string(action), ResourceType: ev.ResourceType, Result: string(res),
		CreatedAt: w.clk.Now(),
	}
	if actor.UserID > 0 {
		row.ActorUserID = &actor.UserID
	}
	if actor.TokenID > 0 {
		row.ActorTokenID = &actor.TokenID
	}
	if actor.TenantID > 0 {
		row.ActorTenantID = &actor.TenantID
	}
	// 被操作对象所属租户：事件明确给出 > 租户范围 Scope 的租户（含 acting-as 的目标租户）> 空（平台级对象）。
	switch {
	case ev.TenantID > 0:
		row.TenantID = &ev.TenantID
	case hasScope && !sc.IsPlatform() && sc.TenantID() > 0:
		t := sc.TenantID()
		row.TenantID = &t
	}
	// acting_as：Scope 钩子登记的审计上下文或 Scope 自身标记（二者任一）。
	_, acting := httpx.ActingAsFrom(ctx)
	row.ActingAs = acting || (hasScope && sc.ActingAs())
	if ev.ResourceUID != "" {
		u := ev.ResourceUID
		row.ResourceUID = &u
	}
	if row.UID, err = w.ids.New(); err != nil {
		return fmt.Errorf("audit: 生成 uid: %w", err)
	}
	if id, ok := requestid.FromContext(ctx); ok && uid.Valid(id) {
		row.RequestID = id
	} else if row.RequestID, err = w.ids.New(); err != nil { // 非 HTTP 来源（后台作业）：无请求 ID 时新生成，保证可检索
		return fmt.Errorf("audit: 生成 request_id: %w", err)
	}
	row.IP = ipBytes(httpx.ClientIPFrom(ctx))
	if ua := httpx.UserAgentFrom(ctx); ua != "" {
		ua = trunc(ua, 512)
		row.UserAgent = &ua
	}
	if row.ChangesJSON, err = redactedJSON(ev.Changes); err != nil {
		return err
	}
	if row.MetaJSON, err = redactedJSON(ev.Meta); err != nil {
		return err
	}
	if err := w.repo.Insert(ctx, sc, row); err != nil {
		return fmt.Errorf("audit: 写入审计记录: %w", err)
	}
	httpx.NoteAudited(ctx)
	return nil
}

func (w *Writer) actionsFor(op string) []Action {
	if a := ActionsFor(op); a != nil {
		return a
	}
	for _, o := range w.extra {
		if o.OperationID == op {
			return o.Actions
		}
	}
	return nil
}

func (w *Writer) extraAction(a Action) bool {
	for _, o := range w.extra {
		if hasAction(o.Actions, a) {
			return true
		}
	}
	return false
}

func hasAction(list []Action, a Action) bool {
	for _, x := range list {
		if x == a {
			return true
		}
	}
	return false
}

func actorOf(ctx context.Context, ev Event, sc tenantscope.Scope, hasScope bool) (Actor, error) {
	if ev.Actor != nil {
		a := *ev.Actor
		if a.Type == "" || a.Label == "" {
			return Actor{}, fmt.Errorf("%w: Actor.Type/Label", ErrInvalidEvent)
		}
		return a, nil
	}
	if p, ok := httpx.PrincipalFrom(ctx); ok {
		if ap, ok := p.(ActorProvider); ok {
			typ, uidv, tok, label, tid := ap.AuditActor()
			if typ != "" && label != "" {
				return Actor{Type: typ, UserID: uidv, TokenID: tok, Label: label, TenantID: tid}, nil
			}
		}
		// 主体没有实现 ActorProvider：退化为 Scope 里的操作者 id（仍可归因，标签为空会被拒绝）。
		return Actor{}, fmt.Errorf("%w: 主体 %T 未实现 audit.ActorProvider", ErrNoActor, p)
	}
	if hasScope && sc.ActorID() > 0 {
		return Actor{}, fmt.Errorf("%w: 只有 Scope 操作者 id，缺少操作者快照", ErrNoActor)
	}
	return Actor{}, ErrNoActor
}

func redactedJSON(m map[string]any) (*string, error) {
	if len(m) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(redact.Value(m))
	if err != nil {
		return nil, fmt.Errorf("%w: 变更摘要无法序列化: %v", ErrInvalidEvent, err)
	}
	if len(b) > maxChangesBytes {
		b = []byte(`{"truncated":true}`)
	}
	s := string(b)
	return &s, nil
}

func ipBytes(s string) []byte {
	ip := net.ParseIP(s)
	if ip == nil {
		return nil
	}
	if v4 := ip.To4(); v4 != nil {
		return v4
	}
	return ip.To16()
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 { // 不截断 UTF-8 多字节序列
		n--
	}
	return s[:n]
}

// Missing 返回「声明了审计的成功请求却没有审计行」的累计次数（运行期兜底的计数；正常运行恒为 0）。
func (w *Writer) Missing() int64 { return w.missing.Load() }

// Auditor 返回 httpx.Auditor 的真实实现。
func (w *Writer) Auditor() httpx.Auditor { return auditor{w: w} }

type auditor struct{ w *Writer }

var _ httpx.Auditor = auditor{}

// Begin 实现 httpx.Auditor：Audit=false 的路由不处理；Audit=true 的路由返回结束回调——请求成功（2xx）、非幂等重放、未 panic，
// 却没有任何审计行（AuditOutcome.Recorded==0）即「声明了审计却没有记录」：记 Error 日志并计数。审计行本身由业务 Service 在业务事务内写入。
func (a auditor) Begin(r *http.Request, meta httpx.RouteMeta) (func(httpx.AuditOutcome), error) {
	if !meta.Audit {
		return nil, nil
	}
	if a.w.actionsFor(meta.OperationID) == nil {
		// 未在 Catalog 登记的 operation 不可能有合法的审计记录者：fail-closed，拒绝执行（500），而不是放行后才发现。
		return nil, apierr.Internal(fmt.Errorf("%w: operation %s 声明了 x-audit 但未在 audit.Catalog 登记", ErrUnknownAction, meta.OperationID))
	}
	return func(o httpx.AuditOutcome) {
		if o.Replayed || o.Panicked || o.Status < 200 || o.Status > 299 || o.Recorded > 0 {
			return
		}
		a.w.missing.Add(1)
		reqID, _ := requestid.FromContext(r.Context())
		a.w.log.Error("声明了审计（x-audit: true）的请求成功返回，却没有任何审计记录（违反 FR-23；业务 Service 必须在事务内调用 audit.Recorder.Record）",
			zap.String("operation_id", meta.OperationID), zap.Int("status", o.Status), zap.String("request_id", reqID))
		if a.w.onMissing != nil {
			a.w.onMissing(meta.OperationID)
		}
	}, nil
}
