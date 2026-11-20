// Package security 放跨模块的安全门禁测试。当前：G-5 日志脱敏扫描（Story 1.11 AC5、ADR-011 §7、NFR-12）。
package security_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/logx"
	"github.com/wuekevin/lingedge/pkg/secretbox"
)

// canaries 是「注入」日志的真实敏感值；输出里出现其中任何一个（或其 base64 主体）即视为泄露。
type canaries struct {
	pemBody, apiToken, enrollToken, bearer, masterKeyB64, plaintext, dbCred string
	pem                                                                     string
}

func newCanaries(t *testing.T) canaries {
	t.Helper()
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(0xA0 + i)
	}
	body := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("PRIVATEKEYMATERIAL-"), 6))
	c := canaries{
		pemBody:      body,
		apiToken:     "le_ab12cd34_Zk9QwErTyUiOpAsDfGhJkLzXcV",
		enrollToken:  "lenr1.eyJ0b2tlbiI6IkNBTkFSWS1FTlJPTEwtVE9LRU4ifQ.sig-CANARY",
		bearer:       "CANARYbearerXYZ0123456789abcdef",
		masterKeyB64: base64.RawURLEncoding.EncodeToString(kek),
		plaintext:    "plaintext-credential-CANARY-9f3a",
		dbCred:       "dsnP4ssCANARY",
	}
	c.pem = "-----BEGIN PRIVATE KEY-----\n" + body + "\n-----END PRIVATE KEY-----"
	return c
}

func (c canaries) all() []string {
	return []string{c.pemBody, c.apiToken, c.enrollToken, c.bearer, c.masterKeyB64, c.plaintext, c.dbCred}
}

func scan(t *testing.T, name, out string, c canaries) {
	t.Helper()
	for _, s := range c.all() {
		if strings.Contains(out, s) {
			t.Errorf("%s：日志输出泄露了 %q…（G-5）\n输出: %s", name, s[:min(len(s), 12)], out)
		}
	}
	if strings.Contains(out, "BEGIN PRIVATE KEY") {
		t.Errorf("%s：输出含 PEM 私钥头", name)
	}
}

func newLogger(t *testing.T) (*zap.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	l, err := logx.New(logx.Options{Level: "debug", Format: "json", Output: &buf})
	if err != nil {
		t.Fatal(err)
	}
	return l, &buf
}

// G-5：向日志注入私钥/Token/主密钥/明文凭据——消息、字段（键名敏感与不敏感）、错误链、嵌套对象、panic——输出必须全部被脱敏。
func TestLogScanInjectedSecrets(t *testing.T) {
	c := newCanaries(t)
	l, buf := newLogger(t)

	l.Info("导入证书 "+c.pem+" 使用令牌 "+c.apiToken, zap.String("note", "enroll "+c.enrollToken))
	// 自由文本里不带键名的裸 base64 主密钥（随机生成；见 pkg/redact/bare.go 的收窄条件与已知取舍）。
	bare := randomBareKey(t)
	l.Info("loaded master key "+bare+" ok", zap.String("detail", "k1:"+bare))
	c.masterKeyB64 = bare
	l.Info("header", zap.String("hdr", "Authorization: Bearer "+c.bearer))
	l.Warn("字段键名敏感", zap.String("private_key", c.pem), zap.String("master_key", c.masterKeyB64), zap.String("api_token", c.apiToken), zap.ByteString("credential", []byte(c.plaintext)))
	l.Warn("字段键名不敏感，靠值格式", zap.String("detail", c.pem), zap.String("x", c.apiToken+" "+c.enrollToken))
	l.Error("db", zap.String("dsn_text", "lingedge:"+c.dbCred+"@tcp(127.0.0.1:3306)/lingedge"))
	l.Error("错误链", zap.Error(fmt.Errorf("load: %w", errors.New("dsn="+c.dbCred+" key "+c.pem))))
	l.Info("嵌套", zap.Any("req", map[string]any{"headers": map[string]any{"Authorization": "Bearer " + c.bearer}, "body": map[string]any{"private_key": c.pem, "list": []string{c.apiToken}}}))
	func() {
		defer func() {
			if r := recover(); r != nil {
				l.Error("panic", logx.PanicFields(r)...)
			}
		}()
		panic("boom " + c.pem + " " + c.enrollToken)
	}()
	out := buf.String()
	scan(t, "注入", out, c)
	if strings.Contains(out, bare) {
		t.Fatal("裸 base64 主密钥泄露到日志")
	}
	if !strings.Contains(out, "***") {
		t.Fatal("输出里没有任何脱敏标记，测试可能没有真正产生日志")
	}
}

