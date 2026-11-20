// Package apierr 是统一错误模型（Story 1.4 AC3、ADR-017 §3、architecture §9.2）。
//
// 业务代码返回 *Error（或用 %w 包装它）；HTTP 入口用 Render/ToBody/Write 把任意 error 转成
// {"error":{"code","message","message_key","details":[{field,reason}],"request_id"}} 与状态码：
//   - code 为稳定的小写点分字符串，状态码来自与 docs/api/README.md §5 逐项核对的映射表；
//   - 5xx（internal.error）一律返回固定的通用消息、不带 details，原因只留在错误链里供日志使用——响应永不泄漏内部原因；
//   - 4xx 的 message/details/message_key 同样经 redact 清洗（纵深防御：开发者误把凭据写进消息也不会出现在响应里；
//     代价是形如 "password: …" 的自由文本消息会被遮蔽，字段名与原因请分别放入 details 的 field/reason）；
//   - 未知错误（非 *Error）、未登记的 code、无 HTTP 状态行的 code 一律按 internal.error 返回。
package apierr

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/wuekevin/lingedge/pkg/redact"
)

// Status 返回 code 对应的 HTTP 状态；未登记或无 HTTP 状态行（task.failed、enroll.token_invalid）返回 500。
func Status(c Code) int {
	if s := table[c]; s != 0 {
		return s
	}
	return http.StatusInternalServerError
}

// Known 报告 code 是否在错误码索引中（含无 HTTP 状态行的码）。
func Known(c Code) bool { _, ok := table[c]; return ok }

// HasHTTPStatus 报告 code 是否可经 REST 状态行返回。
func HasHTTPStatus(c Code) bool { return table[c] != 0 }

// Codes 返回全部已登记错误码（升序）。
func Codes() []Code {
	out := make([]Code, 0, len(table))
	for c := range table {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

var codeSyntax = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

// ValidSyntax 报告 code 是否满足「小写点分」语法（至少两段，段内小写字母/数字/下划线）。
func ValidSyntax(c Code) bool { return codeSyntax.MatchString(string(c)) }

// Detail 是逐字段的错误说明（OpenAPI ErrorDetail）。
type Detail struct {
	Field  string `json:"field,omitempty"`
	Reason string `json:"reason"`
	Code   string `json:"code,omitempty"` // 字段级稳定码（可选）
}

// 通用消息：5xx 响应固定使用，不含任何运行期信息。
const internalMessage = "internal error"

// Error 是业务错误。Message 是面向人的默认语言说明（不得含敏感信息）；cause 不进入响应。
type Error struct {
	Code       Code
	Message    string
	MessageKey string // 为空时取 "errors."+Code
	Details    []Detail
	RetryAfter time.Duration // >0 时响应带 Retry-After（429）

	cause error
}

// New 创建业务错误。
func New(code Code, message string, details ...Detail) *Error {
	return &Error{Code: code, Message: message, Details: details}
}

// Newf 是带格式化的 New。
func Newf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrap 给 cause 套上业务码；cause 仅用于日志与 errors.Is/As，不进入响应。
func Wrap(cause error, code Code, message string, details ...Detail) *Error {
	return &Error{Code: code, Message: message, Details: details, cause: cause}
}

// Validation 构造 400 validation.failed，details 逐字段说明（校验失败统一 400，不用 422）。
func Validation(details ...Detail) *Error {
	return &Error{Code: ValidationFailed, Message: "request validation failed", Details: details}
}

// NotFound 构造 404 resource.not_found（跨租户与不存在一律此码，不泄漏存在性）。
func NotFound() *Error {
	return &Error{Code: ResourceNotFound, Message: "resource not found"}
}

// Conflict 构造 409 resource.conflict（资源状态冲突：唯一性、状态不允许、被引用）。模块专属冲突码（如 site.domain_conflict）直接用 New。
func Conflict(message string, details ...Detail) *Error {
	return &Error{Code: ResourceConflict, Message: message, Details: details}
}

// PreconditionFailed 构造 412 resource.precondition_failed（If-Match 版本不符；乐观锁一律此码，不用 409，IMP-13）。
func PreconditionFailed() *Error {
	return &Error{Code: ResourcePreconditionFailed, Message: "precondition failed"}
}

// KeyReuse 构造 422 idempotency.key_reuse（同一 Idempotency-Key 用于不同请求；「校验统一 400」的唯一例外）。
func KeyReuse() *Error {
	return &Error{Code: IdempotencyKeyReuse, Message: "idempotency key was already used with a different request"}
}

// Internal 构造 500 internal.error；cause 只留在错误链里。
func Internal(cause error) *Error {
	return &Error{Code: InternalError, Message: internalMessage, cause: cause}
}

// Error 实现 error：仅含码与消息（不含 cause，避免经 %v 间接泄漏）。
func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// Unwrap 返回原因（供 errors.Is/As 与日志；绝不写入响应）。
func (e *Error) Unwrap() error { return e.cause }

// HTTPStatus 返回该错误的 HTTP 状态。
func (e *Error) HTTPStatus() int { return Status(e.Code) }

// WithDetails 返回追加了 details 的副本。
func (e *Error) WithDetails(d ...Detail) *Error {
	c := *e
	c.Details = append(append([]Detail(nil), e.Details...), d...)
	return &c
}

// WithRetryAfter 返回设置了 Retry-After 的副本（用于 429）。
func (e *Error) WithRetryAfter(d time.Duration) *Error {
	c := *e
	c.RetryAfter = d
	return &c
}

// WithMessageKey 返回覆盖 message_key 的副本（默认 errors.<code>）。
func (e *Error) WithMessageKey(k string) *Error {
	c := *e
	c.MessageKey = k
	return &c
}

// As 取出错误链上最外层的 *Error。
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) && e != nil {
		return e, true
	}
	return nil, false
}

