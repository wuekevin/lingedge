package signingkeysit

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/lineage"
	"github.com/wuekevin/lingedge/internal/platform/signingkeys"
	"github.com/wuekevin/lingedge/pkg/secretbox"
	"github.com/wuekevin/lingedge/pkg/signing"
	"github.com/wuekevin/lingedge/tests/apistack"
)

func purposes(ps []signingkeys.Purpose) []string {
	var out []string
	for _, p := range ps {
		out = append(out, string(p))
	}
	return out
}

// AC1：初始化生成三种密钥、私钥信封加密入库、证书链与 SAN/用途/有效期正确、lineage=1、审计恰好一条。
func TestInitializeGeneratesKeys(t *testing.T) {
	w := apistack.New(t)
	seedSANExtra(w, `["10.9.8.7","Extra.Example.Test"]`)
	before := time.Now()

	res, err := w.Keys.Initialize(ctxT(t), signingkeys.InitOptions{ServerURL: serverURL})
	if err != nil {
		t.Fatalf("初始化: %v", err)
	}
	if got := strings.Join(purposes(res.Created), ","); got != "config_signing,mtls_ca,grpc_server" {
		t.Fatalf("本次新建: %s", got)
	}
	if !res.LineageInitialized || len(res.Keys) != 3 {
		t.Fatalf("结果: %+v", res)
	}

	keys := readKeys(t, w)
	if len(keys) != 3 {
		t.Fatalf("signing_keys 应恰 3 行: %d", len(keys))
	}
	cfg, ca, srv := byPurpose(t, keys, "config_signing"), byPurpose(t, keys, "mtls_ca"), byPurpose(t, keys, "grpc_server")
	for _, k := range keys {
		if k.Status != "active" {
			t.Errorf("%s 状态应为 active: %s", k.Purpose, k.Status)
		}
		// 私钥只以 5 段信封密文落库（v1:<kek_id>:<wrapped_dek>:<nonce>:<ct>），enc_key_id = 当前 KEK。
		if parts := strings.Split(k.Enc, ":"); len(parts) != 5 || parts[0] != "v1" || parts[1] != k.EncKeyID || k.EncKeyID != w.Box.CurrentKeyID() {
			t.Errorf("%s 的 private_key_enc 不是信封密文: %.20s… enc_key_id=%s", k.Purpose, k.Enc, k.EncKeyID)
		}
		if strings.Contains(k.Enc, "PRIVATE KEY") {
			t.Errorf("%s 密文列含 PEM 私钥", k.Purpose)
		}
	}
	if cfg.Algorithm != "ed25519" || len(cfg.Public) != 32 || cfg.CertPEM.Valid || !strings.HasPrefix(cfg.KeyID, "cfg-") {
		t.Errorf("配置签名密钥: %+v", cfg)
	}
	if ca.Algorithm != "ecdsa_p256" || srv.Algorithm != "ecdsa_p256" || !ca.CertPEM.Valid || !srv.CertPEM.Valid {
		t.Errorf("CA/叶证书算法或证书缺失: %+v %+v", ca.Algorithm, srv.Algorithm)
	}
	if !strings.HasPrefix(ca.KeyID, "ca-") || !strings.HasPrefix(srv.KeyID, "srv-") || len(cfg.KeyID) > 32 {
		t.Errorf("key_id: %s %s %s", cfg.KeyID, ca.KeyID, srv.KeyID)
	}

	// 指纹：配置签名 = SHA-256(公钥)；CA = SHA-256(证书 DER)（enroll Token 的 ca_fp）。
	caCert, leaf := cert(t, ca), cert(t, srv)
	byKey := map[signingkeys.Purpose]signingkeys.KeyInfo{}
	for _, k := range res.Keys {
		byKey[k.Purpose] = k
	}
	if byKey[signingkeys.PurposeConfigSigning].Fingerprint != signing.Fingerprint(cfg.Public) || byKey[signingkeys.PurposeConfigSigning].NotAfter != nil {
		t.Errorf("配置签名指纹/到期: %+v", byKey[signingkeys.PurposeConfigSigning])
	}
	if res.CAFingerprint() != signing.CertFingerprint(caCert.Raw) || len(res.CAFingerprint()) != 64 {
		t.Errorf("CA 指纹应为证书 DER 的 SHA-256: %s", res.CAFingerprint())
	}
	if _, err := hex.DecodeString(res.CAFingerprint()); err != nil {
		t.Errorf("指纹应为十六进制: %v", err)
	}

	// CA：自签、ECDSA P-256、IsCA、pathlen=0、CertSign、默认 10 年。
	if err := caCert.CheckSignatureFrom(caCert); err != nil {
		t.Errorf("CA 应自签: %v", err)
	}
	if k, ok := caCert.PublicKey.(*ecdsa.PublicKey); !ok || k.Curve != elliptic.P256() {
		t.Errorf("CA 应为 ECDSA P-256")
	}
	if !caCert.IsCA || !caCert.BasicConstraintsValid || caCert.MaxPathLen != 0 || !caCert.MaxPathLenZero || caCert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Errorf("CA 约束: %+v", caCert)
	}
	if got := caCert.NotAfter.Sub(before); got < 10*365*24*time.Hour-time.Hour*24 || got > 10*366*24*time.Hour+time.Hour*24 {
		t.Errorf("CA 默认有效期应约 10 年，实际 NotAfter-now=%v", got)
	}

	// 叶证书：由该 CA 签发；ExtKeyUsage 恰为 serverAuth；SAN = server_url 主机 + san_extra；90 天；经标准库按链校验。
	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth || leaf.IsCA {
		t.Errorf("叶证书用途: %+v IsCA=%v", leaf.ExtKeyUsage, leaf.IsCA)
	}
	if got := strings.Join(leaf.DNSNames, ","); got != "edge.example.test,extra.example.test" {
		t.Errorf("DNS SAN: %s", got)
	}
	if len(leaf.IPAddresses) != 1 || !leaf.IPAddresses[0].Equal(net.ParseIP("10.9.8.7")) {
		t.Errorf("IP SAN: %v", leaf.IPAddresses)
	}
	if d := leaf.NotAfter.Sub(before); d < 90*24*time.Hour-time.Hour || d > 90*24*time.Hour+time.Hour {
		t.Errorf("叶证书应约 90 天: %v", d)
	}
	if !leaf.NotAfter.Before(caCert.NotAfter) {
		t.Error("叶证书不得晚于 CA 到期")
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	for _, host := range []string{"edge.example.test", "extra.example.test", "10.9.8.7"} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: host, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			t.Errorf("叶证书应通过 CA→叶 链校验（%s）: %v", host, err)
		}
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "other.example.test", KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err == nil {
		t.Error("SAN 之外的主机名不应通过")
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "edge.example.test", KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err == nil {
		t.Error("叶证书不得可用于 clientAuth")
	}
	// 另一个独立生成的 CA 不能验证本叶证书。
	otherKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherTmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "LingEdge Node CA"}, NotBefore: before.Add(-time.Hour), NotAfter: before.AddDate(1, 0, 0),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true}
	otherDER, err := x509.CreateCertificate(rand.Reader, otherTmpl, otherTmpl, &otherKey.PublicKey, otherKey)
	if err != nil {
		t.Fatal(err)
	}
	otherCA, _ := x509.ParseCertificate(otherDER)
	otherRoots := x509.NewCertPool()
	otherRoots.AddCert(otherCA)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: otherRoots, DNSName: "edge.example.test"}); err == nil {
		t.Error("别的 CA 不应验证通过本叶证书")
	}
	// 叶证书的私钥与证书公钥配对：解密后的叶私钥签名可被叶证书公钥验证（私钥确实是该证书的）。
	der := decrypt(t, w, srv)
	defer secretbox.Zero(der)
	pk, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		t.Fatal(err)
	}
	if !pk.(*ecdsa.PrivateKey).PublicKey.Equal(leaf.PublicKey) {
		t.Error("叶证书私钥与证书公钥不匹配")
	}

	// deploy.lineage=1（同事务写入），审计恰一条且不含私钥。
	if v := w.RootStr("SELECT CAST(value_json AS CHAR) FROM §.system_settings WHERE setting_key = 'deploy.lineage'"); v != "1" {
		t.Errorf("deploy.lineage 应初始化为 1: %q", v)
	}
	if n := w.RootInt("SELECT COUNT(*) FROM §.audit_logs WHERE action = 'system.signing_keys.init' AND result = 'success' AND actor_type = 'system' AND resource_type = 'signing_key'"); n != 1 {
		t.Errorf("审计应恰一条: %d", n)
	}
	// 初始化前没有 lineage 时 1.13 的 lineage show 拒绝（ErrNotInstalled）——已在 TestLineageBumpAfterInit 联调。
}