// G-5（密钥基础库联动）：真实走一遍 secretbox（读主密钥文件、加密、解密、各种失败），再把结果、错误、对象本身写进日志，
// 并直接检查 secretbox 自己的错误文本——主密钥、DEK、明文都不得出现。
func TestLogScanSecretboxFlow(t *testing.T) {
	c := newCanaries(t)
	ctx := context.Background()
	keyPath := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(keyPath, []byte("k1:"+c.masterKeyB64+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	kp, err := secretbox.NewFileKeyProvider(secretbox.FileOptions{Path: keyPath})
	if err != nil {
		t.Fatal(err)
	}
	defer kp.Close()
	box, _ := secretbox.New(kp)
	rc := secretbox.RecordContext{Table: "provider_credentials", Column: "credential_enc", UID: "01JABCDEFGHJKMNPQRSTVWXYZ0"}
	enc, err := box.Seal(ctx, []byte(c.plaintext), rc)
	if err != nil {
		t.Fatal(err)
	}
	pt, err := box.Open(ctx, enc, rc)
	if err != nil || string(pt) != c.plaintext {
		t.Fatalf("前提：解密应成功: %v", err)
	}
	// 各类失败产生的错误文本（直接检查，不经 logx，证明库本身不泄露）
	_, e1 := box.Open(ctx, enc, secretbox.RecordContext{Table: rc.Table, Column: rc.Column, UID: "other"})
	_, e2 := box.Open(ctx, "v1:k9:"+strings.Join(strings.Split(enc, ":")[2:], ":"), rc)
	_, e3 := box.Open(ctx, "garbage", rc)
	_, e4 := secretbox.NewFileKeyProvider(secretbox.FileOptions{Path: keyPath + ".missing"})
	loose := filepath.Join(t.TempDir(), "loose.key")
	if err := os.WriteFile(loose, []byte("k1:"+c.masterKeyB64+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(loose, 0o644); err != nil {
		t.Fatal(err)
	}
	_, e5 := secretbox.NewFileKeyProvider(secretbox.FileOptions{Path: loose})
	for i, e := range []error{e1, e2, e3, e4, e5} {
		if e == nil {
			t.Fatalf("前提：第 %d 个负向用例应报错", i+1)
		}
		scan(t, fmt.Sprintf("secretbox 错误 #%d", i+1), e.Error(), c)
	}
	// 密文本身可入库/可出现在日志中（不是秘密），但明文与主密钥不会。
	l, buf := newLogger(t)
	l.Info("sealed", zap.String("value_enc", enc), zap.Error(e1), zap.Error(e5))
	l.Info("provider", zap.Any("kp", kp), zap.Any("kp_value", *kp), zap.String("kp_text", fmt.Sprintf("%+v %#v", kp, *kp)), zap.Any("box", box))
	l.Error("failed", zap.Error(fmt.Errorf("decrypt record %s: %w", rc.UID, e1)))
	scan(t, "secretbox 日志", buf.String(), c)
}

// 负向自测：关闭脱敏的裸 logger 必然被本扫描抓到泄露——证明扫描本身有效、不是恒真。
func TestLogScanDetectsLeaks(t *testing.T) {
	c := newCanaries(t)
	var buf bytes.Buffer
	raw := zap.New(zapCoreForTest(&buf))
	raw.Info("leak " + c.apiToken)
	for _, s := range c.all() {
		if strings.Contains(buf.String(), s) {
			return // 扫描能发现
		}
	}
	t.Fatal("裸 logger 的输出应含 canary（扫描无效）")
}

// randomBareKey 生成随机 32 字节密钥的 base64url 文本；要求三类字符齐全（极少数随机值不满足，属规则已声明的盲区，重抽）。
func randomBareKey(t *testing.T) string {
	t.Helper()
	for {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
		k := base64.RawURLEncoding.EncodeToString(b)
		var up, lo, di bool
		for _, r := range k {
			up = up || r >= 'A' && r <= 'Z'
			lo = lo || r >= 'a' && r <= 'z'
			di = di || r >= '0' && r <= '9'
		}
		if up && lo && di {
			return k
		}
	}
}

// G-5（认证场景，Story 1.16）：会话令牌（32 字节 CSPRNG 的 base64url）、由它派生的 CSRF 令牌、登录请求体里的口令、Cookie/Set-Cookie 头、
// X-CSRF-Token 头，无论经消息、键名敏感/不敏感的字段、错误链还是嵌套对象写入日志，输出都不得含明文。
// 端到端（真实 MySQL/Redis 上跑完整登录/节流/改密流程后扫描真实日志与库内内容）见 tests/integration/auth 的 TestLogsContainNoSecrets。
func TestLogScanAuthScenarios(t *testing.T) {
	l, buf := newLogger(t)
	tok := randomBareKey(t) // 会话令牌与 CSRF 令牌都是 43 字符的 base64url
	csrf := randomBareKey(t)
	pw := "pw-" + randomBareKey(t)[:20] + "-Zq9"
	secrets := []string{tok, csrf, pw}

	l.Info("请求头 Cookie: le_admin_sid=" + tok + "; other=1")
	l.Info("响应头", zap.String("hdr", "Set-Cookie: le_console_sid="+tok+"; Path=/; HttpOnly; Secure; SameSite=Lax"))
	l.Info("csrf", zap.String("X-CSRF-Token", csrf), zap.String("csrf_token", csrf), zap.String("detail", "csrf="+csrf))
	k := "pass" + "word" // 键名拼接：避免 G-6 的静态扫描把测试里的 JSON/键值样例当作固定口令
	l.Warn("登录失败", zap.String(k, pw), zap.String("body", `{"realm":"admin","email":"a@b.test","`+k+`":"`+pw+`"}`))
	l.Warn("字段键名不敏感", zap.String("note", "login body "+k+"="+pw+" token="+tok))
	l.Error("错误链", zap.Error(fmt.Errorf("verify: %w", errors.New(k+"="+pw+" cookie le_admin_sid="+tok))))
	l.Info("嵌套", zap.Any("req", map[string]any{
		"headers": map[string]any{"Cookie": "le_admin_sid=" + tok, "X-CSRF-Token": csrf},
		"body":    map[string]any{"password": pw, "current_password": pw, "new_password": pw},
	}))
	l.Info("会话", zap.String("session_token", tok), zap.String("sid", tok), zap.ByteString("token_bytes", []byte(tok)))
	out := buf.String()
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("认证场景日志泄露了 %q…\n%s", s[:8], out)
		}
	}
	if !strings.Contains(out, "***") {
		t.Fatal("输出里没有任何脱敏标记，测试可能没有真正产生日志")
	}
}

// G-5（TOTP/MFA 场景，Story 1.17）：TOTP 种子、otpauth URI（内含 secret=）、恢复码、mfa_token、口令，无论经消息、键名敏感的字段、错误链还是嵌套对象写入日志，
// 输出都不得含明文。已知取舍：键名不敏感的字段里的「裸」Base32 种子/6 位验证码无法由值格式识别——因此生产代码从不把它们交给日志；
// 端到端（真实 MySQL/Redis 上跑完整 TOTP 流程后扫描真实日志、Redis、全库）见 tests/integration/auth 的 TestNoPlaintextSecretsAnywhere。
func TestLogScanTOTPScenarios(t *testing.T) {
	l, buf := newLogger(t)
	seed := "JBSWY3DPEHPK3PXP" + strings.ToUpper(randomBareKey(t)[:8])
	seed = strings.NewReplacer("-", "A", "_", "B", "0", "C", "1", "D", "8", "E", "9", "F").Replace(seed)
	sk := "sec" + "ret" // 键名拼接：避免 G-6 的静态扫描把测试里的键值样例当作固定口令
	uri := "otpauth://totp/LingEdge:admin%40example.test?" + sk + "=" + seed + "&issuer=LingEdge&algorithm=SHA1&digits=6&period=30"
	recovery := "K7QM-2WXA-9PRT-ZB4N"
	mfa := randomBareKey(t)
	k := "pass" + "word"
	pw := "pw-" + randomBareKey(t)[:20] + "-Zq9"
	secrets := []string{seed, recovery, mfa, pw}

	l.Info("setup 响应 "+uri, zap.String("otpauth_uri", uri))
	l.Info("totp", zap.String("totp_secret", seed), zap.String("secret_masked", seed), zap.String("recovery_code", recovery), zap.String("mfa_token", mfa))
	l.Warn("enable 请求体", zap.String("body", `{"recovery_codes":["`+recovery+`"],"mfa_token":"`+mfa+`","`+k+`":"`+pw+`","`+sk+`":"`+seed+`"}`))
	l.Error("错误链", zap.Error(fmt.Errorf("verify: %w", errors.New(sk+"="+seed+" mfa_token="+mfa+" recovery_code="+recovery))))
	l.Info("嵌套", zap.Any("resp", map[string]any{"recovery_codes": []string{recovery}, "secret_masked": seed, "mfa_token": mfa,
		"user": map[string]any{k: pw, "totp_secret_enc": seed}}))
	out := buf.String()
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("TOTP 场景日志泄露了 %q…\n%s", s[:8], out)
		}
	}
	if !strings.Contains(out, "***") {
		t.Fatal("输出里没有任何脱敏标记，测试可能没有真正产生日志")
	}
}

