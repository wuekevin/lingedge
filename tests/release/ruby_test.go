package release_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// findRuby 返回支持 Ed25519 的 ruby（$RUBY、PATH、Homebrew）；没有则跳过并明确标注「等待环境验证」（LINGEDGE_REQUIRE_RUBY=1 时失败）。
func findRuby(t *testing.T) string {
	t.Helper()
	script := filepath.Join(repoRoot(t), "scripts", "release-sign", "crosscheck", "ed25519.rb")
	cands := []string{os.Getenv("RUBY")}
	if p, err := exec.LookPath("ruby"); err == nil {
		cands = append(cands, p)
	}
	cands = append(cands, "/opt/homebrew/bin/ruby", "/usr/local/bin/ruby", "/usr/bin/ruby")
	for _, c := range cands {
		if c == "" {
			continue
		}
		if _, err := os.Stat(c); err != nil {
			continue
		}
		cmd := exec.Command(c, script)
		cmd.Stdin = strings.NewReader(`{"op":"probe"}`)
		if err := cmd.Run(); err == nil {
			return c
		}
	}
	if os.Getenv("LINGEDGE_REQUIRE_RUBY") == "1" {
		t.Fatal("LINGEDGE_REQUIRE_RUBY=1 但没有支持 Ed25519 的 Ruby OpenSSL")
	}
	t.Skip("等待环境验证：没有支持 Ed25519 的 Ruby OpenSSL；跳过不代表通过")
	return ""
}

// 流水线真实产出的签名（清单 + 每个包的 SHA256SUMS）由 Ruby OpenSSL 独立验证；篡改一个字节则 Ruby 验签失败。
func TestRubyOpenSSLVerifiesPipelineSignatures(t *testing.T) {
	f := sharedFlow(t)
	ruby := findRuby(t)
	_, pub := pubOf(t, f.keys)
	type sigDoc struct {
		Sig string `json:"sig"`
	}
	load := func(p string) []byte {
		var d sigDoc
		if err := json.Unmarshal(readFile(t, p), &d); err != nil {
			t.Fatal(err)
		}
		return b64d(t, d.Sig)
	}
	var cases []map[string]string
	add := func(name string, in, sig []byte) {
		cases = append(cases, map[string]string{"name": name, "input_b64": base64.StdEncoding.EncodeToString(in), "sig_b64": base64.StdEncoding.EncodeToString(sig)})
	}
	mb := readFile(t, f.pub("stable/manifest.json"))
	add("manifest", append([]byte("LINGEDGE-MANIFEST-V1\n"), mb...), load(f.pub("stable/manifest.json.sig")))
	tam := append([]byte(nil), mb...)
	tam[len(tam)/2] ^= 1
	add("manifest-tampered", append([]byte("LINGEDGE-MANIFEST-V1\n"), tam...), load(f.pub("stable/manifest.json.sig")))
	for _, id := range []string{"server-" + relVer + "-linux-amd64", "server-" + relVer + "-linux-arm64", "tools-" + relVer + "-linux-amd64", "tools-" + relVer + "-linux-arm64"} {
		comp, plat := strings.SplitN(id, "-", 2)[0], id[strings.LastIndex(id[:strings.LastIndex(id, "-")], "-")+1:]
		sums := readFile(t, filepath.Join(f.afterR1, "stage", id, "META", "SHA256SUMS"))
		in := append([]byte("LINGEDGE-PKG-V1\n"+comp+"\n"+relVer+"\n"+plat+"\n"), sums...)
		add("pkg:"+id, in, load(filepath.Join(f.afterR1, "signatures", id+".sig")))
	}
	req, _ := json.Marshal(map[string]any{"op": "verify", "pub_b64": base64.StdEncoding.EncodeToString(pub), "cases": cases})
	cmd := exec.Command(ruby, filepath.Join(repoRoot(t), "scripts", "release-sign", "crosscheck", "ed25519.rb"))
	cmd.Stdin = bytes.NewReader(req)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]bool
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	if len(res) != 6 {
		t.Fatalf("Ruby 应对 6 个用例给出结论: %v", res)
	}
	for name, ok := range res {
		if want := name != "manifest-tampered"; ok != want {
			t.Errorf("Ruby OpenSSL 对 %s 的验签结果 %v，期望 %v", name, ok, want)
		}
	}
}

// 离线签名工具是独立程序：独立 module、只依赖标准库，没有网络与其他仓库包。
func TestReleaseSignIsStandalone(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "scripts", "release-sign")
	gm := string(readFile(t, filepath.Join(dir, "go.mod")))
	if strings.Contains(gm, "require") || !strings.Contains(gm, "module github.com/wuekevin/lingedge/scripts/release-sign") {
		t.Errorf("release-sign 必须是无依赖的独立 module:\n%s", gm)
	}
	out := mustRun(t, dir, "go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", ".")
	for _, p := range strings.Fields(out) {
		if p != "github.com/wuekevin/lingedge/scripts/release-sign" {
			t.Errorf("release-sign 不得依赖标准库之外的包: %s", p)
		}
	}
	deps := mustRun(t, dir, "go", "list", "-deps", "-f", "{{.ImportPath}}", ".")
	if strings.Contains(deps, "\nnet/http\n") {
		t.Error("离线签名工具不得链接 net/http（只在离线机运行，不联网）")
	}
}