// 初始化的参数校验：CA 有效期 0 = 默认 10 年；5–10 年可配；之外拒绝（不静默截断）；非法 server_url 拒绝；失败时不写任何东西。
func TestInitializeValidityAndOptions(t *testing.T) {
	for _, years := range []int{5, 7, 10} {
		t.Run(fmt.Sprintf("%d年", years), func(t *testing.T) {
			w := apistack.New(t)
			start := time.Now()
			if _, err := w.Keys.Initialize(ctxT(t), signingkeys.InitOptions{ServerURL: serverURL, CAValidityYears: years}); err != nil {
				t.Fatalf("%d 年应被接受: %v", years, err)
			}
			ca := cert(t, byPurpose(t, readKeys(t, w), "mtls_ca"))
			want := start.AddDate(years, 0, 0)
			if d := ca.NotAfter.Sub(want); d < -time.Hour || d > time.Hour {
				t.Errorf("%d 年: NotAfter=%v 期望约 %v", years, ca.NotAfter, want)
			}
		})
	}
	w := apistack.New(t)
	for _, years := range []int{-1, 1, 4, 11, 100} {
		_, err := w.Keys.Initialize(ctxT(t), signingkeys.InitOptions{ServerURL: serverURL, CAValidityYears: years})
		if !errors.Is(err, signingkeys.ErrInvalidCAValidity) {
			t.Errorf("%d 年应被拒绝: %v", years, err)
		}
	}
	for _, u := range []string{"", "edge.example.test", "ftp://edge.example.test", "https://", fmt.Sprintf("https://%s:%s@edge.example.test", "ops", randKey(t)), "https://bad host/", "https://0.0.0.0:9443", "://x"} {
		if _, err := w.Keys.Initialize(ctxT(t), signingkeys.InitOptions{ServerURL: u}); !errors.Is(err, signingkeys.ErrInvalidServerURL) {
			t.Errorf("server_url %q 应被拒绝: %v", u, err)
		}
	}
	if n := w.RootInt("SELECT COUNT(*) FROM §.signing_keys"); n != 0 {
		t.Errorf("参数非法时不得写入任何密钥: %d", n)
	}
	// IP 形式的 server_url 进入 IP SAN。
	if _, err := w.Keys.Initialize(ctxT(t), signingkeys.InitOptions{ServerURL: "https://192.0.2.10:9443"}); err != nil {
		t.Fatal(err)
	}
	leaf := cert(t, byPurpose(t, readKeys(t, w), "grpc_server"))
	if len(leaf.IPAddresses) != 1 || !leaf.IPAddresses[0].Equal(net.ParseIP("192.0.2.10")) || len(leaf.DNSNames) != 0 {
		t.Errorf("IP SAN: %v %v", leaf.IPAddresses, leaf.DNSNames)
	}
}

