package manifest

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// 仅测试：所有测试私钥都由「标签 → SHA-256 → 种子」确定性派生，只存在于 _test.go 与 testdata。
// 生产构建（含 builtin/keys.json 注入）不得出现这些公钥：gates release-keys 的指纹表覆盖它们（见 tests/security/releasekeys）。

type tkey struct {
	id   string
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

func goldenSeed() []byte {
	s := sha256.Sum256([]byte("LingEdge golden test vector seed v1 - TEST ONLY"))
	return s[:]
}

// usedTestPubs 记录本包测试用到的全部测试公钥；TestMain 在全部测试结束后核对它们都已登记到 release-keys 门禁的指纹表
// （scripts/gates/data/test-release-pubkeys.txt）——新增测试密钥而忘记登记会使测试失败，避免它混入发行公钥而无人发现。
var (
	usedTestPubs   = map[string]bool{}
	usedTestPubsMu sync.Mutex
)

func recordTestPub(pub []byte) {
	usedTestPubsMu.Lock()
	defer usedTestPubsMu.Unlock()
	usedTestPubs[base64.StdEncoding.EncodeToString(pub)] = true
}

func TestMain(m *testing.M) {
	code := m.Run()
	// 只在完整运行时核对（-run 过滤或 -fuzz 时并非所有测试都执行，记录不完整）。
	partial := flag.Lookup("test.run").Value.String() != "" || flag.Lookup("test.fuzz").Value.String() != ""
	if code == 0 && !partial {
		b, err := os.ReadFile("../../../scripts/gates/data/test-release-pubkeys.txt")
		if err != nil {
			fmt.Fprintln(os.Stderr, "无法读取测试公钥指纹表:", err)
			os.Exit(1)
		}
		if len(usedTestPubs) < 5 {
			fmt.Fprintln(os.Stderr, "测试公钥登记检查无效：本次只记录到", len(usedTestPubs), "把")
			os.Exit(1)
		}
		for pub := range usedTestPubs {
			if !bytes.Contains(b, []byte(pub)) {
				fmt.Fprintf(os.Stderr, "测试公钥 %s 未登记到 scripts/gates/data/test-release-pubkeys.txt（release-keys 门禁）\n", pub)
				code = 1
			}
		}
	}
	os.Exit(code)
}

func deriveKey(id, label string) tkey {
	seed := goldenSeed()
	if label != "" {
		s := sha256.Sum256([]byte("LingEdge unit test key v1 - TEST ONLY: " + label))
		seed = s[:]
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	recordTestPub(pub)
	return tkey{id: id, priv: priv, pub: pub}
}

func (k tkey) entry(status, nb, na string) Key {
	return Key{KeyID: k.id, Alg: "ed25519", PublicKey: base64.StdEncoding.EncodeToString(k.pub), Status: status, NotBefore: nb, NotAfter: na}
}

func keysDoc(t *testing.T, revoked []string, keys ...Key) []byte {
	t.Helper()
	if revoked == nil {
		revoked = []string{}
	}
	b, err := json.Marshal(KeysFile{SchemaVersion: 1, Keys: keys, RevokedKeyIDs: revoked})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func signManifest(k tkey, raw []byte) []byte {
	sig := ed25519.Sign(k.priv, append([]byte(ManifestDomain), raw...))
	return sigDoc(k.id, sig)
}

func sigDoc(id string, sig []byte) []byte {
	b, _ := json.Marshal(map[string]string{"key_id": id, "alg": "ed25519", "sig": base64.StdEncoding.EncodeToString(sig)})
	return b
}

var t0 = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

// mem 是内存里的 SequenceStore（测试里仅用于不关心落盘的用例；落盘行为由 FileSequenceStore 的用例覆盖）。
type memSeq map[string]int64

func (m memSeq) Max(ch string) (int64, bool, error) { v, ok := m[ch]; return v, ok, nil }
func (m memSeq) Advance(ch string, n int64) error {
	if n > m[ch] {
		m[ch] = n
	}
	return nil
}

type env struct {
	t      *testing.T
	active tkey
	trust  *TrustSet
	seen   memSeq
	now    time.Time
	v      *Verifier
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, active: deriveKey("rel-test-a", "a"), seen: memSeq{}, now: t0}
	retired := deriveKey("rel-test-old", "old")
	bf, err := ParseKeysFile(keysDoc(t, []string{"rel-test-x"},
		e.active.entry(StatusActive, "2026-01-01T00:00:00Z", "2028-01-01T00:00:00Z"),
		retired.entry(StatusRetired, "", "")))
	if err != nil {
		t.Fatal(err)
	}
	e.trust, err = NewTrustSet(bf)
	if err != nil {
		t.Fatal(err)
	}
	e.v = &Verifier{Trust: e.trust, Seen: e.seen, Now: func() time.Time { return e.now }}
	return e
}

// mf 生成一份合法清单字节（可经 mutate 改写 map 再序列化）。
func mf(t *testing.T, mutate func(m map[string]any)) []byte {
	t.Helper()
	m := map[string]any{
		"schema_version": 1, "channel": "stable", "sequence": 10,
		"generated_at": "2026-11-01T00:00:00Z", "valid_until": "2027-02-01T00:00:00Z",
		"revoked_versions": []any{map[string]any{"component": "node", "version": "0.0.9", "reason": "示意：已知缺陷"}},
		"components": map[string]any{
			"server": map[string]any{"status": "available", "versions": []any{
				map[string]any{
					"version": "0.1.0", "min_upgrade_from": "0.1.0",
					"requires": map[string]any{"external": map[string]any{"mysql": ">=8.0.16 <9", "redis": ">=8"}},
					"ports":    []any{map[string]any{"proto": "tcp", "port": 8080, "bind": "127.0.0.1", "purpose": "rest"}},
					"variants": map[string]any{"default": map[string]any{"artifacts": map[string]any{
						"linux-amd64": map[string]any{"url": "https://rel.example.test/server/0.1.0/lingedge-server-0.1.0-linux-amd64.tar.gz", "sha256": sha256hex("x"), "size": 1234},
					}}},
				}}},
			"dns":       map[string]any{"status": "unreleased"},
			"scheduler": map[string]any{"status": "builtin", "builtin_in": "server"},
		},
	}
	if mutate != nil {
		mutate(m)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

func sha256hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hexOf(h[:])
}

func hexOf(b []byte) string {
	const d = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, d[c>>4], d[c&15])
	}
	return string(out)
}

func writeFile(t *testing.T, p string, b []byte, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, perm); err != nil {
		t.Fatal(err)
	}
}

func wantKind(t *testing.T, err error, k Kind) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望失败 %s，实际成功", k)
	}
	me, ok := err.(*Error)
	if !ok {
		t.Fatalf("错误类型应为 *Error: %T %v", err, err)
	}
	if me.Kind != k {
		t.Fatalf("错误分类 = %s，期望 %s（%v）", me.Kind, k, err)
	}
}
