package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/rbac"
	"github.com/wuekevin/lingedge/pkg/redact"
)

func entryJSON(t *testing.T, e permEntry) string {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Story 1.39b：权限目录里的每一个权限码（单独、整体）都必须能通过守卫——含被 redact 启发式误判的 token:manage、credential:*。
func TestPermEntryGuardAcceptsWholeCatalog(t *testing.T) {
	var all []string
	var lookalike []string
	for _, p := range rbac.All() {
		all = append(all, p.Code)
		e := permEntry{P: []string{p.Code}, V: permEntryVersion}
		raw := entryJSON(t, e)
		if err := permEntryGuard(raw); err != nil {
			t.Errorf("目录内权限码 %q 被守卫拒绝: %v", p.Code, err)
		}
		if redact.String(raw) != raw {
			lookalike = append(lookalike, p.Code)
		}
	}
	// 目录整体 + 内置角色码 + 典型菜单键。
	full := entryJSON(t, permEntry{P: all, R: []string{"platform_admin", "platform_operator", "platform_auditor", "tenant_owner", "site_operator", "tenant_viewer", "token_admin"}, M: []string{"system.users", "sites", "dns-zones"}, V: permEntryVersion})
	if err := permEntryGuard(full); err != nil {
		t.Errorf("完整权限集被拒绝: %v", err)
	}
	want := map[string]bool{"token:manage": true, "credential:read": true, "credential:write": true}
	for _, c := range lookalike {
		delete(want, c)
	}
	if len(want) != 0 {
		t.Errorf("redact 误判清单应含 token:manage/credential:read/credential:write（回归的前提），缺: %v；实际: %v", want, lookalike)
	}
	t.Logf("被 redact.String 误判（守卫放行）的权限码: %v", lookalike)
}

// 负向：守卫是白名单——真正的密钥/令牌、未知权限码、额外字段、非 JSON 一律拒绝。
func TestPermEntryGuardRejectsSecretsAndGarbage(t *testing.T) {
	apiTok := "le_abcd1234_" + strings.Repeat("A1b2C3d4", 6)
	pem := "-----BEGIN PRIVATE KEY-----\\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASC\\n-----END PRIVATE KEY-----"
	bare := "k3Jx9QmZpV7sLw2NfH8dRtY6uBcE4aGoXiMvP1zKq0A" // 43 位裸密钥形态
	cases := map[string]string{
		"p 放 API Token":      `{"p":["` + apiTok + `"],"v":2}`,
		"p 放 Bearer":         `{"p":["Bearer abcdefgh12345678"],"v":2}`,
		"p 放 password 键值":    `{"p":["password:hunter2"],"v":2}`,
		"p 放 token: 值":       `{"p":["token:abcdef0123456789"],"v":2}`,
		"p 放 PEM":            `{"p":["` + pem + `"],"v":2}`,
		"p 放裸密钥":             `{"p":["` + bare + `"],"v":2}`,
		"p 放目录外的形似权限码":       `{"p":["secret:rotate"],"v":2}`,
		"p 放目录外的 key:manage": `{"p":["key:manage"],"v":2}`,
		"p 放目录码加后缀":          `{"p":["token:manage2"],"v":2}`,
		"p 放目录码加尾随空白":        `{"p":["token:manage "],"v":2}`,
		"额外字段":               `{"p":["user:read"],"v":2,"unexpected":"v"}`,
		"额外字段携带令牌":           `{"p":["user:read"],"x":"` + apiTok + `"}`,
		"r 放 API Token":      `{"p":[],"r":["` + apiTok + `"],"v":2}`,
		"r 放非法角色码":           `{"p":[],"r":["Token:Manage"],"v":2}`,
		"m 放 API Token":      `{"p":[],"m":["` + apiTok + `"],"v":2}`,
		"m 放带冒号的键值":          `{"p":[],"m":["token:abcdef0123"],"v":2}`,
		"p 类型错误":             `{"p":"token:manage","v":2}`,
		"非 JSON":             `token:manage`,
		"纯明文口令":              `password=hunter2`,
		"空串":                 ``,
		"尾随内容":               `{"p":["user:read"],"v":2} password=hunter2`,
		"两个对象":               `{"p":["user:read"]}{"p":["user:read"]}`,
	}
	for name, raw := range cases {
		if err := permEntryGuard(raw); err == nil {
			t.Errorf("%s: 守卫必须拒绝: %s", name, raw)
		}
	}
	// 守卫的拒绝原因不得回显被拒绝的内容。
	err := permEntryGuard(`{"p":["` + apiTok + `"],"v":2}`)
	if err == nil || strings.Contains(err.Error(), apiTok) || strings.Contains(err.Error(), "abcd1234") {
		t.Errorf("拒绝原因不得回显内容: %v", err)
	}
}

// pkg/redact 的保护能力不变：目录内的这些权限码字串在自由文本里仍会被启发式遮蔽（修复不靠放宽 redact）。
func TestRedactStillMasksLookalikeInFreeText(t *testing.T) {
	for _, s := range []string{`{"p":["token:manage"]}`, `{"p":["credential:read"]}`, `token: abcdef0123456789`, `password=hunter2`} {
		if redact.String(s) == s {
			t.Errorf("redact.String 应仍遮蔽: %s", s)
		}
	}
}
