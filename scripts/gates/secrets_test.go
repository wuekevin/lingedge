package main

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 门禁的负向样本一律在运行时生成真实形态的私钥（随机、不落入仓库）；本文件自身位于 scripts/gates，被扫描器跳过。
func randEd25519(t *testing.T) (ed25519.PrivateKey, []byte) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return priv, der
}

func secretRules(fs []Finding) string {
	var r []string
	for _, f := range fs {
		r = append(r, f.Rule)
	}
	return strings.Join(r, ",")
}

func TestSecretsPEMRealKeysDetected(t *testing.T) {
	_, der := randEd25519(t)
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecDER, err := x509.MarshalECPrivateKey(ec)
	if err != nil {
		t.Fatal(err)
	}
	for name, blk := range map[string]*pem.Block{
		"pkcs8":   {Type: "PRIVATE KEY", Bytes: der},
		"ec":      {Type: "EC PRIVATE KEY", Bytes: ecDER},
		"encrypt": {Type: "ENCRYPTED PRIVATE KEY", Bytes: der},
	} {
		text := string(pem.EncodeToMemory(blk))
		if got := secretRules(scanSecretText("a/b.txt", []byte(text))); !strings.Contains(got, "secret-pem-private") {
			t.Errorf("%s: 真实 PEM 私钥必须被发现，得到 %q", name, got)
		}
		// JSON 转义换行形态（出现在测试数据/配置的字符串里）同样被发现。
		esc := strings.ReplaceAll(text, "\n", `\n`)
		if got := secretRules(scanSecretText("a/b.json", []byte(`{"k":"`+esc+`"}`))); !strings.Contains(got, "secret-pem-private") {
			t.Errorf("%s: 转义换行形态的 PEM 私钥必须被发现", name)
		}
	}
	// OpenSSH 形态：魔数开头的正文。
	body := base64.StdEncoding.EncodeToString(append([]byte("openssh-key-v1\x00"), make([]byte, 64)...))
	if got := secretRules(scanSecretText("x", []byte("-----BEGIN OPENSSH PRIVATE KEY-----\n"+body+"\n-----END OPENSSH PRIVATE KEY-----"))); !strings.Contains(got, "secret-pem-private") {
		t.Errorf("OpenSSH 私钥必须被发现: %q", got)
	}
}

func TestSecretsPEMPlaceholdersNotFlagged(t *testing.T) {
	for name, text := range map[string]string{
		"截断占位":     "-----BEGIN PRIVATE KEY-----\nMIIB\n-----END PRIVATE KEY-----",
		"AAAA":     "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n",
		"重复字样":     "-----BEGIN PRIVATE KEY-----\nMIIFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKE\n-----END PRIVATE KEY-----",
		"Ed25519头": "-----BEGIN PRIVATE KEY-----\nMC4CAQAwBQYDK2VwBCIEIA==\n-----END PRIVATE KEY-----\n",
		"只有头":      "-----BEGIN RSA PRIVATE KEY-----",
	} {
		if got := secretRules(scanSecretText("t.go", []byte(text))); got != "" {
			t.Errorf("%s: 占位 PEM 不是私钥，不应触发，得到 %q", name, got)
		}
	}
}

func TestSecretsReleaseKeyFileAndPKCS8(t *testing.T) {
	priv, der := randEd25519(t)
	seed := base64.StdEncoding.EncodeToString(priv.Seed())
	keyFile := `{"format":"lingedge-release-key-v1","key_id":"rel-x","alg":"ed25519","seed":"` + seed + `"}`
	if got := secretRules(scanSecretText("keys/k.json", []byte(keyFile))); !strings.Contains(got, "secret-release-key-file") {
		t.Errorf("签名工具私钥文件必须被发现: %q", got)
	}
	if got := secretRules(scanSecretText("x.txt", []byte("blob="+base64.StdEncoding.EncodeToString(der)))); !strings.Contains(got, "secret-pkcs8-ed25519") {
		t.Errorf("Ed25519 PKCS#8 base64 必须被发现: %q", got)
	}
	// 只有格式标识、没有 seed（源码/文档里的常量与说明）不触发。
	if got := secretRules(scanSecretText("x.go", []byte(`const keyFileFormat = "lingedge-release-key-v1"`))); got != "" {
		t.Errorf("仅格式标识不应触发: %q", got)
	}
}

