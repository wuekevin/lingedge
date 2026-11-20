package httpx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/idempotency"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
)

// IdempotencyKeyHeader 是幂等键请求头；IdempotentReplayHeader 在重放响应上为 "true"。
const (
	IdempotencyKeyHeader   = "Idempotency-Key"
	IdempotentReplayHeader = "Idempotent-Replayed"
)

const taskIDKey = "lingedge.idempotency.task_id"
const replayBodyKey = "lingedge.idempotency.replay_body"

// SetReplayBody 登记「幂等记录里保存的响应体」，取代真实响应体：响应里含一次性秘密（如服务端生成的临时口令，只回显一次）的 handler
// 必须登记去除了秘密的响应体——否则幂等记录会把秘密明文写进 MySQL，且同键重放会再次回显它。首次响应仍是真实响应体。
func SetReplayBody(c *gin.Context, body []byte) { c.Set(replayBodyKey, append([]byte(nil), body...)) }

// SetTaskID 由创建 Task 的 handler 登记「本次响应关联的 tasks.id」，随首次响应一并存入 idempotency_keys.task_id（0 表示无）。
func SetTaskID(c *gin.Context, id uint64) {
	if id != 0 {
		c.Set(taskIDKey, id)
	}
}

// 响应封装里保留的响应头（其余一概不缓存：Set-Cookie、X-Request-ID 等都属于「这一次」请求）。
var replayHeaders = []string{"Content-Type", "Location", "ETag"}

type envelope struct {
	Headers map[string][]string `json:"headers"`
	Body    []byte              `json:"body"`
}

func idempotencyMiddleware(store idempotency.Store, m *Metrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		vals := c.Request.Header.Values(IdempotencyKeyHeader)
		if len(vals) == 0 {
			c.Next() // 幂等键是可选的
			return
		}
		key, err := validateKey(vals)
		if err != nil {
			Fail(c, err)
			return
		}
		p, ok := PrincipalFrom(c.Request.Context())
		if !ok { // Register 已拒绝公开路由 + Idempotent；链被错误组装时 fail-closed
			Fail(c, apierr.New(apierr.AuthUnauthenticated, "authentication required"))
			return
		}
		path := c.Request.URL.Path
		if len(path) > idempotency.MaxPathLen {
			Fail(c, apierr.Validation(apierr.Detail{Field: "path", Reason: "request path too long for idempotent requests"}))
			return
		}
		body, err := readBody(c)
		if err != nil {
			Fail(c, err)
			return
		}
		req := idempotency.Request{
			PrincipalKey: principalKey2(c.Request.Context(), p),
			Key:          key,
			Method:       c.Request.Method,
			Path:         path,
			Hash:         requestHash(c.Request.Method, path, c.Request.URL.Query(), body),
		}
		out, err := store.Begin(c.Request.Context(), req)
		if err != nil {
			Fail(c, apierr.Internal(err)) // 存储不可用：不得在没有幂等保护的情况下执行创建类请求
			return
		}
		switch out.Result {
		case idempotency.Mismatch:
			m.reuses.Add(1)
			Fail(c, apierr.KeyReuse())
		case idempotency.InFlight:
			Fail(c, apierr.Conflict("a request with this Idempotency-Key is still being processed").WithRetryAfter(time.Second))
		case idempotency.Replay:
			if replay(c, out) {
				m.replays.Add(1)
			}
		case idempotency.Acquired:
			runAndRecord(c, store, out.ID)
		default:
			Fail(c, apierr.Internal(errUnknownResult))
		}
	}
}

func validateKey(vals []string) (string, error) {
	bad := func(reason string) error {
		return apierr.Validation(apierr.Detail{Field: IdempotencyKeyHeader, Reason: reason})
	}
	if len(vals) != 1 {
		return "", bad("must be provided at most once")
	}
	k := vals[0]
	if k == "" || len(k) > idempotency.MaxKeyLen {
		return "", bad("must be 1 to " + strconv.Itoa(idempotency.MaxKeyLen) + " characters")
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x21 || k[i] > 0x7e {
			return "", bad("must contain only visible ASCII characters")
		}
	}
	return k, nil
}

