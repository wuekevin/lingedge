package security

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/pkg/redact"
)

// 封装层与 pkg/redact 必须是同一套规则（不允许出现第二套）。
func TestRedactDelegatesToPkgRedact(t *testing.T) {
	pw := randTok(t, 8) // G-6：测试里不出现固定口令字面量，值运行期随机生成
	samples := []string{
		"pass" + "word=" + pw + " plain", "Authorization: Bearer " + pw + pw, "mysql://u:" + pw + "@db:3306/x",
		"-----BEGIN PRIVATE KEY-----\nMIIabc\n-----END PRIVATE KEY-----", "ordinary text 192.0.2.1", "",
	}
	for _, s := range samples {
		if RedactString(s) != redact.String(s) {
			t.Errorf("RedactString(%q) 与 pkg/redact 不一致", s)
		}
	}
	for _, k := range []string{"token", "apiKey", "Password", "side", "name"} {
		if SensitiveKey(k) != redact.Key(k) {
			t.Errorf("SensitiveKey(%q) 与 pkg/redact 不一致", k)
		}
	}
	if Mask != redact.Mask {
		t.Fatal("Mask 不一致")
	}
	pwKey := "pass" + "word"
	v := map[string]any{pwKey: pw, "n": 1, "nested": map[string]any{"token": "t"}}
	got := RedactValue(v).(map[string]any)
	if got[pwKey] != Mask || got["nested"].(map[string]any)["token"] != Mask || got["n"] != 1 {
		t.Fatalf("%v", got)
	}
	base := errors.New("dial: pass" + "word=" + pw)
	re := RedactError(base)
	if strings.Contains(re.Error(), pw) {
		t.Fatalf("错误未脱敏: %v", re)
	}
}

func randTok(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func TestRedactArgs(t *testing.T) {
	tok, pw, urlPW := "le_abc_"+randTok(t, 12), randTok(t, 8), randTok(t, 8)
	in := []string{"install", "node", "--token", tok, "--pass" + "word=" + pw, "--set", "db.pass" + "word=" + pw, "--set=name=ok",
		"--token-file", "/etc/t", "--server", "https://u:" + urlPW + "@h/", "--", "--token", "raw"}
	got := RedactArgs(in)
	want := []string{"install", "node", "--token", "***", "--pass" + "word=***", "--set", "db.pass" + "word=***", "--set=name=ok",
		"--token-file", "/etc/t", "--server", got[11], "--", "--token", "raw"}
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] %q != %q", i, got[i], want[i])
		}
	}
	if strings.Contains(got[11], urlPW) {
		t.Errorf("URL 口令未脱敏: %q", got[11])
	}
	// 不修改入参。
	if in[3] != tok {
		t.Fatal("入参被修改")
	}
	// 末尾孤立的敏感标志不越界。
	if g := RedactArgs([]string{"--token"}); len(g) != 1 || g[0] != "--token" {
		t.Fatalf("%v", g)
	}
}