// 幂等：已存在不覆盖；deploy.lineage 已存在则不改；全部已存在时不写审计；缺一个 purpose 时只补缺失的（CA 已存在则解密旧 CA 私钥签发新叶）。
func TestInitializeIdempotent(t *testing.T) {
	w := apistack.New(t)
	// 运维在初始化之前就有的 lineage（例如从备份恢复后的库）：已存在则不改。
	uid, _ := apistack.IDs.New()
	w.Root("INSERT INTO §.system_settings (uid, setting_key, value_json, group_name) VALUES (?, 'deploy.lineage', CAST('5' AS JSON), 'deploy')", uid)

	r1, err := w.Keys.Initialize(ctxT(t), signingkeys.InitOptions{ServerURL: serverURL})
	if err != nil || len(r1.Created) != 3 || r1.LineageInitialized {
		t.Fatalf("首次: %+v %v", r1, err)
	}
	first := readKeys(t, w)
	auditN := w.RootInt("SELECT COUNT(*) FROM §.audit_logs WHERE action = 'system.signing_keys.init'")

	for i := 0; i < 3; i++ {
		r, err := w.Keys.Initialize(ctxT(t), signingkeys.InitOptions{ServerURL: "https://another.example.test", CAValidityYears: 6})
		if err != nil || len(r.Created) != 0 || r.LineageInitialized {
			t.Fatalf("再次初始化应为空操作: %+v %v", r, err)
		}
		for j, k := range r.Keys {
			if k.Fingerprint != r1.Keys[j].Fingerprint || k.KeyID != r1.Keys[j].KeyID {
				t.Errorf("已存在的密钥被改动: %s", k.Purpose)
			}
		}
	}
	after := readKeys(t, w)
	if len(after) != 3 {
		t.Fatalf("行数应不变: %d", len(after))
	}
	for i := range first {
		if first[i].Enc != after[i].Enc || first[i].KeyID != after[i].KeyID || first[i].CertPEM != after[i].CertPEM || string(first[i].Public) != string(after[i].Public) {
			t.Errorf("%s 被覆盖", first[i].Purpose)
		}
	}
	if v := w.RootStr("SELECT CAST(value_json AS CHAR) FROM §.system_settings WHERE setting_key = 'deploy.lineage'"); v != "5" {
		t.Errorf("已存在的 deploy.lineage 不得被改: %q", v)
	}
	if n := w.RootInt("SELECT COUNT(*) FROM §.audit_logs WHERE action = 'system.signing_keys.init'"); n != auditN || n != 1 {
		t.Errorf("空操作不得再写审计: %d（首次 %d）", n, auditN)
	}

	// 叶证书被移除（模拟部分初始化）：重新初始化只补叶证书，CA 与配置签名密钥不变，新叶仍由原 CA 签发。
	w.Root("DELETE FROM §.signing_keys WHERE purpose = 'grpc_server'")
	r, err := w.Keys.Initialize(ctxT(t), signingkeys.InitOptions{ServerURL: serverURL})
	if err != nil || strings.Join(purposes(r.Created), ",") != "grpc_server" {
		t.Fatalf("补建: %+v %v", r, err)
	}
	keys := readKeys(t, w)
	if len(keys) != 3 || byPurpose(t, keys, "mtls_ca").Enc != byPurpose(t, first, "mtls_ca").Enc {
		t.Fatal("CA 不应被改动")
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert(t, byPurpose(t, keys, "mtls_ca")))
	if _, err := cert(t, byPurpose(t, keys, "grpc_server")).Verify(x509.VerifyOptions{Roots: roots, DNSName: "edge.example.test"}); err != nil {
		t.Errorf("补建的叶证书应由原 CA 签发: %v", err)
	}
}