// Payload 是响应体里的 error 对象（OpenAPI Error）。
type Payload struct {
	Code       string   `json:"code"`
	Message    string   `json:"message"`
	MessageKey string   `json:"message_key"`
	Details    []Detail `json:"details"`
	RequestID  string   `json:"request_id"`
}

// Body 是统一错误响应体（OpenAPI ErrorResponse）。
type Body struct {
	Error Payload `json:"error"`
}

// Response 是渲染结果：状态、头与体。
type Response struct {
	Status int
	Header http.Header
	Body   Body
}

// Render 把任意 error 渲染为响应。err 为 nil 视为内部错误（调用方 bug），不会 panic。
func Render(err error, requestID string) Response {
	e, ok := As(err)
	if !ok || !Known(e.Code) || !HasHTTPStatus(e.Code) {
		e = nil
	}
	if e == nil {
		return Response{Status: http.StatusInternalServerError, Header: http.Header{}, Body: newBody(InternalError, internalMessage, "", nil, requestID)}
	}
	status := e.HTTPStatus()
	h := http.Header{}
	if status >= 500 {
		return Response{Status: status, Header: h, Body: newBody(e.Code, internalMessage, "", nil, requestID)}
	}
	if e.RetryAfter > 0 {
		secs := int64(e.RetryAfter / time.Second) // 向上取整，避免 d+1s 在极大值时溢出
		if e.RetryAfter%time.Second != 0 {
			secs++
		}
		h.Set("Retry-After", strconv.FormatInt(secs, 10))
	}
	details := make([]Detail, len(e.Details))
	for i, d := range e.Details {
		details[i] = Detail{Field: redact.String(d.Field), Reason: redact.String(d.Reason), Code: redact.String(d.Code)}
	}
	return Response{Status: status, Header: h, Body: newBody(e.Code, redact.String(e.Message), redact.String(e.MessageKey), details, requestID)}
}

func newBody(code Code, msg, key string, details []Detail, requestID string) Body {
	if key == "" {
		key = "errors." + string(code)
	}
	if details == nil {
		details = []Detail{}
	}
	return Body{Error: Payload{Code: string(code), Message: msg, MessageKey: key, Details: details, RequestID: requestID}}
}

// ToBody 返回状态码与响应体（不含头；需要 Retry-After 时用 Render）。
func ToBody(err error, requestID string) (int, Body) {
	r := Render(err, requestID)
	return r.Status, r.Body
}

// Write 把 err 渲染到 net/http 响应（Content-Type: application/json）。X-Request-ID 响应头由 requestid 中间件负责。
func Write(w http.ResponseWriter, err error, requestID string) {
	r := Render(err, requestID)
	for k, v := range r.Header {
		w.Header()[k] = v
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(r.Status)
	_ = json.NewEncoder(w).Encode(r.Body) // 写失败时连接已不可用，无可补救
}
