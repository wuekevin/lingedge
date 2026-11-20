package httpx_test

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
)

// q 逐段解码查询串（不像 url.ParseQuery 那样拒绝分号，以便测试注入样本）。
func q(s string) url.Values {
	v := url.Values{}
	if s == "" {
		return v
	}
	for _, kv := range strings.Split(s, "&") {
		k, val, _ := strings.Cut(kv, "=")
		dk, _ := url.QueryUnescape(k)
		dv, _ := url.QueryUnescape(val)
		v[dk] = append(v[dk], dv)
	}
	return v
}

func wantValidation(t *testing.T, err error, field string) {
	t.Helper()
	e, ok := apierr.As(err)
	if !ok || e.Code != apierr.ValidationFailed {
		t.Fatalf("期望 400 validation.failed，实际 %v", err)
	}
	for _, d := range e.Details {
		if d.Field == field {
			return
		}
	}
	t.Fatalf("details 应指出字段 %q: %+v", field, e.Details)
}

// AC4：page/page_size 默认 20、最大 200；边界。
func TestParsePageBoundaries(t *testing.T) {
	p, err := httpx.ParsePage(q(""))
	if err != nil || p.Page != 1 || p.PageSize != 20 || p.Offset() != 0 || p.Limit() != 20 {
		t.Fatalf("默认值: %+v %v", p, err)
	}
	for _, c := range []struct {
		q          string
		page, size int
		offset     int
	}{
		{"page=1&page_size=1", 1, 1, 0},
		{"page=3&page_size=200", 3, 200, 400},
		{"page=50001&page_size=200", 50001, 200, 10000000},
		{"page=10000001&page_size=1", 10000001, 1, 10000000},
	} {
		p, err := httpx.ParsePage(q(c.q))
		if err != nil || p.Page != c.page || p.PageSize != c.size || p.Offset() != c.offset {
			t.Errorf("%s: %+v %v", c.q, p, err)
		}
	}
	for _, c := range []struct{ q, field string }{
		{"page=0", "page"}, {"page=-1", "page"}, {"page=abc", "page"}, {"page=", "page"}, {"page=1.5", "page"}, {"page=1&page=2", "page"},
		{"page=2147483648", "page"}, {"page=99999999999999999999", "page"},
		{"page_size=0", "page_size"}, {"page_size=201", "page_size"}, {"page_size=-5", "page_size"}, {"page_size=x", "page_size"}, {"page_size=1&page_size=2", "page_size"},
	} {
		_, err := httpx.ParsePage(q(c.q))
		wantValidation(t, err, c.field)
	}
	// 两个参数都非法时一并报告。
	_, err = httpx.ParsePage(q("page=0&page_size=999"))
	if e, _ := apierr.As(err); e == nil || len(e.Details) != 2 {
		t.Errorf("应同时报告两个字段: %v", err)
	}
	// OFFSET 过大（深翻页）被拒绝，避免对数据库的放大攻击。
	for _, deep := range []string{"page=50002&page_size=200", "page=10000002&page_size=1", "page=2147483647&page_size=200"} {
		_, err = httpx.ParsePage(q(deep))
		wantValidation(t, err, "page")
	}
}

func TestParseCursorBoundaries(t *testing.T) {
	c, err := httpx.ParseCursor(q(""))
	if err != nil || c.Limit != 50 || c.Cursor != "" {
		t.Fatalf("默认: %+v %v", c, err)
	}
	c, err = httpx.ParseCursor(q("cursor=abc_DEF-123&limit=200"))
	if err != nil || c.Cursor != "abc_DEF-123" || c.Limit != 200 {
		t.Fatalf("%+v %v", c, err)
	}
	if c, err = httpx.ParseCursor(q("limit=1")); err != nil || c.Limit != 1 {
		t.Fatalf("%+v %v", c, err)
	}
	for _, x := range []struct{ q, field string }{
		{"limit=0", "limit"}, {"limit=201", "limit"}, {"limit=x", "limit"}, {"limit=1&limit=2", "limit"},
		{"cursor=a&cursor=b", "cursor"}, {"cursor=" + strings.Repeat("a", 513), "cursor"}, {"cursor=a%0Ab", "cursor"},
	} {
		_, err := httpx.ParseCursor(q(x.q))
		wantValidation(t, err, x.field)
	}
	if _, err := httpx.ParseCursor(q("cursor=" + strings.Repeat("a", 512))); err != nil {
		t.Errorf("512 字符游标应被接受: %v", err)
	}
}

func TestParseSortWhitelist(t *testing.T) {
	allowed := []string{"created_at", "name"}
	def := httpx.Sort{Field: "created_at", Desc: true}
	if s, err := httpx.ParseSort(q(""), allowed, def); err != nil || s != def {
		t.Fatalf("默认排序: %+v %v", s, err)
	}
	if s, err := httpx.ParseSort(q("sort=name"), allowed, def); err != nil || s != (httpx.Sort{Field: "name"}) {
		t.Fatalf("%+v %v", s, err)
	}
	if s, err := httpx.ParseSort(q("sort=-name"), allowed, def); err != nil || s != (httpx.Sort{Field: "name", Desc: true}) {
		t.Fatalf("%+v %v", s, err)
	}
	for _, bad := range []string{"sort=password_hash", "sort=", "sort=-", "sort=Name", "sort=name,created_at", "sort=name;DROP TABLE users", "sort=--name", "sort=name&sort=created_at", "sort=created_at%20desc", "sort=+name"} {
		_, err := httpx.ParseSort(q(bad), allowed, def)
		wantValidation(t, err, "sort")
	}
	// 白名单为空：任何显式 sort 都被拒绝。
	if _, err := httpx.ParseSort(q("sort=name"), nil, def); err == nil {
		t.Error("空白名单应拒绝任何 sort")
	}
}

func TestPageAndCursorResultShape(t *testing.T) {
	b, _ := json.Marshal(httpx.NewPageResult[string](nil, httpx.PageParams{Page: 2, PageSize: 20}, 41))
	if string(b) != `{"items":[],"page":2,"page_size":20,"total":41}` {
		t.Errorf("分页响应: %s", b)
	}
	b, _ = json.Marshal(httpx.NewCursorResult([]int{1}, ""))
	if string(b) != `{"items":[1],"next_cursor":null}` {
		t.Errorf("末页游标应为 null: %s", b)
	}
	b, _ = json.Marshal(httpx.NewCursorResult[int](nil, "abc"))
	if string(b) != `{"items":[],"next_cursor":"abc"}` {
		t.Errorf("%s", b)
	}
}