// 并发初始化只成功一次：N 个并发调用，恰有一个报告 Created；库内每个 purpose 恰一行 active、lineage 一行、审计一条。
func TestInitializeConcurrentOnlyOnce(t *testing.T) {
	w := apistack.New(t)
	const n = 12
	var wg sync.WaitGroup
	results := make([]signingkeys.InitResult, n)
	errs := make([]error, n)
	start := make(chan struct{})
	svcs := make([]*signingkeys.Service, n)
	for i := range svcs {
		svcs[i] = newService(t, w, nil, nil)
	}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = svcs[i].Initialize(context.Background(), signingkeys.InitOptions{ServerURL: serverURL})
		}()
	}
	close(start)
	wg.Wait()
	created := 0
	var fp string
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("并发调用 %d 失败: %v", i, errs[i])
		}
		if len(results[i].Created) > 0 {
			created++
			if len(results[i].Created) != 3 {
				t.Errorf("胜者应一次建全 3 个: %v", results[i].Created)
			}
		}
		if fp == "" {
			fp = results[i].CAFingerprint()
		} else if results[i].CAFingerprint() != fp {
			t.Error("所有调用最终看到同一个 CA")
		}
	}
	if created != 1 {
		t.Errorf("恰一个调用应报告创建，实际 %d", created)
	}
	for _, p := range []string{"config_signing", "mtls_ca", "grpc_server"} {
		if c := w.RootInt("SELECT COUNT(*) FROM §.signing_keys WHERE purpose = ? AND status = 'active'", p); c != 1 {
			t.Errorf("%s active 行数应为 1: %d", p, c)
		}
	}
	if c := w.RootInt("SELECT COUNT(*) FROM §.signing_keys"); c != 3 {
		t.Errorf("总行数应为 3: %d", c)
	}
	if c := w.RootInt("SELECT COUNT(*) FROM §.system_settings WHERE setting_key = 'deploy.lineage'"); c != 1 {
		t.Errorf("lineage 行数: %d", c)
	}
	if c := w.RootInt("SELECT COUNT(*) FROM §.audit_logs WHERE action = 'system.signing_keys.init'"); c != 1 {
		t.Errorf("审计应恰一条: %d", c)
	}
}

