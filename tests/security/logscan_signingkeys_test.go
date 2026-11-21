package security_test

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/logx"
)

// G-5（签名/CA 密钥场景，Story 1.23）：真实生成的 Ed25519 配置签名私钥、ECDSA P-256 的 CA/叶证书私钥（PKCS#8 DER 与 PEM），
// 无论经消息、键名敏感的字段、键名不敏感的字段、错误链还是嵌套对象写入日志，输出都不得含私钥材料（PEM 块、DER 的 base64、原始种子的 hex/base64）。
// 端到端（真实 MySQL/Redis 上初始化与签名后扫描真实日志、审计表、API 响应与全库）见 tests/integration/signingkeys 的 TestPrivateKeysNeverLeak。
func TestLogScanSigningKeyMaterial(t *testing.T) {
	_, edPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	edDER, _ := x509.MarshalPKCS8PrivateKey(edPriv)
	ecDER, _ := x509.MarshalPKCS8PrivateKey(ecPriv)
	edPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: edDER}))
	ecPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER}))
	seed := edPriv.Seed()
	ecScalar, err := ecPriv.Bytes()
	if err != nil {
		t.Fatal(err)
	}

	// 必须无痕的材料：PEM 的 base64 主体、DER 的 base64（std/url）、种子的 hex 与 base64。
	secrets := []string{
		base64.StdEncoding.EncodeToString(edDER), base64.StdEncoding.EncodeToString(ecDER),
		base64.RawURLEncoding.EncodeToString(edDER), base64.RawURLEncoding.EncodeToString(ecDER),
		hex.EncodeToString(seed), hex.EncodeToString(ecScalar),
		base64.StdEncoding.EncodeToString(seed), base64.RawURLEncoding.EncodeToString(seed),
	}

	l, buf := newLogger(t)
	l.Info("初始化签名密钥 "+edPEM+" 与 CA "+ecPEM, zap.String("purpose", "config_signing"))
	l.Warn("键名敏感", zap.String("private_key", edPEM), zap.String("signing_private_key", ecPEM), zap.ByteString("private_key_der", ecDER),
		zap.String("ca_private_key", base64.StdEncoding.EncodeToString(ecDER)), zap.String("seed_private_key", hex.EncodeToString(seed)))
	l.Warn("键名不敏感（靠值格式）", zap.String("detail", edPEM), zap.String("blob", ecPEM))
	l.Error("错误链", zap.Error(fmt.Errorf("sign: %w", errors.New("parse key "+edPEM))))
	l.Info("嵌套", zap.Any("key", map[string]any{"purpose": "mtls_ca", "private_key": ecPEM, "material": map[string]any{"private_key_enc_plain": base64.StdEncoding.EncodeToString(ecDER)}}))
	func() {
		defer func() {
			if r := recover(); r != nil {
				l.Error("panic", logx.PanicFields(r)...)
			}
		}()
		panic("boom " + edPEM)
	}()
	out := buf.String()
	if !strings.Contains(out, "***") {
		t.Fatal("输出里没有任何脱敏标记，测试可能没有真正产生日志")
	}
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("日志泄露了私钥材料 %.12s…\n%s", s, out)
		}
	}
	if strings.Contains(out, "PRIVATE KEY-----") || strings.Contains(out, "BEGIN PRIVATE") {
		t.Errorf("日志含 PEM 私钥块\n%s", out)
	}
}
