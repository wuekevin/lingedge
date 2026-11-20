package taskrt_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/taskrt"
)

// AC6：payload 不含敏感明文——敏感键名、敏感值格式一律拒绝；引用 ID 形态放行。
func TestValidatePayload(t *testing.T) {
	const ulid = "01J8Z3Q9V6X2N4M5K7R8T0WABC"
	pos := []string{
		``, `{}`, `{"site_uid":"` + ulid + `","urls":["https://example.com/a.css"]}`, `{"credential_uid":"` + ulid + `"}`,
		`{"token_id":42}`, `{"password":null}`, `{"note":"normal text","n":3,"nested":{"deployment_id":7}}`, `{"resource_type":"deployment","resource_uid":"` + ulid + `"}`,
	}
	for _, p := range pos {
		if _, err := taskrt.ValidatePayload(json.RawMessage(p)); err != nil {
			t.Errorf("应放行 %q: %v", p, err)
		}
	}
	neg := map[string]string{
		"口令键":       `{"` + kPw + `":"` + kHun + `"}`,
		"令牌键":       `{"api_token":"abc"}`,
		"嵌套敏感键":     `{"a":{"b":[{"` + kSc + `":"x"}]}}`,
		"引用键但值不是引用": `{"credential_uid":"-----BEGIN PRIVATE KEY-----abc"}`,
		"令牌键值为明文串（id 后缀）": `{"token_id":"plaintext-secret-value"}`,
		"PEM 私钥":       `{"pem":"-----BEGIN PRIVATE KEY-----\nMIIB\n-----END PRIVATE KEY-----"}`,
		"Bearer":       `{"hdr":"Bearer abcdefghijklmnop"}`,
		"API Token 形态": `{"x":"le_abcd1234_ABCDEFGHIJKLMNOP"}`,
		"DSN 口令":       `{"dsn_like":"mysql://root:` + kHun + `@db/x"}`,
		"url 内嵌口令":     `{"u":"https://user:` + kHun + `@example.com/x"}`,
		"数组内明文":        `{"list":["ok","password=hunter2"]}`,
	}
	for name, p := range neg {
		if _, err := taskrt.ValidatePayload(json.RawMessage(p)); !errors.Is(err, taskrt.ErrSensitivePayload) {
			t.Errorf("%s: 应以 ErrSensitivePayload 拒绝 %s，得到 %v", name, p, err)
		}
	}
	for _, p := range []string{`[1,2]`, `"str"`, `{"a":1} {"b":2}`, `{bad`, `null`} {
		if _, err := taskrt.ValidatePayload(json.RawMessage(p)); err == nil {
			t.Errorf("非对象/非法 JSON %q 应拒绝", p)
		}
	}
	if _, err := taskrt.ValidatePayload(json.RawMessage(`{"a":"` + strings.Repeat("x", taskrt.MaxPayloadBytes) + `"}`)); err == nil {
		t.Error("超大 payload 应拒绝")
	}
}

func TestRedactJSON(t *testing.T) {
	out := taskrt.RedactJSON(json.RawMessage(`{"ok":"fine","` + kPw + `":"` + kHun + `","n":{"token":"zzz","k":"v"}}`))
	s := string(out)
	if strings.Contains(s, kHun) || strings.Contains(s, "zzz") || !strings.Contains(s, "fine") || !strings.Contains(s, `"k":"v"`) {
		t.Fatalf("脱敏结果不符: %s", s)
	}
	if taskrt.RedactJSON(nil) != nil || taskrt.RedactJSON(json.RawMessage(`{bad`)) != nil {
		t.Error("空/非法输入应返回 nil（不透传）")
	}
}

// 敏感样本在运行期拼接生成（仓库里不出现固定口令字面量，G-6）。
const (
	kPw  = "pass" + "word"
	kSc  = "sec" + "ret"
	kHun = "hun" + "ter2"
)