// 缺主密钥时拒绝初始化：Box 为空、主密钥文件缺失、KeyProvider 已关闭（主密钥已清零）——都不得写入任何东西（含 lineage 与审计）。
func TestInitializeRefusesWithoutMasterKey(t *testing.T) {
	w := apistack.New(t)
	assertNothingWritten := func(when string) {
		t.Helper()
		if n := w.RootInt("SELECT COUNT(*) FROM §.signing_keys"); n != 0 {
			t.Errorf("%s：不得写入密钥: %d", when, n)
		}
		if n := w.RootInt("SELECT COUNT(*) FROM §.system_settings WHERE setting_key = 'deploy.lineage'"); n != 0 {
			t.Errorf("%s：不得写入 deploy.lineage: %d", when, n)
		}
		if n := w.RootInt("SELECT COUNT(*) FROM §.audit_logs WHERE action = 'system.signing_keys.init'"); n != 0 {
			t.Errorf("%s：不得写审计: %d", when, n)
		}
	}

	if _, err := signingkeys.New(signingkeys.Options{Pool: w.App, Box: nil, Audit: w.Audit, IDs: apistack.IDs}); !errors.Is(err, signingkeys.ErrMasterKeyMissing) {
		t.Errorf("没有 Box 应拒绝构造: %v", err)
	}
	// 主密钥文件缺失：secretbox 本身拒绝，拿不到 KeyProvider。
	missing := filepath.Join(t.TempDir(), "absent.key")
	if _, err := secretbox.NewFileKeyProvider(secretbox.FileOptions{Path: missing}); !errors.Is(err, secretbox.ErrMasterKeyMissing) {
		t.Errorf("主密钥文件缺失应返回 ErrMasterKeyMissing: %v", err)
	}
	// KeyProvider 已关闭：Box 仍在，但无法加解密——初始化前的自检拒绝。
	kp := closedProvider(t)
	box, err := secretbox.New(kp)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := signingkeys.New(signingkeys.Options{Pool: w.App, Box: box, Audit: w.Audit, IDs: apistack.IDs})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Initialize(ctxT(t), signingkeys.InitOptions{ServerURL: serverURL}); !errors.Is(err, signingkeys.ErrMasterKeyMissing) {
		t.Errorf("主密钥不可用应拒绝初始化: %v", err)
	}
	assertNothingWritten("主密钥已关闭")

	// 对照：同一个库用可用的主密钥可正常初始化（拒绝确实来自缺主密钥，而不是别的原因）。
	if _, err := w.Keys.Initialize(ctxT(t), signingkeys.InitOptions{ServerURL: serverURL}); err != nil {
		t.Fatalf("有主密钥时应成功: %v", err)
	}
	// 初始化后主密钥再次不可用：即使所有密钥都已存在，初始化仍拒绝（先自检），Sign 也失败而不是返回垃圾。
	if _, err := svc.Initialize(ctxT(t), signingkeys.InitOptions{ServerURL: serverURL}); !errors.Is(err, signingkeys.ErrMasterKeyMissing) {
		t.Errorf("已初始化后缺主密钥仍应拒绝: %v", err)
	}
	if _, err := svc.Sign(ctxT(t), []byte("{}")); err == nil {
		t.Error("主密钥不可用时 Sign 必须失败")
	}
}

