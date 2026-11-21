// Package releasekeys 是发行信任域①内置公钥的「构建守卫」端到端检查（Story 1.27 AC5、NFR-1）：
//   - 缺失 keys.json：发行构建（make build RELEASE=1；make release 本体属 Story 1.34）失败；开发构建放行但产物内置零公钥（运行期失败关闭）；
//   - 含测试公钥：门禁失败，且对真实 go build 出的 lingedge 二进制做字节扫描（带负向样本，证明扫描不是恒真）；
//   - 干净构建：二进制不含任何测试公钥（base64/hex）。
//
// 构建用 `go build -overlay` 把 keys.json 叠加进真实源码树，不拷贝仓库、不在工作区留下任何文件。
package releasekeys

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/manifest"
)

const goldenPubB64 = "P3pE/lvUPOLOFWJtF/1sxj4LU923fmJQU9Utz1K5ghc="

func root(t *testing.T) string {
	t.Helper()
	r, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func run(t *testing.T, dir string, env []string, name string, args ...string) (int, string) {
	t.Helper()
	c := exec.Command(name, args...)
	c.Dir = dir
	c.Env = append(os.Environ(), env...)
	var buf bytes.Buffer
	c.Stdout, c.Stderr = &buf, &buf
	err := c.Run()
	if err == nil {
		return 0, buf.String()
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), buf.String()
	}
	t.Fatalf("%s %v: %v", name, args, err)
	return -1, ""
}

func requireNoInjectedKeys(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root(t), "internal/tools/manifest/builtin/keys.json")); err == nil {
		t.Skip("工作区已注入 builtin/keys.json（发行构建环境）；本组用例验证未注入的仓库状态")
	}
}

func keysDoc(id, pub string) string {
	b, _ := json.Marshal(map[string]any{"schema_version": 1, "revoked_key_ids": []string{},
		"keys": []any{map[string]any{"key_id": id, "alg": "ed25519", "public_key": pub, "status": "active"}}})
	return string(b)
}

// buildWithKeys 以 overlay 注入 keys.json 构建 ./cmd/lingedge，返回二进制路径（keys 为空则不注入）。
func buildWithKeys(t *testing.T, keys string) string {
	t.Helper()
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "lingedge")
	args := []string{"build", "-trimpath", "-buildvcs=false", "-o", bin}
	if keys != "" {
		kf := filepath.Join(tmp, "keys.json")
		if err := os.WriteFile(kf, []byte(keys), 0o600); err != nil {
			t.Fatal(err)
		}
		ov := filepath.Join(tmp, "overlay.json")
		spec, _ := json.Marshal(map[string]any{"Replace": map[string]string{filepath.Join(root(t), "internal/tools/manifest/builtin/keys.json"): kf}})
		if err := os.WriteFile(ov, spec, 0o600); err != nil {
			t.Fatal(err)
		}
		args = append(args, "-overlay", ov)
	}
	args = append(args, "./cmd/lingedge")
	if code, out := run(t, root(t), []string{"CGO_ENABLED=0", "GOFLAGS="}, "go", args...); code != 0 {
		t.Fatalf("go build 失败: %s", out)
	}
	return bin
}

func testPubKeys(t *testing.T) [][]byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root(t), "scripts/gates/data/test-release-pubkeys.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) == 0 || strings.HasPrefix(l, "#") {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(f[0])
		if err != nil || len(raw) != 32 {
			t.Fatalf("指纹表行非法: %q", l)
		}
		out = append(out, raw)
	}
	if len(out) < 10 {
		t.Fatalf("指纹表只有 %d 项", len(out))
	}
	return out
}

func containsTestKey(bin []byte, keys [][]byte) (string, bool) {
	low := bytes.ToLower(bin)
	for _, k := range keys {
		for _, rep := range []string{base64.StdEncoding.EncodeToString(k), hex.EncodeToString(k)} {
			if bytes.Contains(low, []byte(strings.ToLower(rep))) {
				return rep, true
			}
		}
	}
	return "", false
}

// 缺失 keys.json：发行构建的守卫失败（经真实 Makefile）；开发构建放行。
func TestMakeGuardFailsReleaseBuildWithoutKeys(t *testing.T) {
	requireNoInjectedKeys(t)
	code, out := run(t, root(t), nil, "make", "--no-print-directory", "release-keys-check", "RELEASE=1")
	if code == 0 || !strings.Contains(out, "release-keys-missing") {
		t.Fatalf("发行构建缺失 keys.json 必须失败，实际 %d:\n%s", code, out)
	}
	if code, out := run(t, root(t), nil, "make", "--no-print-directory", "release-keys-check"); code != 0 {
		t.Fatalf("开发构建缺失 keys.json 应放行（运行期失败关闭），实际 %d:\n%s", code, out)
	}
	// make build RELEASE=1 以该守卫为前置（make release 本体属 Story 1.34，须经 build RELEASE=1）。
	code, out = run(t, root(t), nil, "make", "-n", "--no-print-directory", "build", "RELEASE=1")
	if code != 0 || !strings.Contains(out, "release-keys -require") {
		t.Fatalf("make build RELEASE=1 必须先执行 release-keys -require 守卫，实际 %d:\n%s", code, out)
	}
	// 普通 make build 也会先运行守卫（非 -require）：含测试公钥时任何构建失败。
	_, out = run(t, root(t), nil, "make", "-n", "--no-print-directory", "build")
	if !strings.Contains(out, "release-keys") || strings.Contains(out, "release-keys -require") {
		t.Fatalf("make build（非发行）应运行守卫但不带 -require:\n%s", out)
	}
}

func TestBuildWithoutKeysEmbedsNoKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("-short：跳过真实编译")
	}
	requireNoInjectedKeys(t)
	bin := buildWithKeys(t, "")
	b, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if rep, bad := containsTestKey(b, testPubKeys(t)); bad {
		t.Fatalf("干净构建的 lingedge 不得含测试公钥 %s", rep)
	}
	if bytes.Contains(b, []byte(`"public_key"`)) && bytes.Contains(b, []byte(`"revoked_key_ids":[`)) {
		t.Fatal("未注入时二进制不应内置任何 keys.json")
	}
}

// 注入真实形态公钥：被嵌入二进制（证明注入路径真实有效、可达），门禁通过，且二进制不含测试公钥。
func TestBuildWithRealShapedKeyEmbedsIt(t *testing.T) {
	if testing.Short() {
		t.Skip("-short：跳过真实编译")
	}
	requireNoInjectedKeys(t)
	pub, _, err := ed25519.GenerateKey(rand.Reader) // 运行期随机，代表发行方真实公钥
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString(pub)
	bin := buildWithKeys(t, keysDoc("rel-2026-a", b64))
	data, _ := os.ReadFile(bin)
	if !bytes.Contains(data, []byte(b64)) {
		t.Fatal("注入的公钥未出现在二进制里：内置公钥路径不可达或 embed 未生效")
	}
	if rep, bad := containsTestKey(data, testPubKeys(t)); bad {
		t.Fatalf("含真实形态公钥的构建不得含测试公钥 %s", rep)
	}
}

// 含测试公钥：门禁拒绝；且字节扫描器确实能在真实二进制里找到被注入的测试公钥（负向样本，证明扫描非恒真）。
func TestBuildWithTestKeyIsRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("-short：跳过真实编译")
	}
	requireNoInjectedKeys(t)
	keys := keysDoc("rel-2026-a", goldenPubB64)
	// 门禁层：把同样的 keys.json 放进临时仓库骨架，release-keys 必须失败（开发与发行模式都失败）。
	skel := t.TempDir()
	dir := filepath.Join(skel, "internal/tools/manifest/builtin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keys.json"), []byte(keys), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, extra := range [][]string{nil, {"-require"}} {
		args := append([]string{"run", "./scripts/gates", "release-keys", "-root", skel}, extra...)
		code, out := run(t, root(t), nil, "go", args...)
		if code != 1 || !strings.Contains(out, "release-keys-test-pubkey") {
			t.Fatalf("含测试公钥必须使门禁失败（%v），实际 %d:\n%s", extra, code, out)
		}
	}
	// 二进制层：若有人绕过门禁强行构建，测试公钥确实会被嵌入——扫描器能发现它（负向样本）。
	bin := buildWithKeys(t, keys)
	data, _ := os.ReadFile(bin)
	if _, bad := containsTestKey(data, testPubKeys(t)); !bad {
		t.Fatal("扫描器应在被注入测试公钥的二进制中命中（否则「二进制不含测试公钥」的断言是恒真的）")
	}
}

// Story 1.35：真实发行二进制（cmd/lingedge）确实链接了 manifest 的内置公钥与生产验证器装配：嵌入的字节就是被 `cli.Env.NewVerifier` → `manifest.BuiltinKeys`
// 消费的那份 keys.json（可解析、可建信任集、公钥与注入的一致），而不是一个没人读取的孤立字符串。
// （1.34 登记的「cmd/lingedge 链接 manifest 后守卫才对真实产物生效」在 1.33 接线 install 时已成立，这里补上对真实产物的证明。）
func TestRealBinaryLinksBuiltinKeysConsumer(t *testing.T) {
	if testing.Short() {
		t.Skip("-short：跳过真实编译")
	}
	requireNoInjectedKeys(t)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString(pub)
	keys := keysDoc("rel-2026-a", b64)
	bin := buildWithKeys(t, keys)
	nm, err := exec.Command("go", "tool", "nm", bin).Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, sym := range []string{"manifest.BuiltinKeys", "cli.Env.NewVerifier", "cli.runInstall", "manifest.(*Verifier).Verify"} {
		if !bytes.Contains(nm, []byte(sym)) {
			t.Errorf("真实 lingedge 二进制应链接 %s（内置公钥 → 验证器 → install）", sym)
		}
	}
	data, _ := os.ReadFile(bin)
	if !bytes.Contains(data, []byte(keys)) {
		t.Fatal("二进制里找不到与注入原文逐字节一致的内嵌 keys.json")
	}
	kf, err := manifest.ParseKeysFile([]byte(keys))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.NewTrustSet(kf); err != nil {
		t.Fatalf("内嵌 keys.json 无法建立信任集: %v", err)
	}
}
