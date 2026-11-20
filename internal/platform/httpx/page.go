package httpx

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
)

// 分页约定（ADR-017 §5、openapi 的 Page/PageSize/Cursor/Limit/Sort 参数）。
const (
	DefaultPage     = 1
	DefaultPageSize = 20
	MaxPageSize     = 200
	DefaultLimit    = 50
	MaxLimit        = 200
	maxCursorLen    = 512
	// MaxOffset 是 page 分页允许的最大 OFFSET（行数）。
	MaxOffset = 10_000_000
)

// PageParams 是 page/page_size 分页参数（已校验）。
type PageParams struct {
	Page     int
	PageSize int
}

// Offset 是 SQL OFFSET。
func (p PageParams) Offset() int { return (p.Page - 1) * p.PageSize }

// Limit 是 SQL LIMIT。
func (p PageParams) Limit() int { return p.PageSize }

// ParsePage 解析 page（≥1，默认 1）与 page_size（1–200，默认 20）；非法值一律 400 validation.failed，不静默纠正。
func ParsePage(q url.Values) (PageParams, error) {
	p := PageParams{Page: DefaultPage, PageSize: DefaultPageSize}
	var ds []apierr.Detail
	if v, ok := q["page"]; ok {
		n, err := parseInt(v)
		switch {
		case err != nil || n < 1:
			ds = append(ds, apierr.Detail{Field: "page", Reason: "must be an integer >= 1"})
		case n > 1<<31-1:
			ds = append(ds, apierr.Detail{Field: "page", Reason: "too large"})
		default:
			p.Page = int(n)
		}
	}
	if v, ok := q["page_size"]; ok {
		n, err := parseInt(v)
		if err != nil || n < 1 || n > MaxPageSize {
			ds = append(ds, apierr.Detail{Field: "page_size", Reason: "must be an integer between 1 and " + strconv.Itoa(MaxPageSize)})
		} else {
			p.PageSize = int(n)
		}
	}
	if len(ds) > 0 {
		return PageParams{}, apierr.Validation(ds...)
	}
	// 深翻页会把代价转嫁给数据库（OFFSET 线性扫描）：超过 MaxOffset 请改用游标分页。
	if int64(p.Page-1)*int64(p.PageSize) > MaxOffset {
		return PageParams{}, apierr.Validation(apierr.Detail{Field: "page", Reason: "page is too deep; use cursor pagination for large collections"})
	}
	return p, nil
}

// CursorParams 是 cursor/limit 游标分页参数（已校验）；Cursor 对本包不透明，由资源自己的游标编码解释。
type CursorParams struct {
	Cursor string
	Limit  int
}

// ParseCursor 解析 cursor（可空，≤512 字符，不含控制字符）与 limit（1–200，默认 50）。
func ParseCursor(q url.Values) (CursorParams, error) {
	p := CursorParams{Limit: DefaultLimit}
	var ds []apierr.Detail
	if v, ok := q["cursor"]; ok {
		c := ""
		if len(v) > 0 {
			c = v[0]
		}
		if len(v) != 1 || len(c) > maxCursorLen || hasControl(c) {
			ds = append(ds, apierr.Detail{Field: "cursor", Reason: "invalid cursor"})
		} else {
			p.Cursor = c
		}
	}
	if v, ok := q["limit"]; ok {
		n, err := parseInt(v)
		if err != nil || n < 1 || n > MaxLimit {
			ds = append(ds, apierr.Detail{Field: "limit", Reason: "must be an integer between 1 and " + strconv.Itoa(MaxLimit)})
		} else {
			p.Limit = int(n)
		}
	}
	if len(ds) > 0 {
		return CursorParams{}, apierr.Validation(ds...)
	}
	return p, nil
}

// Sort 是已通过白名单校验的排序。
type Sort struct {
	Field string // 白名单里的字段名（调用方据此映射到列，绝不把它拼进 SQL）
	Desc  bool
}

// ParseSort 解析 sort（`field` 升序、`-field` 降序，单字段）。allowed 是该资源的白名单字段（调用方声明）；
// 未传 sort 时返回 def；字段不在白名单（含空串、多字段、大小写不符）→ 400 validation.failed。
func ParseSort(q url.Values, allowed []string, def Sort) (Sort, error) {
	v, ok := q["sort"]
	if !ok {
		return def, nil
	}
	bad := func() (Sort, error) {
		return Sort{}, apierr.Validation(apierr.Detail{Field: "sort", Reason: "must be one of: " + strings.Join(allowed, ", ") + " (prefix - for descending)"})
	}
	if len(v) != 1 {
		return bad()
	}
	s := Sort{Field: v[0]}
	if strings.HasPrefix(s.Field, "-") {
		s.Desc, s.Field = true, s.Field[1:]
	}
	for _, a := range allowed {
		if s.Field == a {
			return s, nil
		}
	}
	return bad()
}

// PageResult 是 {items,page,page_size,total}。
type PageResult[T any] struct {
	Items    []T   `json:"items"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
	Total    int64 `json:"total"`
}

// NewPageResult 构造分页响应；items 为 nil 时输出 []（而非 null）。
func NewPageResult[T any](items []T, p PageParams, total int64) PageResult[T] {
	if items == nil {
		items = []T{}
	}
	return PageResult[T]{Items: items, Page: p.Page, PageSize: p.PageSize, Total: total}
}

// CursorResult 是 {items,next_cursor}；next_cursor 为 null 表示已到末尾（openapi CursorMeta）。
type CursorResult[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

// NewCursorResult 构造游标响应；next 为空串表示没有下一页。
func NewCursorResult[T any](items []T, next string) CursorResult[T] {
	if items == nil {
		items = []T{}
	}
	r := CursorResult[T]{Items: items}
	if next != "" {
		r.NextCursor = &next
	}
	return r
}

func parseInt(v []string) (int64, error) {
	if len(v) != 1 {
		return 0, strconv.ErrSyntax
	}
	return strconv.ParseInt(v[0], 10, 64)
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
