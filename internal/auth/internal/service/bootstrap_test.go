package service

import (
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
)

// 向导的口令强度校验：无任何默认口令；弱口令/含邮箱本地部分/字符类不足被拒绝，强口令与长口令短语通过。
func TestValidateSetupPassword(t *testing.T) {
	const email = "ops.admin@example.test"
	ok := []string{"Tr0ub4dor&3xyz", "Cx7#mQp2$Lw9zR", "correct horse battery staple", "Zq9!Kv3#Rt7$Wn5%", "a-very-long-passphrase-with-dashes"}
	for _, p := range ok {
		if err := ValidateSetupPassword(p, email); err != nil {
			t.Errorf("%q 应通过: %v", p, err)
		}
	}
	bad := map[string]string{
		"短":          "Ab1!",
		"11 字符":      "Ab1!Ab1!Ab1",
		"仅小写":        "abcdefghijklmnop",
		"两类":         "abcdefgh12345678",
		"全同字符":       strings.Repeat("a", 24),
		"含弱口令片段":     "MyPassword!2024xx",
		"含 changeme": "Changeme-2024-Zz!",
		"含邮箱本地部分":    "ops.admin-Zz9!!xx",
		"等于邮箱":       email,
		"超长":         strings.Repeat("Ab1!", 40),
		"含 123456":   "Xy!q123456Zz",
	}
	for name, p := range bad {
		err := ValidateSetupPassword(p, email)
		ae, isAE := apierr.As(err)
		if err == nil || !isAE || ae.Code != apierr.AuthPasswordPolicy {
			t.Errorf("%s（%q）应被拒绝为 auth.password_policy: %v", name, p, err)
		}
	}
}
