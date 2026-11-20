package taskrt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wuekevin/lingedge/internal/platform/uid"
	"github.com/wuekevin/lingedge/pkg/redact"
)

// MaxPayloadBytes 是 payload 上限（任务载荷只放引用，不放内容）。
const MaxPayloadBytes = 64 << 10

// ValidatePayload 校验入队 payload（AC6：payload_json 不含敏感明文，ADR-019 §9）：
//
//   - 必须是 JSON 对象（nil/空 视为 {}），体积 ≤ MaxPayloadBytes；
//   - 键名按 redact.Key 判定为敏感（password/secret/token/credential/…）时拒绝——除非键名以 _id/_uid/_ref 结尾且值是 ULID 或整数，
//     即「对敏感对象的引用」（如 credential_uid），运行时再经 KeyProvider/Repository 取明文；
//   - 任意字符串值若被 redact.String 改写（含 PEM、Bearer、le_ 令牌、DSN 口令、裸高熵密钥等形态）则拒绝。
//
// 返回规整后的 JSON（紧凑、键序保持原样）。拒绝而不是静默脱敏：入队方带着明文说明调用方有缺陷，应当暴露而不是被掩盖。
func ValidatePayload(raw json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage(`{}`), nil
	}
	if len(raw) > MaxPayloadBytes {
		return nil, fmt.Errorf("%w: payload 超过 %d 字节", ErrSensitivePayload, MaxPayloadBytes)
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("taskrt: payload 不是合法 JSON: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("taskrt: payload 含多余内容")
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, fmt.Errorf("taskrt: payload 必须是 JSON 对象")
	}
	if where := findSensitive(v, ""); where != "" {
		return nil, fmt.Errorf("%w: %s", ErrSensitivePayload, where)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func isReferenceKey(k string) bool {
	l := strings.ToLower(k)
	return strings.HasSuffix(l, "_id") || strings.HasSuffix(l, "_uid") || strings.HasSuffix(l, "_ref") ||
		strings.HasSuffix(l, "id") && l != "id" && len(k) > 2 && k[len(k)-2] == 'I' // camelCase: tokenID/credentialId
}

func isReferenceValue(v any) bool {
	switch x := v.(type) {
	case json.Number:
		_, err := x.Int64()
		return err == nil
	case string:
		return uid.Valid(x)
	}
	return false
}

// findSensitive 返回第一处敏感位置的描述（路径，不含值）；空串表示干净。
func findSensitive(v any, path string) string {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			p := path + "/" + k
			if redact.Key(k) {
				if val == nil || (isReferenceKey(k) && isReferenceValue(val)) {
					continue
				}
				return "键 " + p + " 为敏感键名（只允许以 _id/_uid/_ref 结尾且值为引用 ID 的形态）"
			}
			if w := findSensitive(val, p); w != "" {
				return w
			}
		}
	case []any:
		for i, val := range x {
			if w := findSensitive(val, fmt.Sprintf("%s/%d", path, i)); w != "" {
				return w
			}
		}
	case string:
		if redact.String(x) != x {
			return "值 " + path + " 含敏感格式（令牌/私钥/口令/DSN 等）"
		}
	}
	return ""
}

// RedactJSON 对输出到 API/日志的 JSON 再做一次脱敏（纵深防御：历史数据或处理函数写入的 result 也不得外泄敏感值）。
// 无法解析时返回 nil（宁可不返回也不原样透传）。
func RedactJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	out, err := json.Marshal(redact.Value(v))
	if err != nil {
		return nil
	}
	return out
}
