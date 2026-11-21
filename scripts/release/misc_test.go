package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/manifest"
)

func TestParseMigratePlan(t *testing.T) {
	good := `{"mode":"all","latest_version":13,"count":3,"has_irreversible":false,"migrations":[{"version":1,"class":"expand"},{"version":2,"class":"reversible"},{"version":13,"class":"expand"}]}`
	m, n, err := parseMigratePlan([]byte(good))
	if err != nil || n != 3 || m.SchemaFrom != "0000" || m.SchemaTo != "0013" || m.IrreversibleCount != 0 || !m.BackupRequired || strings.Join(m.Classes, ",") != "expand,reversible" {
		t.Fatalf("解析结果不符: %+v %d %v", m, n, err)
	}
	irr := `{"mode":"all","latest_version":2,"count":2,"has_irreversible":true,"migrations":[{"version":1,"class":"expand"},{"version":2,"class":"irreversible"}]}`
	if m, _, err := parseMigratePlan([]byte(irr)); err != nil || m.IrreversibleCount != 1 || !m.BackupRequired {
		t.Fatalf("不可逆应计数并要求备份: %+v %v", m, err)
	}
	for name, bad := range map[string]string{
		"非 JSON":              `nope`,
		"连库模式":                `{"mode":"pending","latest_version":1,"count":1,"migrations":[{"version":1,"class":"expand"}]}`,
		"空":                   `{"mode":"all","latest_version":0,"count":0,"migrations":[]}`,
		"未知分类":                `{"mode":"all","latest_version":1,"count":1,"migrations":[{"version":1,"class":"weird"}]}`,
		"has_irreversible 矛盾": `{"mode":"all","latest_version":1,"count":1,"has_irreversible":true,"migrations":[{"version":1,"class":"expand"}]}`,
		"count 矛盾":            `{"mode":"all","latest_version":1,"count":2,"migrations":[{"version":1,"class":"expand"}]}`,
	} {
		if _, _, err := parseMigratePlan([]byte(bad)); err == nil {
			t.Errorf("%s: 应拒绝", name)
		}
	}
}

func TestCompareBuildsDetectsMismatch(t *testing.T) {
	jobs := []buildJob{{"a", "linux-amd64"}, {"b", "linux-amd64"}, {"c", "linux-arm64"}}
	h := map[string]string{"linux-amd64/a": "11", "linux-amd64/b": "22", "linux-arm64/c": "33"}
	if _, bad := compareBuilds(jobs, h, h); bad != 0 {
		t.Fatal("相同哈希不应报告不一致")
	}
	h2 := map[string]string{"linux-amd64/a": "11", "linux-amd64/b": "XX", "linux-arm64/c": "33"}
	rows, bad := compareBuilds(jobs, h, h2)
	if bad != 1 || rows[1].Equal || !rows[0].Equal {
		t.Fatalf("应只有 b 不一致: %+v %d", rows, bad)
	}
	if _, bad := compareBuilds(jobs, h, map[string]string{"linux-amd64/a": "11"}); bad != 2 {
		t.Fatal("缺失一侧视为不一致")
	}
	if _, bad := compareBuilds(jobs, map[string]string{}, map[string]string{}); bad != 3 {
		t.Fatal("两侧都为空哈希不能算一致")
	}
}

