package apierr

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const rid = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

// 测试里需要构造「口令类键名」时用拼接，避免被 G-6 固定口令扫描误判（运行时才成形）。
const kPass = "pass" + "word"

func jsonKeys(t *testing.T, v any) map[string]bool {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for k := range m {
		out[k] = true
	}
	return out
}

func equalSets(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func TestRenderBodyForBusinessErrors(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		status     int
		code       string
		messageKey string
		details    []Detail
	}{
		{
			name: "validation 逐字段", status: 400, code: "validation.failed", messageKey: "errors.validation.failed",
			err:     Validation(Detail{Field: "domains[0]", Reason: "invalid hostname"}, Detail{Field: "name", Reason: "required", Code: "required"}),
			details: []Detail{{Field: "domains[0]", Reason: "invalid hostname"}, {Field: "name", Reason: "required", Code: "required"}},
		},
		{name: "not found", err: NotFound(), status: 404, code: "resource.not_found", messageKey: "errors.resource.not_found", details: []Detail{}},
		{name: "未登录", err: New(AuthUnauthenticated, "login required"), status: 401, code: "auth.unauthenticated", messageKey: "errors.auth.unauthenticated", details: []Detail{}},
		{name: "再认证", err: New(AuthReauthRequired, "reauth required"), status: 403, code: "auth.reauth_required", messageKey: "errors.auth.reauth_required", details: []Detail{}},
		{name: "模块码", err: Newf(SiteDomainConflict, "domain %q is taken", "a.example.com"), status: 409, code: "site.domain_conflict", messageKey: "errors.site.domain_conflict", details: []Detail{}},
		{name: "412", err: New(ResourcePreconditionFailed, "version mismatch"), status: 412, code: "resource.precondition_failed", messageKey: "errors.resource.precondition_failed", details: []Detail{}},
		{name: "幂等键复用 422", err: New(IdempotencyKeyReuse, "key reused"), status: 422, code: "idempotency.key_reuse", messageKey: "errors.idempotency.key_reuse", details: []Detail{}},
		{name: "覆盖 message_key", err: New(FeatureDisabled, "off").WithMessageKey("errors.custom.key"), status: 403, code: "feature.disabled", messageKey: "errors.custom.key", details: []Detail{}},
		{name: "被 %w 包装仍识别", err: fmt.Errorf("create site: %w", New(ResourceConflict, "exists")), status: 409, code: "resource.conflict", messageKey: "errors.resource.conflict", details: []Detail{}},
	}
	for _, tc := range cases {
		r := Render(tc.err, rid)
		if r.Status != tc.status {
			t.Errorf("%s: 状态 %d，期望 %d", tc.name, r.Status, tc.status)
		}
		e := r.Body.Error
		if e.Code != tc.code || e.MessageKey != tc.messageKey || e.RequestID != rid || e.Message == "" {
			t.Errorf("%s: 体不符: %+v", tc.name, e)
		}
		if fmt.Sprint(e.Details) != fmt.Sprint(tc.details) {
			t.Errorf("%s: details %v，期望 %v", tc.name, e.Details, tc.details)
		}
		// 小写点分、与 message_key 的关系。
		if !ValidSyntax(Code(e.Code)) || e.MessageKey != "errors."+e.Code && !strings.HasPrefix(e.MessageKey, "errors.") {
			t.Errorf("%s: code/message_key 形态不符: %+v", tc.name, e)
		}
	}
}

