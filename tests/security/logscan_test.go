// Package security 放跨模块的安全门禁测试。当前：G-5 日志脱敏扫描（Story 1.11 AC5、ADR-011 §7、NFR-12）。
package security_test

import (
	"bytes"
	"context"
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
