package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/requestid"
)

const contentTypeJSON = "application/json; charset=utf-8"

// DefaultMaxBodyBytes 是请求体上限（BindJSON 与幂等中间件共用）。
const DefaultMaxBodyBytes int64 = 1 << 20

const (
	loggerKey  = "lingedge.logger"
	maxBodyKey = "lingedge.maxbody"
)

// Logger 返回带 request_id 的请求级 Logger（路由链之外调用时返回 Nop）。
func Logger(c *gin.Context) *zap.Logger {
	if l, ok := c.Get(loggerKey); ok {
		return l.(*zap.Logger)
	}
	return zap.NewNop()
}

// MapError 把常见的下层错误规整为统一错误：*apierr.Error 原样；db.ErrNotFound（含越权被 Scope 过滤的 0 行）→ 404 resource.not_found；
// 其余保持不变（由 Fail 按 500 处理）。
func MapError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := apierr.As(err); ok {
		return err
	}
	if errors.Is(err, db.ErrNotFound) {
		return apierr.Wrap(err, apierr.ResourceNotFound, "resource not found")
	}
	return err
}

// Fail 以统一错误体中止请求：状态码/错误码/Retry-After 来自 apierr（见其映射表），5xx 的原因只写日志、不进响应。
func Fail(c *gin.Context, err error) {
	err = MapError(err)
	id, _ := requestid.FromContext(c.Request.Context())
	r := apierr.Render(err, id)
	if r.Status >= 500 {
		Logger(c).Error("请求处理失败", zap.Int("status", r.Status), zap.String("error_type", errType(err)), zap.Error(err))
	}
	for k, v := range r.Header {
		c.Writer.Header()[k] = v
	}
	c.Header("Content-Type", contentTypeJSON)
	c.Writer.WriteHeader(r.Status)
	_ = json.NewEncoder(c.Writer).Encode(r.Body)
	c.Abort()
}

func errType(err error) string {
	if e, ok := apierr.As(err); ok {
		return string(e.Code)
	}
	return "unknown"
}

// JSON 写出 JSON 响应（Content-Type: application/json; charset=utf-8）。
func JSON(c *gin.Context, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		Fail(c, apierr.Internal(err))
		return
	}
	c.Data(status, contentTypeJSON, append(b, '\n'))
}

// OK 写 200。
func OK(c *gin.Context, v any) { JSON(c, http.StatusOK, v) }

// Created 写 201（location 非空时带 Location 头）。
func Created(c *gin.Context, location string, v any) {
	if location != "" {
		c.Header("Location", location)
	}
	JSON(c, http.StatusCreated, v)
}

// Accepted 写 202 + Task（Location: /api/v1/tasks/{uid}，ADR-017 §7）；同时把 task 记录 id 登记给幂等中间件（可为 0）。
func Accepted(c *gin.Context, taskUID string, taskID uint64, v any) {
	c.Header("Location", APIPrefix+"/tasks/"+taskUID)
	SetTaskID(c, taskID)
	JSON(c, http.StatusAccepted, v)
}

// NoContent 写 204。
func NoContent(c *gin.Context) { c.Status(http.StatusNoContent); c.Writer.WriteHeaderNow() }

// BindJSON 严格解析 JSON 请求体到 dst：Content-Type 必须是 application/json、体积受限、拒绝未知字段与多余内容。
// 失败返回 400 validation.failed（details 指出字段；不回显请求内容）。
func BindJSON(c *gin.Context, dst any) error {
	if mt, _, err := mime.ParseMediaType(c.GetHeader("Content-Type")); err != nil || mt != "application/json" {
		return apierr.Validation(apierr.Detail{Field: "Content-Type", Reason: "must be application/json"})
	}
	body, err := readBody(c)
	if err != nil {
		return err
	}
	return decodeStrict(body, dst)
}

func decodeStrict(body []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var ute *json.UnmarshalTypeError
		var se *json.SyntaxError
		switch {
		case errors.Is(err, io.EOF):
			return apierr.Validation(apierr.Detail{Field: "body", Reason: "request body is required"})
		case errors.As(err, &ute):
			return apierr.Validation(apierr.Detail{Field: ute.Field, Reason: "must be of type " + ute.Type.String()})
		case errors.As(err, &se):
			return apierr.Validation(apierr.Detail{Field: "body", Reason: "malformed JSON"})
		case strings.HasPrefix(err.Error(), "json: unknown field"):
			return apierr.Validation(apierr.Detail{Field: strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`), Reason: "unknown field"})
		default:
			return apierr.Validation(apierr.Detail{Field: "body", Reason: "invalid JSON"})
		}
	}
	if dec.More() {
		return apierr.Validation(apierr.Detail{Field: "body", Reason: "unexpected trailing content"})
	}
	return nil
}

// readBody 读取（并缓存回 Request.Body，使后续中间件/handler 可再读）受限的请求体。
func readBody(c *gin.Context) ([]byte, error) {
	limit := DefaultMaxBodyBytes
	if v, ok := c.Get(maxBodyKey); ok {
		limit = v.(int64)
	}
	if c.Request.Body == nil || c.Request.Body == http.NoBody {
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(c.Request.Body, limit+1))
	_ = c.Request.Body.Close()
	if err != nil {
		return nil, apierr.Validation(apierr.Detail{Field: "body", Reason: "could not read request body"})
	}
	if int64(len(b)) > limit {
		return nil, apierr.Validation(apierr.Detail{Field: "body", Reason: "request body too large"})
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(b))
	return b, nil
}

// CheckIfMatch 实现乐观并发的前置条件检查：请求带 If-Match 且与资源当前版本（ETag/version 文本）不符 → 412 resource.precondition_failed。
// 未带 If-Match 返回 nil（是否强制要求由 handler 决定）。支持 `*`、带引号与弱校验前缀 W/ 的取值，多值以逗号分隔。
func CheckIfMatch(c *gin.Context, current string) error {
	h := c.GetHeader("If-Match")
	if h == "" {
		return nil
	}
	for _, v := range strings.Split(h, ",") {
		v = strings.TrimPrefix(strings.TrimSpace(v), "W/")
		v = strings.Trim(v, `"`)
		if v == "*" || v == current {
			return nil
		}
	}
	return apierr.PreconditionFailed()
}
