package requestid

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/uid"
)

const goodID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

func testGen() *uid.Generator {
	return uid.NewGenerator(clock.Func(func() time.Time { return time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC) }))
}

type failGen struct{}

func (failGen) New() (string, error) { return "", errors.New("熵源故障") }

func TestResolveTable(t *testing.T) {
	cases := []struct {
		name     string
		incoming string
		want     string // 空串=应生成新值
	}{
		{"合法 ULID 沿用", goodID, goodID},
		{"小写规整为大写", strings.ToLower(goodID), goodID},
		{"缺失则生成", "", ""},
		{"非 ULID（UUID）丢弃", "123e4567-e89b-12d3-a456-426614174000", ""},
		{"过长丢弃", goodID + "X", ""},
		{"含换行（日志注入）丢弃", goodID[:20] + "\r\nX-Evil: 1", ""},
		{"含空白丢弃", " " + goodID, ""},
		{"128 位溢出首位丢弃", "81ARZ3NDEKTSV4RRFFQ69G5FAV", ""},
	}
	for _, tc := range cases {
		got, err := Resolve(tc.incoming, testGen())
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !uid.Valid(got) {
			t.Errorf("%s: 结果不是规范 ULID: %q", tc.name, got)
		}
		if tc.want != "" && got != tc.want {
			t.Errorf("%s: got %q，期望 %q", tc.name, got, tc.want)
		}
		if tc.want == "" && got == tc.incoming {
			t.Errorf("%s: 非法入站值不得被沿用: %q", tc.name, got)
		}
	}
	if _, err := Resolve("", failGen{}); err == nil {
		t.Error("生成器失败应向上返回错误")
	}
}

func TestContextRoundTripRejectsNonULID(t *testing.T) {
	ctx := context.Background()
	if _, ok := FromContext(ctx); ok {
		t.Fatal("空上下文不应有 Request ID")
	}
	ctx2, ok := NewContext(ctx, strings.ToLower(goodID))
	if !ok {
		t.Fatal("合法 ID 应可写入")
	}
	if got, ok := FromContext(ctx2); !ok || got != goodID {
		t.Errorf("FromContext=%q,%v，期望规整后的 %q", got, ok, goodID)
	}
	for _, bad := range []string{"", "abc", "x\ny", goodID + "0"} {
		c, ok := NewContext(ctx, bad)
		if ok || c != ctx {
			t.Errorf("非法 ID %q 必须被拒绝且不改变上下文", bad)
		}
	}
}

func TestEnsureKeepsExistingID(t *testing.T) {
	ctx, _ := NewContext(context.Background(), goodID)
	ctx2, id, err := Ensure(ctx, "01BX5ZZKBKACTAV9WEVGEMMVRZ", failGen{}) // 已有 ID：不读 incoming、不调用生成器
	if err != nil || id != goodID || ctx2 != ctx {
		t.Fatalf("Ensure 应保留已有 ID: id=%q err=%v", id, err)
	}
	ctx3, id3, err := Ensure(context.Background(), "", testGen())
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := FromContext(ctx3); got != id3 || !uid.Valid(id3) {
		t.Errorf("Ensure 应写入新生成的 ID: ctx=%q ret=%q", got, id3)
	}
	if _, _, err := Ensure(context.Background(), "", failGen{}); err == nil {
		t.Error("生成失败应返回错误")
	}
}

func TestHTTPMiddleware(t *testing.T) {
	var seen string
	h := Middleware(testGen())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = FromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))

	cases := []struct {
		name, header string
		wantSame     bool
	}{
		{"沿用合法入站值", goodID, true},
		{"缺失则生成", "", false},
		{"非法入站值被替换", "evil\r\nSet-Cookie: x=1", false},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		if tc.header != "" {
			req.Header[http.CanonicalHeaderKey(Header)] = []string{tc.header} // 绕过 Header.Set 的值检查，模拟恶意原始值
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		got := rec.Header().Get(Header)
		if !uid.Valid(got) {
			t.Errorf("%s: 响应头必须返回规范 ULID，实际 %q", tc.name, got)
		}
		if seen != got {
			t.Errorf("%s: handler 上下文中的 ID %q 应与响应头 %q 一致", tc.name, seen, got)
		}
		if (got == tc.header) != tc.wantSame {
			t.Errorf("%s: 沿用判断不符，入站 %q 响应 %q", tc.name, tc.header, got)
		}
		if rec.Header().Get("Set-Cookie") != "" {
			t.Errorf("%s: 入站恶意值不得进入响应头", tc.name)
		}
	}

	// 生成失败：500、不调用下游、响应体不含原因。
	called := false
	bad := Middleware(failGen{})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	rec := httptest.NewRecorder()
	bad.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusInternalServerError || called {
		t.Errorf("生成失败应 500 且不调用 next: code=%d called=%v", rec.Code, called)
	}
	if strings.Contains(rec.Body.String(), "熵源") {
		t.Errorf("响应体泄漏原因: %q", rec.Body.String())
	}
	var body apierr.Body
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != "internal.error" {
		t.Errorf("生成失败应返回统一错误体: %v %s", err, rec.Body.String())
	}
}