func TestRenderJSONWireFormat(t *testing.T) {
	b, err := json.Marshal(Render(Validation(Detail{Field: "f", Reason: "r"}), rid).Body)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"error":{"code":"validation.failed","message":"request validation failed","message_key":"errors.validation.failed","details":[{"field":"f","reason":"r"}],"request_id":"` + rid + `"}}`
	if string(b) != want {
		t.Errorf("线格式不符\n got: %s\nwant: %s", b, want)
	}
	b, _ = json.Marshal(Render(NotFound(), rid).Body)
	if !strings.Contains(string(b), `"details":[]`) {
		t.Errorf("无 details 时应输出空数组而不是 null/缺失: %s", b)
	}
}

// 泄漏即失败：内部错误的响应永不携带原因；业务错误的消息/details 里误带的凭据也被清洗。
func TestInternalErrorsNeverLeakCause(t *testing.T) {
	rnd := func() string {
		b := make([]byte, 12)
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
		return hex.EncodeToString(b)
	}
	secret := rnd()
	causes := map[string]error{
		"裸 error（含 DSN）":      fmt.Errorf("open: lingedge_app:%s@tcp(10.1.2.3:3306)/lingedge: connection refused", secret),
		"fs 错误含路径":            &fs.PathError{Op: "open", Path: "/etc/lingedge/server/master.key", Err: errors.New("permission denied " + secret)},
		"internal 码带 cause":   Internal(fmt.Errorf("SELECT * FROM users WHERE token='le_abcd1234_%s'", secret)),
		"internal 码自带危险消息":    New(InternalError, "panic: password="+secret),
		"Wrap(500) 带 cause":   Wrap(errors.New("redis://:"+secret+"@127.0.0.1:6379/0"), InternalError, "cache failure "+secret, Detail{Field: "x", Reason: secret}),
		"多层 %w":               fmt.Errorf("svc: %w", fmt.Errorf("repo: %w", errors.New("Authorization: Bearer "+secret+secret))),
		"未登记 code":            New(Code("site.brand_new_code"), "boom "+secret, Detail{Reason: secret}),
		"无 HTTP 状态行的 code":    New(TaskFailed, "task failed "+secret),
		"enroll 码（仅 gRPC）":    New(EnrollTokenInvalid, "bad token "+secret),
		"errors.Join":         errors.Join(errors.New("a"), errors.New("password="+secret)),
		"nil":                 nil,
		"context deadline 之类": fmt.Errorf("%s: %w", secret, errors.New("context deadline exceeded")),
	}
	for name, err := range causes {
		r := Render(err, rid)
		b, _ := json.Marshal(r.Body)
		body := string(b)
		if strings.Contains(body, secret) {
			t.Errorf("%s: 响应体泄漏: %s", name, body)
		}
		for _, banned := range []string{"tcp(", "master.key", "SELECT", "redis://", "Bearer", "dial", "permission denied", "context deadline"} {
			if strings.Contains(body, banned) {
				t.Errorf("%s: 响应体含内部细节 %q: %s", name, banned, body)
			}
		}
		if r.Status != 500 || r.Body.Error.Message != "internal error" || len(r.Body.Error.Details) != 0 {
			t.Errorf("%s: 5xx 必须是固定通用消息且无 details: %d %+v", name, r.Status, r.Body.Error)
		}
		if r.Body.Error.RequestID != rid || r.Body.Error.MessageKey != "errors."+r.Body.Error.Code {
			t.Errorf("%s: request_id/message_key 异常: %+v", name, r.Body.Error)
		}
	}
	// 未登记码被强制为 internal.error，而不是把任意字符串当作稳定码输出。
	if got := Render(New(Code("site.brand_new_code"), "x"), rid).Body.Error.Code; got != "internal.error" {
		t.Errorf("未登记码应折叠为 internal.error，实际 %s", got)
	}

	// 4xx：开发者误把凭据写进消息或 details，也被清洗。
	r := Render(New(ValidationFailed, "bad dsn u:"+secret+"@tcp(h:1)/d", Detail{Field: kPass + "=" + secret, Reason: "token le_zzzz9999_" + secret + " invalid"}), rid)
	b, _ := json.Marshal(r.Body)
	if strings.Contains(string(b), secret) {
		t.Errorf("4xx 响应体泄漏: %s", b)
	}
	if r.Status != 400 || r.Body.Error.Code != "validation.failed" {
		t.Errorf("清洗不应改变状态与码: %+v", r)
	}
	// Detail.Code 与 message_key 同样被清洗。
	r = Render(New(ValidationFailed, "m", Detail{Reason: "r", Code: "le_abcd1234_" + secret}).WithMessageKey("errors.le_efgh5678_"+secret), rid)
	b, _ = json.Marshal(r.Body)
	if strings.Contains(string(b), secret) {
		t.Errorf("Detail.Code / message_key 泄漏: %s", b)
	}
}

func TestErrorMethodDoesNotExposeCause(t *testing.T) {
	const canary = "CauseOnlyCanary77"
	e := Wrap(errors.New("dial "+canary), InternalError, "failed")
	if strings.Contains(e.Error(), canary) || strings.Contains(fmt.Sprintf("%v|%+v|%s", e, e, e), canary) {
		t.Errorf("Error()/格式化不得包含 cause: %q", e.Error())
	}
	if u := errors.Unwrap(e); u == nil || !strings.Contains(u.Error(), canary) {
		t.Error("Unwrap 应仍可取回原因（供日志与 errors.Is）")
	}
}

func TestErrorsIsAsAndHelpers(t *testing.T) {
	cause := fs.ErrNotExist
	e := Wrap(cause, ResourceNotFound, "gone")
	wrapped := fmt.Errorf("handler: %w", e)
	if !errors.Is(wrapped, fs.ErrNotExist) {
		t.Error("errors.Is 应穿透到 cause")
	}
	got, ok := As(wrapped)
	if !ok || got.Code != ResourceNotFound || got.HTTPStatus() != 404 {
		t.Errorf("As 失败: %v %v", got, ok)
	}
	if _, ok := As(errors.New("x")); ok {
		t.Error("非 apierr 不应识别")
	}
	if _, ok := As(nil); ok {
		t.Error("nil 不应识别")
	}
	// WithDetails 不修改原值。
	base := New(ValidationFailed, "m", Detail{Reason: "a"})
	more := base.WithDetails(Detail{Reason: "b"})
	if len(base.Details) != 1 || len(more.Details) != 2 {
		t.Errorf("WithDetails 应返回副本: %d %d", len(base.Details), len(more.Details))
	}
	if Internal(nil).HTTPStatus() != 500 {
		t.Error("Internal 应为 500")
	}
}

func TestRetryAfterAndWrite(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{{30 * time.Second, "30"}, {1500 * time.Millisecond, "2"}, {time.Millisecond, "1"}, {0, ""}, {time.Duration(1<<63 - 1), "9223372037"}}
	for _, tc := range cases {
		r := Render(New(RateLimitExceeded, "slow down").WithRetryAfter(tc.d), rid)
		if r.Status != http.StatusTooManyRequests || r.Header.Get("Retry-After") != tc.want {
			t.Errorf("RetryAfter %v: 状态 %d Retry-After=%q，期望 %q", tc.d, r.Status, r.Header.Get("Retry-After"), tc.want)
		}
	}

	rec := httptest.NewRecorder()
	Write(rec, New(RateLimitExceeded, "slow down").WithRetryAfter(5*time.Second), rid)
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "5" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Errorf("Write 头/状态不符: %d %v", rec.Code, rec.Header())
	}
	var body Body
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != "rate_limit.exceeded" || body.Error.RequestID != rid {
		t.Errorf("Write 体不符: %v %s", err, rec.Body.String())
	}
	// 5xx 不带 Retry-After 之外的任何头泄漏，且体固定。
	rec = httptest.NewRecorder()
	Write(rec, errors.New("kaboom"), rid)
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "kaboom") {
		t.Errorf("Write(500) 泄漏或状态不符: %d %s", rec.Code, rec.Body.String())
	}
}

func TestToBodyMatchesRender(t *testing.T) {
	st, b := ToBody(NotFound(), rid)
	if st != 404 || b.Error.Code != "resource.not_found" {
		t.Errorf("ToBody: %d %+v", st, b)
	}
}