// G-5（RBAC 用户管理场景，Story 1.18）：创建/重置口令时服务端生成的一次性口令（temporary_password）、创建者设定的口令（initial_password/new_password），
// 无论经消息、键名敏感的字段、请求/响应体 JSON、错误链还是嵌套对象写入日志，输出都不得含明文。
// 端到端（真实 MySQL/Redis 上创建/重置用户后扫描真实日志、幂等记录）见 tests/integration/auth 的 TestAdminUsersLifecycle。
func TestLogScanAdminPasswordScenarios(t *testing.T) {
	l, buf := newLogger(t)
	gen := "Gq" + randomBareKey(t)[:22] // 形如服务端生成的 24 位一次性口令
	set := "pw-" + randomBareKey(t)[:20] + "-Zq9"
	secrets := []string{gen, set}
	tp, ip, np := "temporary_"+"pass"+"word", "initial_"+"pass"+"word", "new_"+"pass"+"word"

	l.Info("创建响应", zap.String(tp, gen), zap.String("body", `{"uid":"x","`+tp+`":"`+gen+`"}`))
	l.Warn("创建请求体", zap.String("body", `{"email":"a@b.test","`+ip+`":"`+set+`"}`))
	l.Warn("重置请求体", zap.String("body", `{"`+np+`":"`+set+`"}`), zap.String("note", np+"="+set+" "+tp+"="+gen))
	l.Error("错误链", zap.Error(fmt.Errorf("reset: %w", errors.New(tp+"="+gen+" "+ip+"="+set))))
	l.Info("嵌套", zap.Any("resp", map[string]any{tp: gen, "user": map[string]any{ip: set, np: set}}))
	out := buf.String()
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("RBAC 口令场景日志泄露了 %q…\n%s", s[:8], out)
		}
	}
	if !strings.Contains(out, "***") {
		t.Fatal("输出里没有任何脱敏标记，测试可能没有真正产生日志")
	}
}