func closedProvider(t *testing.T) *secretbox.FileKeyProvider {
	t.Helper()
	p := filepath.Join(t.TempDir(), "master.key")
	// 运行期随机生成 32 字节主密钥（base64url，无填充）：不在仓库里出现任何固定密钥（G-6）。
	if err := os.WriteFile(p, []byte("k1:"+randKey(t)+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	kp, err := secretbox.NewFileKeyProvider(secretbox.FileOptions{Path: p})
	if err != nil {
		t.Fatal(err)
	}
	kp.Close()
	return kp
}

// 审计与初始化同一事务：注入审计写入失败（真实 BEFORE INSERT 触发器）→ 初始化失败、密钥/lineage 全部回滚无残留；清除注入后重试成功且审计恰一条。
func TestInitializeRollsBackWithAudit(t *testing.T) {
	w := apistack.New(t)
	w.Root("CREATE TRIGGER §.trg_inject_audit_failure BEFORE INSERT ON §.audit_logs FOR EACH ROW BEGIN IF NEW.action = 'system.signing_keys.init' THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'injected audit write failure'; END IF; END")
	_, err := w.Keys.Initialize(ctxT(t), signingkeys.InitOptions{ServerURL: serverURL})
	if err == nil {
		t.Fatal("审计写入失败时初始化必须失败（fail-closed）")
	}
	if n := w.RootInt("SELECT COUNT(*) FROM §.signing_keys"); n != 0 {
		t.Errorf("回滚后不得残留密钥: %d", n)
	}
	if n := w.RootInt("SELECT COUNT(*) FROM §.system_settings WHERE setting_key = 'deploy.lineage'"); n != 0 {
		t.Errorf("回滚后不得残留 deploy.lineage: %d", n)
	}
	if n := w.RootInt("SELECT COUNT(*) FROM §.audit_logs WHERE action = 'system.signing_keys.init'"); n != 0 {
		t.Errorf("回滚后不得残留审计: %d", n)
	}
	w.Root("DROP TRIGGER §.trg_inject_audit_failure")
	if _, err := w.Keys.Initialize(ctxT(t), signingkeys.InitOptions{ServerURL: serverURL}); err != nil {
		t.Fatalf("清除注入后应成功: %v", err)
	}
	if n := w.RootInt("SELECT COUNT(*) FROM §.audit_logs WHERE action = 'system.signing_keys.init'"); n != 1 {
		t.Errorf("重试后审计应恰一条: %d", n)
	}
	if n := w.RootInt("SELECT COUNT(*) FROM §.signing_keys"); n != 3 {
		t.Errorf("重试后应有 3 把密钥: %d", n)
	}
}

// 联调（1.13 遗留）：初始化前 lineage show/bump 拒绝；初始化后可读到 1、可 bump（单调）并写审计。
func TestLineageBumpAfterInit(t *testing.T) {
	w := apistack.New(t)
	svc, err := lineage.New(w.App, clock.System(), apistack.IDs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Show(ctxT(t)); !errors.Is(err, lineage.ErrNotInstalled) {
		t.Fatalf("初始化前 show 应拒绝: %v", err)
	}
	if _, err := svc.Bump(ctxT(t)); !errors.Is(err, lineage.ErrNotInstalled) {
		t.Fatalf("初始化前 bump 应拒绝: %v", err)
	}
	if _, err := w.Keys.Initialize(ctxT(t), signingkeys.InitOptions{ServerURL: serverURL}); err != nil {
		t.Fatal(err)
	}
	if v, err := svc.Show(ctxT(t)); err != nil || v != 1 {
		t.Fatalf("初始化后 lineage 应为 1: %d %v", v, err)
	}
	prev := int64(1)
	for i := 0; i < 3; i++ {
		r, err := svc.Bump(ctxT(t))
		if err != nil || r.Before != prev || r.After <= prev {
			t.Fatalf("bump %d: %+v %v", i, r, err)
		}
		prev = r.After
	}
	if n := w.RootInt("SELECT COUNT(*) FROM §.audit_logs WHERE action = 'system.lineage.bump' AND result = 'success'"); n != 3 {
		t.Errorf("bump 审计: %d", n)
	}
	// 再次初始化不会把已 bump 的值改回 1。
	if _, err := w.Keys.Initialize(ctxT(t), signingkeys.InitOptions{ServerURL: serverURL}); err != nil {
		t.Fatal(err)
	}
	if v, _ := svc.Show(ctxT(t)); v != prev {
		t.Errorf("幂等初始化不得改 lineage: %d != %d", v, prev)
	}
}

// doctor 用的 CA 剩余有效期检查：刚初始化 = ok；时间推进到剩余 < 1 年 = warn；过期 = expired。
func TestCAStatusForDoctor(t *testing.T) {
	w := apistack.New(t)
	var offset time.Duration
	var mu sync.Mutex
	clk := clock.Func(func() time.Time { mu.Lock(); defer mu.Unlock(); return time.Now().UTC().Add(offset) })
	svc := newService(t, w, nil, clk)
	if _, err := svc.CAStatus(ctxT(t)); !errors.Is(err, signingkeys.ErrNotInitialized) {
		t.Fatalf("初始化前: %v", err)
	}
	if _, err := svc.Initialize(ctxT(t), signingkeys.InitOptions{ServerURL: serverURL, CAValidityYears: 5}); err != nil {
		t.Fatal(err)
	}
	st, err := svc.CAStatus(ctxT(t))
	if err != nil || st.Level != signing.CAExpiryOK || st.Remaining < 4*365*24*time.Hour {
		t.Fatalf("刚初始化应 ok: %+v %v", st, err)
	}
	set := func(d time.Duration) { mu.Lock(); offset = d; mu.Unlock() }
	set(4*365*24*time.Hour + 200*24*time.Hour) // 剩余约 160 天
	if st, _ := svc.CAStatus(ctxT(t)); st.Level != signing.CAExpiryWarn {
		t.Errorf("剩余 < 1 年应告警: %+v", st)
	}
	set(6 * 365 * 24 * time.Hour)
	if st, _ := svc.CAStatus(ctxT(t)); st.Level != signing.CAExpiryExpired {
		t.Errorf("过期应 expired: %+v", st)
	}
}

func randKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