func TestSecretsPrivateKeyField(t *testing.T) {
	priv, _ := randEd25519(t)
	b64 := base64.StdEncoding.EncodeToString(priv.Seed())
	hx := hex.EncodeToString(priv.Seed())
	for name, text := range map[string]string{
		"json b64": `{"private_key": "` + b64 + `"}`,
		"yaml hex": "release_key: " + hx,
		"env":      "SIGNING_KEY=" + base64.StdEncoding.EncodeToString(priv),
		"go":       `privateKey := "` + b64 + `"`,
	} {
		if got := secretRules(scanSecretText("c.cfg", []byte(text))); !strings.Contains(got, "secret-private-key-field") {
			t.Errorf("%s: 应触发 secret-private-key-field，得到 %q", name, got)
		}
	}
	for name, text := range map[string]string{
		"路径":      "private_key: /etc/lingedge/release-signing/private-key-file-long-path-name-x",
		"短值":      "private_key: abc123",
		"公钥":      `"public_key": "` + base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)) + `"`,
		"占位符":     "private_key: ${LINGEDGE_PRIVATE_KEY_PLACEHOLDER_VALUE_FROM_ENV_SOMEWHERE}",
		"DER 固定头": "private_key=MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC",
	} {
		if got := secretRules(scanSecretText("c.cfg", []byte(text))); got != "" {
			t.Errorf("%s: 不应触发，得到 %q", name, got)
		}
	}
}

func TestSecretsFileNamesAndCI(t *testing.T) {
	for _, n := range []string{"a/b/release.key", "x/id_ed25519", "x/ID_RSA", "k/server.p12", "release-key.json", "release_signing_key-2026.json", "a.keystore"} {
		if secretFileNameFinding(n) == nil {
			t.Errorf("%s 应被识别为私钥类文件名", n)
		}
	}
	for _, n := range []string{"a/id_ed25519.pub", "docs/release.md", "keys.json", "internal/tools/manifest/builtin/keys.json", "a/monkey.go"} {
		if f := secretFileNameFinding(n); f != nil {
			t.Errorf("%s 不应被识别: %v", n, f)
		}
	}
	ci := "steps:\n  - run: release-sign sign -key ${{ secrets.RELEASE_SIGNING_KEY }}\n"
	if got := secretRules(scanSecretText(".github/workflows/rel.yml", []byte(ci))); !strings.Contains(got, "secret-ci-release-key") {
		t.Errorf("CI 引用发行私钥必须被发现: %q", got)
	}
	if got := secretRules(scanSecretText("docs/operations/release.md", []byte(ci))); got != "" {
		t.Errorf("非 CI 文件（文档叙述）不应触发 CI 规则: %q", got)
	}
}

func TestSecretsRunOnTree(t *testing.T) {
	root := t.TempDir()
	write := func(rel, c string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("README.md", "# 仓库\n")
	write("scripts/x.go", "package x\n")
	if got, err := runSecrets(root, options{}); err != nil || len(got) != 0 {
		t.Fatalf("干净树应通过: %v %v", got, err)
	}
	priv, der := randEd25519(t)
	write("a/leak.pem", string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})))
	write("b/rel.key", "binary-ish\n")
	write("c/keyfile.json", `{"format":"lingedge-release-key-v1","key_id":"rel-y","alg":"ed25519","seed":"`+base64.StdEncoding.EncodeToString(priv.Seed())+`"}`)
	got, err := runSecrets(root, options{})
	if err != nil {
		t.Fatal(err)
	}
	r := secretRules(got)
	for _, want := range []string{"secret-pem-private", "secret-key-filename", "secret-release-key-file"} {
		if !strings.Contains(r, want) {
			t.Errorf("缺少发现 %s，得到 %q", want, r)
		}
	}
}