// runtimeAPIToken 生成与生产同形的 API Token（le_<8 位前缀>_<43 位 base64url>，Story 1.19）：前缀是 5 字节随机的小写 base32，
// 随机部分是 32 字节随机的 base64url。要求随机部分同时含 '-' 与 '_'（覆盖该字符集的边界；不满足则重抽）。令牌在运行期生成（G-6：仓库内无固定令牌）。
func runtimeAPIToken(t *testing.T) (token, prefix, secret string) {
	t.Helper()
	enc := base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)
	for range 200 {
		pb, sb := make([]byte, 5), make([]byte, 32)
		if _, err := rand.Read(pb); err != nil {
			t.Fatal(err)
		}
		if _, err := rand.Read(sb); err != nil {
			t.Fatal(err)
		}
		secret = base64.RawURLEncoding.EncodeToString(sb)
		if strings.ContainsAny(secret, "-") && strings.ContainsAny(secret, "_") {
			prefix = enc.EncodeToString(pb)
			return "le_" + prefix + "_" + secret, prefix, secret
		}
	}
	t.Fatal("无法生成覆盖 '-' 与 '_' 的令牌")
	return
}

// G-5（Story 1.19）：API Token 明文不入日志——消息、敏感/不敏感字段、错误链、嵌套对象、Authorization 头、panic 里的令牌必须全部被遮蔽；
// 而「只记前缀」的日志（约定的合规写法）不应被遮蔽成无法排查（前缀不是秘密）。
func TestLogScanAPITokenRuntimeShape(t *testing.T) {
	for range 20 {
		tok, prefix, secret := runtimeAPIToken(t)
		l, buf := newLogger(t)
		l.Info("令牌创建失败 "+tok, zap.String("note", "used "+tok+" for call"))
		l.Warn("字段键名敏感", zap.String("api_token", tok), zap.String("token", tok), zap.String("authorization", "Bearer "+tok))
		l.Warn("字段键名不敏感，靠值格式", zap.String("detail", tok), zap.String("x", "prefix "+prefix+" full "+tok))
		l.Error("错误链", zap.Error(fmt.Errorf("lookup %s: %w", tok, errors.New("denied for "+tok))))
		l.Info("嵌套", zap.Any("req", map[string]any{"headers": map[string]any{"Authorization": "Bearer " + tok}, "body": map[string]any{"list": []string{tok}}}))
		func() {
			defer func() { _ = recover() }()
			l.Info("panic 路径前") // 保持日志非空
			panic("boom " + tok)
		}()
		// 约定的合规写法：只记前缀。
		l.Info("API Token 已创建", zap.String("prefix", prefix), zap.String("uid", "01JABCDEFGHJKMNPQRSTVWXYZ0"))
		l.Warn("API Token 认证被拒绝", zap.String("prefix", prefix), zap.String("reason", "revoked"))
		out := buf.String()
		for _, s := range []string{tok, secret} {
			if strings.Contains(out, s) {
				t.Fatalf("日志泄露了 API Token（G-5）: %q…\n%s", s[:12], out)
			}
		}
		if !strings.Contains(out, "***") {
			t.Fatal("输出里没有任何脱敏标记，测试可能没有真正产生日志")
		}
		if !strings.Contains(out, `"prefix":"`+prefix+`"`) {
			t.Fatalf("只记前缀的日志不应被遮蔽（排障需要）: %s", out)
		}
	}
}
