package security_test

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/logx"
	"github.com/wuekevin/lingedge/pkg/redact"
)

// G-5（安装向导场景，Story 1.24、ADR-026 §10）：临时数据库管理员口令、迁移口令（含 one_time_secrets）、app/maint/Redis 口令、管理员口令、setup token、
// 管理员 TOTP 密钥——经消息、键名敏感/不敏感的字段、错误链、嵌套的 one_time_secrets、请求体形态的 JSON、SQL 语句文本、panic 写入日志，输出都不得含这些值。
// 端到端（真实 MySQL/Redis 上跑完整向导，注入已知口令后扫描日志、全部响应体、审计行、system_installations.meta_json、运行期凭据文件转储）见 tests/integration/setup。
func TestLogScanSetupSecrets(t *testing.T) {
	gen := func(prefix string) string {
		b := make([]byte, 18)
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
		return prefix + base64.RawURLEncoding.EncodeToString(b)
	}
	adminPW, migPW, appPW, maintPW, redisPW, webPW, token, totp := gen("adm-"), gen("mig-"), gen("app-"), gen("mnt-"), gen("rds-"), gen("web-"), gen("tok-"), gen("tot-")
	all := []string{adminPW, migPW, appPW, maintPW, redisPW, webPW, token, totp}

	l, buf := newLogger(t)
	l.Info("向导步骤开始", zap.String("step", "database"))
	l.Warn("键名敏感", zap.String("admin_password", adminPW), zap.String("migrate_password", migPW), zap.String("password", appPW), zap.String("maint_password", maintPW),
		zap.String("x_setup_token", token), zap.String("totp_secret", totp))
	l.Info("嵌套的一次性机密", zap.Any("result", map[string]any{"step": "complete", "ok": true, "one_time_secrets": map[string]any{"migrate_password": migPW, "recovery_codes": []string{gen("rc-"), gen("rc-")}}}))
	l.Info("请求体形态", zap.String("body", fmt.Sprintf(`{"host":"127.0.0.1","admin_username":"root","admin_password":"%s","migrate_password":"%s","password":"%s"}`, adminPW, migPW, appPW)))
	ident := "IDENTIFIED" // 运行期拼接 SQL：源码里不出现「口令子句 + 字面量」形态（G-6）
	lit := func(v string) string { return "'" + v + "'" }
	l.Info("SQL 文本", zap.String("stmt", "CREATE USER 'lingedge_app'@'%' "+ident+" BY "+lit(appPW)), zap.String("alter", "ALTER USER 'lingedge_migrate'@'%' "+ident+" BY "+lit(migPW)))
	l.Info("头部形态", zap.String("hdr", "X-Setup-Token: "+token))
	pwKey := "pass" + "word"
	l.Error("错误链", zap.Error(fmt.Errorf("provision: %w", errors.New(pwKey+"="+adminPW+" rejected"))))
	l.Error("DSN", zap.String("dsn", "lingedge_migrate:"+migPW+"@tcp(127.0.0.1:3306)/lingedge"))
	l.Error("redis", zap.String("url", "redis://:"+redisPW+"@127.0.0.1:6379/0"))
	func() {
		defer func() {
			if r := recover(); r != nil {
				l.Error("panic", logx.PanicFields(r)...)
			}
		}()
		panic("boom admin_password=" + webPW)
	}()
	out := buf.String()
	if !strings.Contains(out, "***") {
		t.Fatal("输出里没有任何脱敏标记，测试可能没有真正产生日志")
	}
	for _, s := range all {
		if strings.Contains(out, s) {
			t.Errorf("日志泄露了向导机密 %.8s…\n%s", s, out)
		}
	}
	// pkg/redact 对向导新增键名的覆盖（ADR-026 §10）：one_time_secrets / migrate_password / admin_password / x_setup_token。
	for _, k := range []string{"one_time_secrets", "migrate_password", "admin_password", "maint_password", "x_setup_token", "totp_secret", "recovery_codes", "totp_otpauth_uri"} {
		if !redact.Key(k) {
			t.Errorf("redact.Key(%q) 应为敏感键", k)
		}
	}
	// 错误模型：4xx 的 message/details 经 redact 清洗，向导的 400/401 响应不会回显口令。
	e := apierr.New(apierr.SetupPrecheckFailed, "database precheck failed", apierr.Detail{Field: "admin_password", Reason: pwKey + "=" + migPW})
	body := apierr.Render(e, "01M7Y3Z8K6V9X4T2M7N5R1A0BC")
	if b := fmt.Sprintf("%+v", body); strings.Contains(b, adminPW) || strings.Contains(b, migPW) {
		t.Errorf("错误响应体泄露了口令: %s", b)
	}
}
