package signingkeysit

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/signingkeys"
	"github.com/wuekevin/lingedge/pkg/secretbox"
	"github.com/wuekevin/lingedge/tests/apistack"
)

const serverURL = "https://edge.example.test:9443"

func admin(w *apistack.World) apistack.Session {
	u := w.SeedUser("platform", nil)
	w.GrantPlatformRole(u, "platform_admin")
	return w.Login(u)
}

// probe 记录每个被物化的私钥明文缓冲：保留切片引用与物化时刻的拷贝，用于操作结束后断言「物化时非零、结束后全零」。
type probe struct {
	mu   sync.Mutex
	recs []probeRec
}

type probeRec struct {
	name   string
	live   []byte // 与服务内同一底层数组
	before []byte // 物化时刻的拷贝
}

func (p *probe) fn() signingkeys.SecretProbe {
	return func(name string, buf []byte) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.recs = append(p.recs, probeRec{name: name, live: buf, before: append([]byte(nil), buf...)})
	}
}

func (p *probe) names() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, r := range p.recs {
		out = append(out, r.name)
	}
	return out
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

// assertWiped：每个被物化的缓冲在物化时都含非零内容（探针有效，不是对空缓冲的恒真断言），操作结束后全部为零。
func (p *probe) assertWiped(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.recs) == 0 {
		t.Fatal("探针没有记录到任何私钥缓冲，测试无效")
	}
	for _, r := range p.recs {
		if len(r.live) == 0 || allZero(r.before) {
			t.Errorf("%s：物化时缓冲为空或全零，探针无效", r.name)
		}
		if !allZero(r.live) {
			t.Errorf("%s：操作结束后私钥明文缓冲未被清零", r.name)
		}
	}
}

func (p *probe) reset() { p.mu.Lock(); p.recs = nil; p.mu.Unlock() }

// newService 在同一个 World 的 app 池、真实审计记录器与真实信封加密上构造一个独立的 signingkeys.Service（可带探针与自定义时钟）。
func newService(t *testing.T, w *apistack.World, p *probe, clk clock.Clock) *signingkeys.Service {
	t.Helper()
	o := signingkeys.Options{Pool: w.App, Box: w.Box, Audit: w.Audit, IDs: apistack.IDs, Clock: clk}
	if p != nil {
		o.Probe = p.fn()
	}
	s, err := signingkeys.New(o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func ctxT(t testing.TB) context.Context { return apistack.Ctx(t) }

func seedSANExtra(w *apistack.World, jsonList string) {
	w.T.Helper()
	uid, err := apistack.IDs.New()
	if err != nil {
		w.T.Fatal(err)
	}
	w.Root("INSERT INTO §.system_settings (uid, setting_key, value_json, group_name) VALUES (?, 'grpc.tls.san_extra', CAST(? AS JSON), 'security')", uid, jsonList)
}

type dbKey struct {
	UID, Purpose, KeyID, Algorithm, Status, Enc, EncKeyID string
	Public                                                []byte
	CertPEM                                               sql.NullString
}

func readKeys(t testing.TB, w *apistack.World) []dbKey {
	t.Helper()
	rows, err := w.Adm.QueryContext(ctxT(t), "SELECT uid, purpose, key_id, algorithm, status, private_key_enc, enc_key_id, public_key, cert_pem FROM `"+w.DBName+"`.signing_keys ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []dbKey
	for rows.Next() {
		var k dbKey
		if err := rows.Scan(&k.UID, &k.Purpose, &k.KeyID, &k.Algorithm, &k.Status, &k.Enc, &k.EncKeyID, &k.Public, &k.CertPEM); err != nil {
			t.Fatal(err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func byPurpose(t testing.TB, ks []dbKey, p string) dbKey {
	t.Helper()
	for _, k := range ks {
		if k.Purpose == p {
			return k
		}
	}
	t.Fatalf("没有 purpose=%s 的密钥", p)
	return dbKey{}
}

func cert(t testing.TB, k dbKey) *x509.Certificate {
	t.Helper()
	b, _ := pem.Decode([]byte(k.CertPEM.String))
	if b == nil {
		t.Fatalf("%s 没有证书 PEM", k.Purpose)
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// decrypt 用真实信封加密解出私钥明文（PKCS#8 DER）：仅测试用，用于构造「泄露扫描」的针与用旧 key 签 keyset。调用方负责清零。
func decrypt(t testing.TB, w *apistack.World, k dbKey) []byte {
	t.Helper()
	pt, err := w.Box.Open(ctxT(t), k.Enc, secretbox.RecordContext{Table: "signing_keys", Column: "private_key_enc", UID: k.UID})
	if err != nil {
		t.Fatalf("解密 %s: %v", k.Purpose, err)
	}
	return pt
}

func edKey(t testing.TB, der []byte) ed25519.PrivateKey {
	t.Helper()
	p, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		t.Fatal(err)
	}
	k, ok := p.(ed25519.PrivateKey)
	if !ok {
		t.Fatal("不是 Ed25519 私钥")
	}
	return k
}

// needles 返回私钥材料的各种编码（原始字节、hex、base64 std/url/raw）：泄露扫描的「针」。
func needles(t testing.TB, der []byte) [][]byte {
	t.Helper()
	p, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		t.Fatal(err)
	}
	var raws [][]byte
	raws = append(raws, der)
	switch k := p.(type) {
	case ed25519.PrivateKey:
		raws = append(raws, k.Seed(), []byte(k))
	case *ecdsa.PrivateKey:
		scalar, err := k.Bytes()
		if err != nil {
			t.Fatal(err)
		}
		raws = append(raws, scalar)
	}
	var out [][]byte
	for _, r := range raws {
		out = append(out, r, []byte(hex.EncodeToString(r)), []byte(strings.ToUpper(hex.EncodeToString(r))),
			[]byte(base64.StdEncoding.EncodeToString(r)), []byte(base64.URLEncoding.EncodeToString(r)),
			[]byte(base64.RawStdEncoding.EncodeToString(r)), []byte(base64.RawURLEncoding.EncodeToString(r)))
	}
	return out
}

// dumpDB 返回库内全部表全部单元格的内容（原始字节拼接，含每个单元格之间的分隔），用于全库扫描。
func dumpDB(t testing.TB, w *apistack.World) [][]byte {
	t.Helper()
	tabs, err := w.Adm.QueryContext(ctxT(t), "SELECT table_name FROM information_schema.tables WHERE table_schema = ? AND table_type = 'BASE TABLE'", w.DBName)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tabs.Next() {
		var n string
		if err := tabs.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	_ = tabs.Close()
	if len(names) < 50 {
		t.Fatalf("全库扫描只发现 %d 张表，扫描无效", len(names))
	}
	var cells [][]byte
	for _, n := range names {
		rows, err := w.Adm.QueryContext(ctxT(t), "SELECT * FROM `"+w.DBName+"`.`"+n+"`")
		if err != nil {
			t.Fatalf("扫描表 %s: %v", n, err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			raw := make([]sql.RawBytes, len(cols))
			dest := make([]any, len(cols))
			for i := range raw {
				dest[i] = &raw[i]
			}
			if err := rows.Scan(dest...); err != nil {
				t.Fatal(err)
			}
			for _, c := range raw {
				if len(c) > 0 {
					cells = append(cells, append([]byte(nil), c...))
				}
			}
		}
		_ = rows.Close()
	}
	return cells
}

var _ = time.Second
