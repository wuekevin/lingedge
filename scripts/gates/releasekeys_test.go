package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goldenPubB64 = "P3pE/lvUPOLOFWJtF/1sxj4LU923fmJQU9Utz1K5ghc="

func rkRoot(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "internal/tools/manifest"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, releaseKeysDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func realLookingPub(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader) // 运行期随机公钥，代表发行方真实公钥
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(pub)
}

func keysJSON(id, pub, status string, extra map[string]any, revoked ...string) string {
	k := map[string]any{"key_id": id, "alg": "ed25519", "public_key": pub, "status": status}
	for a, b := range extra {
		k[a] = b
	}
	if revoked == nil {
		revoked = []string{}
	}
	b, _ := json.Marshal(map[string]any{"schema_version": 1, "keys": []any{k}, "revoked_key_ids": revoked})
	return string(b)
}

func rules(fs []Finding) string {
	var r []string
	for _, f := range fs {
		r = append(r, f.Rule)
	}
	return strings.Join(r, ",")
}

func TestReleaseKeysGate(t *testing.T) {
	readme := map[string]string{"README.txt": "注入说明"}
	good := map[string]string{"README.txt": "x", "keys.json": ""}
	good["keys.json"] = keysJSON("rel-2026-a", realLookingPub(t), "active", nil)

	cases := []struct {
		name    string
		files   map[string]string
		require bool
		want    string // 期望命中的规则（逗号分隔，空=无发现）
	}{
		{"开发构建缺失 keys.json 放行（运行期失败关闭）", readme, false, ""},
		{"发行构建缺失 keys.json 失败", readme, true, "release-keys-missing"},
		{"发行构建缺失（空目录）失败", map[string]string{}, true, "release-keys-missing"},
		{"合法真实形态公钥通过（发行）", good, true, ""},
		{"合法真实形态公钥通过（开发）", good, false, ""},
		{"含 golden 测试公钥（发行）", map[string]string{"keys.json": keysJSON("rel-2026-a", goldenPubB64, "active", nil)}, true, "release-keys-test-pubkey"},
		{"含 golden 测试公钥（开发构建也失败）", map[string]string{"keys.json": keysJSON("rel-2026-a", goldenPubB64, "active", nil)}, false, "release-keys-test-pubkey"},
		{"key_id 含 test", map[string]string{"keys.json": keysJSON("rel-test-1", realLookingPub(t), "active", nil)}, true, "release-keys-test-key-id"},
		{"key_id 含 golden", map[string]string{"keys.json": keysJSON("golden-1", realLookingPub(t), "active", nil)}, false, "release-keys-test-key-id"},
		{"含私钥字段", map[string]string{"keys.json": keysJSON("rel-a", realLookingPub(t), "active", map[string]any{"private_key": "x"})}, true, "release-keys-private-material"},
		{"含 seed 字段", map[string]string{"keys.json": keysJSON("rel-a", realLookingPub(t), "active", map[string]any{"seed": "x"})}, true, "release-keys-private-material"},
		{"没有 active 公钥", map[string]string{"keys.json": keysJSON("rel-a", realLookingPub(t), "retired", nil)}, true, "release-keys-format"},
		{"active 但已吊销", map[string]string{"keys.json": keysJSON("rel-a", realLookingPub(t), "active", nil, "rel-a")}, true, "release-keys-format"},
		{"公钥长度错", map[string]string{"keys.json": keysJSON("rel-a", "AAAA", "active", nil)}, true, "release-keys-format"},
		{"alg 错", map[string]string{"keys.json": keysJSON("rel-a", realLookingPub(t), "active", map[string]any{"alg": "rsa"})}, true, "release-keys-format"},
		{"不是 JSON", map[string]string{"keys.json": "nope"}, true, "release-keys-format"},
		{"杂散文件（测试密钥换名混入 embed）", map[string]string{"keys.json": good["keys.json"], "dev-keys.json": keysJSON("rel-a", realLookingPub(t), "active", nil)}, true, "release-keys-stray-file"},
		{"README 里夹带测试公钥 base64", map[string]string{"README.txt": goldenPubB64, "keys.json": good["keys.json"]}, true, "release-keys-test-pubkey"},
		{"README 里夹带测试公钥 hex", map[string]string{"README.txt": hex.EncodeToString(mustB64(t, goldenPubB64)), "keys.json": good["keys.json"]}, true, "release-keys-test-pubkey"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := rules(checkReleaseKeys(rkRoot(t, c.files), c.require))
			if c.want == "" {
				if got != "" {
					t.Fatalf("应无发现，实际: %s", got)
				}
				return
			}
			if !strings.Contains(got, c.want) {
				t.Fatalf("应命中 %s，实际: %q", c.want, got)
			}
		})
	}
}

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// 指纹表自身：非空、含 §9.6 golden 公钥、全部是 32 字节。
func TestReleaseKeysFingerprintTable(t *testing.T) {
	m := testReleasePubKeys()
	if len(m) < 10 {
		t.Fatalf("指纹表只有 %d 项", len(m))
	}
	if _, ok := m[string(mustB64(t, goldenPubB64))]; !ok {
		t.Fatal("指纹表缺少 §9.6 golden 公钥")
	}
}

// G-11：测试公钥（base64/hex）出现在生产文件里会被 testdouble 门禁抓到；出现在 tests/ 与 testdata/ 里不算。
func TestTestdoubleGateCatchesTestPubKeyInProduction(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("internal/tools/manifest/keys_prod.go", "package manifest\n\nconst k = \""+goldenPubB64+"\"\n")
	write("configs/tools.yaml", "k: "+hex.EncodeToString(mustB64(t, goldenPubB64))+"\n")
	write("internal/tools/manifest/testdata/vectors.json", goldenPubB64)
	write("tests/security/x_test.go", "package x\n\nconst k = \""+goldenPubB64+"\"\n")
	fs, err := runTestdouble(root, options{skipBuild: true})
	if err != nil {
		t.Fatal(err)
	}
	hit := map[string]bool{}
	for _, f := range fs {
		if f.Rule == "testdouble-fake-data" {
			hit[f.File] = true
		}
	}
	if !hit["internal/tools/manifest/keys_prod.go"] || !hit["configs/tools.yaml"] {
		t.Fatalf("生产文件中的测试公钥应被抓到: %v", fs)
	}
	if hit["internal/tools/manifest/testdata/vectors.json"] || hit["tests/security/x_test.go"] {
		t.Fatalf("测试位置的测试公钥不应报错: %v", fs)
	}
}