// principalKey2 构造 `<platform|tenant>:<actor>:<tenant>`（database/platform-identity §4.4）：租户取 Scope（代租户时是目标租户）。
func principalKey2(ctx context.Context, p tenantscope.Principal) string {
	kind := "tenant"
	if p.IsPlatform() {
		kind = "platform"
	}
	var tenant uint64
	if s, ok := tenantscope.FromContext(ctx); ok {
		tenant = s.TenantID()
	}
	return kind + ":" + strconv.FormatUint(p.ActorID(), 10) + ":" + strconv.FormatUint(tenant, 10)
}

func requestHash(method, path string, q url.Values, body []byte) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{'\n'})
	h.Write([]byte(path))
	h.Write([]byte{'\n'})
	h.Write([]byte(q.Encode())) // Encode 按键排序，规范化参数顺序
	h.Write([]byte{'\n'})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

func replay(c *gin.Context, out idempotency.Outcome) bool {
	var env envelope
	if err := json.Unmarshal(out.Body, &env); err != nil || out.Status < 200 || out.Status > 599 {
		Fail(c, apierr.Internal(errCorruptReplay))
		return false
	}
	for _, name := range replayHeaders {
		if vs, ok := env.Headers[name]; ok {
			c.Writer.Header()[name] = vs
		}
	}
	c.Header(IdempotentReplayHeader, "true")
	c.Set(replayedKey, true)
	c.Writer.WriteHeader(out.Status)
	_, _ = c.Writer.Write(env.Body)
	c.Abort()
	return true
}

// bufferedWriter 把 handler 的响应先缓冲，待幂等记录写入后再放行（响应与记录一致：不会出现「客户端收到了、记录却没存下」之外的分歧）。
type bufferedWriter struct {
	gin.ResponseWriter
	buf    bytes.Buffer
	status int
}

func (w *bufferedWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}
func (w *bufferedWriter) WriteHeaderNow() {}
func (w *bufferedWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.buf.Write(b)
}
func (w *bufferedWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func (w *bufferedWriter) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}
func (w *bufferedWriter) Size() int     { return w.buf.Len() }
func (w *bufferedWriter) Written() bool { return w.status != 0 }
func (w *bufferedWriter) Flush()        {}

func runAndRecord(c *gin.Context, store idempotency.Store, id uint64) {
	orig := c.Writer
	bw := &bufferedWriter{ResponseWriter: orig}
	c.Writer = bw
	done := false
	defer func() {
		c.Writer = orig
		if !done { // handler panic：释放占位让客户端可重试，panic 继续向上交给 recover
			release(c, store, id)
		}
	}()
	c.Next()
	done = true
	c.Writer = orig

	status := bw.Status()
	if status >= 200 && status < 300 {
		env := envelope{Headers: map[string][]string{}, Body: bw.buf.Bytes()}
		if v, ok := c.Get(replayBodyKey); ok {
			env.Body, _ = v.([]byte)
		}
		for _, name := range replayHeaders {
			if vs, ok := orig.Header()[name]; ok {
				env.Headers[name] = append([]string(nil), vs...)
			}
		}
		raw, err := json.Marshal(env)
		var taskID uint64
		if v, ok := c.Get(taskIDKey); ok {
			taskID, _ = v.(uint64)
		}
		if err == nil {
			err = detached(c, func(ctx context.Context) error { return store.Complete(ctx, id, status, raw, taskID) })
		}
		if err != nil {
			// 响应已经产生副作用：照常返回，但记录没存下 → 之后同键重放会被当作仍在执行，直到租约到期后重新执行；此处必须留痕。
			Logger(c).Error("幂等记录写入失败（响应照常返回）", zap.Error(err))
		}
	} else {
		release(c, store, id) // 失败响应不缓存：纠正后可用同一个键重试
	}
	// 放行缓冲的响应。
	orig.WriteHeader(status)
	_, _ = orig.Write(bw.buf.Bytes())
}

func release(c *gin.Context, store idempotency.Store, id uint64) {
	if err := detached(c, func(ctx context.Context) error { return store.Abandon(ctx, id) }); err != nil {
		Logger(c).Warn("释放幂等占位失败（占位将在租约到期后可被接管）", zap.Error(err))
	}
}

// detached 在脱离请求取消的上下文里收尾：客户端断开不应让幂等记录的写入半途而废。
func detached(c *gin.Context, fn func(ctx context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 5*time.Second)
	defer cancel()
	return fn(ctx)
}

type constErr string

func (e constErr) Error() string { return string(e) }

const (
	errUnknownResult constErr = "httpx: 幂等存储返回未知结果"
	errCorruptReplay constErr = "httpx: 幂等记录的响应封装损坏"
)