func TestGoVersionMAndGoSBOM(t *testing.T) {
	sample := "bin/x: go1.27.4\n\tpath\texample.com/cmd/x\n\tmod\texample.com\t(devel)\t\n\tdep\tgithub.com/a/b\tv1.2.3\th1:AAAA=\n\tdep\tgolang.org/x/net\tv0.1.0\th1:BBBB=\n\t=>\texample.com/fork\tv0.0.1\th1:CCCC=\n\tbuild\t-trimpath=true\n\tbuild\tCGO_ENABLED=0\n"
	info, err := parseGoVersionM(sample)
	if err != nil || info.GoVersion != "go1.27.4" || len(info.Deps) != 2 || info.Deps[1].ReplacedBy != "example.com/fork@v0.0.1" || info.Build["CGO_ENABLED"] != "0" {
		t.Fatalf("解析不符: %+v %v", info, err)
	}
	if _, err := parseGoVersionM("x: not a go binary"); err == nil {
		t.Fatal("非 Go 二进制应报错")
	}
	// 真实二进制：用本测试自己（由 go test 构建的 Go 二进制）生成 SBOM，核对结构与确定性。
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	a, err := goSBOM(self, "release.test", "9.9.9", "linux-amd64", "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	ab, err := a.finish()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := goSBOM(self, "release.test", "9.9.9", "linux-amd64", "2026-01-01T00:00:00Z")
	bb, _ := b.finish()
	if !bytes.Equal(ab, bb) {
		t.Fatal("同一二进制两次生成的 SBOM 必须逐字节相同（可复现）")
	}
	var doc map[string]any
	if err := json.Unmarshal(ab, &doc); err != nil || doc["bomFormat"] != "CycloneDX" || doc["specVersion"] != "1.5" || !strings.HasPrefix(doc["serialNumber"].(string), "urn:uuid:") {
		t.Fatalf("SBOM 结构不符: %v", err)
	}
	comps := doc["components"].([]any)
	foundGo, foundDep := false, false
	for _, c := range comps {
		cm := c.(map[string]any)
		foundGo = foundGo || cm["type"] == "platform"
		foundDep = foundDep || strings.HasPrefix(cm["purl"].(string), "pkg:golang/") && cm["type"] == "library"
	}
	if !foundGo || len(comps) < 2 {
		t.Fatalf("SBOM 应含 toolchain 与依赖模块: %d 个组件", len(comps))
	}
	_ = foundDep
}

func TestNPMLockSBOM(t *testing.T) {
	integ := func(b byte) string {
		return "sha512-" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 64))
	}
	lock := "lockfileVersion: '9.0'\npackages:\n  vue@3.4.21:\n    resolution: {integrity: " + integ(1) + "}\n  '@scope/pkg@1.0.0(peer@2.0.0)':\n    resolution: {integrity: " + integ(2) + "}\n"
	dir := t.TempDir()
	p := filepath.Join(dir, "pnpm-lock.yaml")
	if err := os.WriteFile(p, []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := npmSBOM(p, "lingedge-website", "1.0.0", "2026-01-01T00:00:00Z")
	if err != nil || b == nil {
		t.Fatal(err)
	}
	out, _ := b.finish()
	s := string(out)
	for _, w := range []string{"pkg:npm/vue@3.4.21", "pkg:npm/%40scope/pkg@1.0.0", "SHA-512", "npm:lockfile-sha256"} {
		if !strings.Contains(s, w) {
			t.Errorf("npm SBOM 缺少 %q", w)
		}
	}
	if none, err := npmSBOM(filepath.Join(dir, "absent.yaml"), "x", "1.0.0", "t"); none != nil || err != nil {
		t.Fatalf("无 lockfile 应返回 (nil,nil)，由调用方如实记录: %v %v", none, err)
	}
	if err := os.WriteFile(p, []byte("packages: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := npmSBOM(p, "x", "1.0.0", "t"); err == nil {
		t.Fatal("缺 lockfileVersion 应报错")
	}
}

func TestCheckBaseURLAndRevoke(t *testing.T) {
	for _, ok := range []string{"https://releases.example.org", "https://r.example.org/lingedge/"} {
		if _, err := checkBaseURL(ok); err != nil {
			t.Errorf("%s 应合法: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://x.org", "https://u:" + "p@x.org", "https://x.org?a=1", "https://x.org#f", "ftp://x", "", "https://", "https://x.org/a b"} {
		if _, err := checkBaseURL(bad); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
	rv, err := parseRevoke("node@0.0.9=已知缺陷")
	if err != nil || rv.Component != "node" || rv.Version != "0.0.9" || rv.Reason != "已知缺陷" {
		t.Fatalf("%+v %v", rv, err)
	}
	for _, bad := range []string{"node", "node@x", "Node@1.0.0", "@1.0.0"} {
		if _, err := parseRevoke(bad); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
}

// 包是确定性的：同一 stage 与签名永远得到相同字节；META 在前；权限位与时间戳固定。
func TestWritePackageDeterministicAndOrdered(t *testing.T) {
	stage := t.TempDir()
	mk(t, stage, "payload/bin/x", "BIN", 0o755)
	mk(t, stage, "payload/share/a.txt", "A", 0o644)
	files, err := listPayload(filepath.Join(stage, "payload"))
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	var total int64
	for _, f := range files {
		h, n, _ := sha256File(f.Path)
		sb.WriteString(h + "  payload/" + f.Rel + "\n")
		total += n
	}
	sums := sb.String()
	meta := manifest.PackageMeta{Component: "server", Version: "1.0.0", Variant: "default", Platform: "linux-amd64", BuiltAt: "2026-01-01T00:00:00Z", FilesCount: 2, UnpackedSize: total}
	mb, _ := json.Marshal(meta)
	mk(t, stage, "META/package.json", string(mb), 0o644)
	mk(t, stage, "META/SHA256SUMS", sums, 0o644)
	a, err := writePackage(stage, []byte(`{"key_id":"k","alg":"ed25519","sig":"x"}`), "2026-01-01T00:00:00Z", []byte(sums))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := writePackage(stage, []byte(`{"key_id":"k","alg":"ed25519","sig":"x"}`), "2026-01-01T00:00:00Z", []byte(sums))
	if !bytes.Equal(a, b) {
		t.Fatal("同一输入两次打包必须逐字节相同")
	}
	zr, _ := gzip.NewReader(bytes.NewReader(a))
	tr := tar.NewReader(zr)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		if h.Typeflag != tar.TypeReg || h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.ModTime.Unix() != 1767225600 {
			t.Errorf("%s 头字段不确定/类型非法: %+v", h.Name, h)
		}
		if h.Name == "payload/bin/x" && h.Mode != 0o755 {
			t.Errorf("可执行文件模式应为 0755: %o", h.Mode)
		}
	}
	want := []string{"META/package.json", "META/SHA256SUMS", "META/SHA256SUMS.sig", "payload/bin/x", "payload/share/a.txt"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("条目顺序必须是 META 在前、payload 按路径排序: %v", names)
	}
	// 签名之后 stage 被改 → 拒绝。
	mk(t, stage, "payload/share/a.txt", "TAMPERED", 0o644)
	if _, err := writePackage(stage, []byte("{}"), "2026-01-01T00:00:00Z", []byte(sums)); err == nil {
		t.Fatal("stage 与已签名 SHA256SUMS 不一致必须失败")
	}
}

// 在线流水线没有任何签名代码（私钥只在离线机的 scripts/release-sign）。
func TestOnlinePipelineHasNoSigningCode(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	forbidden := []string{"ed25519.Sign", "ed25519.GenerateKey", "ed25519.NewKeyFromSeed", "ed25519.PrivateKey", "rsa.SignPKCS1v15", "ecdsa.Sign", "x509.ParsePKCS8PrivateKey", "ParsePKCS8", "SignPSS"}
	n := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		n++
		for _, w := range forbidden {
			if bytes.Contains(b, []byte(w)) {
				t.Errorf("%s 含签名/私钥相关调用 %q：在线流水线不得持有任何签名能力（ADR-012 §1.5）", f, w)
			}
		}
	}
	if n < 8 {
		t.Fatalf("只检查了 %d 个源文件，扫描范围异常", n)
	}
	// 依赖闭包里没有 pkg/signing 与 Server 业务模块（信任域隔离）。
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, p := range strings.Fields(string(out)) {
		if strings.HasPrefix(p, "github.com/wuekevin/lingedge/pkg/signing") ||
			strings.HasPrefix(p, "github.com/wuekevin/lingedge/internal/") && !strings.HasPrefix(p, "github.com/wuekevin/lingedge/internal/tools/") {
			t.Errorf("在线流水线不得依赖 %s", p)
		}
	}
}

// 发行产物字节里出现测试公钥（原始/base64/hex 任一形态）→ 构建失败（NFR-1）。
func TestScanBinaryForTestKeys(t *testing.T) {
	root := filepath.Join("..", "..")
	pubB64 := "P3pE/lvUPOLOFWJtF/1sxj4LU923fmJQU9Utz1K5ghc="
	raw, _ := base64.StdEncoding.DecodeString(pubB64)
	write := func(content []byte) string {
		p := filepath.Join(t.TempDir(), "bin")
		if err := os.WriteFile(p, content, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if err := scanBinaryForTestKeys(root, write(bytes.Repeat([]byte{0x90}, 4096))); err != nil {
		t.Fatalf("干净二进制不应失败: %v", err)
	}
	for name, content := range map[string][]byte{
		"base64": append(bytes.Repeat([]byte{1}, 100), []byte(pubB64)...),
		"hex":    []byte("xx" + strings.ToUpper(hex.EncodeToString(raw)) + "yy"),
		"raw":    append(append(bytes.Repeat([]byte{1}, 100), raw...), 9, 9),
	} {
		if err := scanBinaryForTestKeys(root, write(content)); err == nil {
			t.Errorf("%s 形态的测试公钥必须被发现", name)
		}
	}
}
