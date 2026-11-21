package manifest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/pkg/signing"
)

// 信任域①与②③的隔离（ADR-012 §2）：不同的签名域前缀、不共享代码路径与密钥。
func TestDomainPrefixesAreDistinctFromServerDomains(t *testing.T) {
	mine := []string{ManifestDomain, PackageDomain, KeyRotationDomain}
	theirs := []string{signing.RevisionDomain, signing.KeysetDomain}
	for _, a := range mine {
		for _, b := range theirs {
			if a == b || strings.HasPrefix(a, b) || strings.HasPrefix(b, a) {
				t.Fatalf("签名域前缀 %q 与 %q 冲突", a, b)
			}
		}
	}
	seen := map[string]bool{}
	for _, a := range mine {
		if seen[a] || !strings.HasSuffix(a, "\n") || !strings.HasPrefix(a, "LINGEDGE-") {
			t.Fatalf("域前缀 %q 重复或格式不符", a)
		}
		seen[a] = true
	}
	if ManifestDomain != "LINGEDGE-MANIFEST-V1\n" || PackageDomain != "LINGEDGE-PKG-V1\n" {
		t.Fatal("域前缀是冻结契约（CF-6），不得改动")
	}
}

// manifest 包的（非测试）依赖闭包不得包含 pkg/signing、Server 业务模块或任何签名私钥加载代码。
func TestManifestDoesNotDependOnServerSigning(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, p := range strings.Fields(string(out)) {
		switch {
		case strings.HasPrefix(p, "github.com/wuekevin/lingedge/pkg/signing"),
			strings.HasPrefix(p, "github.com/wuekevin/lingedge/pkg/secretbox"),
			strings.HasPrefix(p, "github.com/wuekevin/lingedge/internal/") && !strings.HasPrefix(p, "github.com/wuekevin/lingedge/internal/tools/"):
			t.Errorf("manifest 不得依赖 %s（信任域隔离）", p)
		}
	}
}

// 本包只验签：非测试源码不得出现任何签名/私钥/密钥生成调用（离线签名流水线属 Story 1.34，私钥离线）。
func TestPackageSourceHasNoSigningCode(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		n++
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"ed25519.Sign(", "ed25519.PrivateKey", "NewKeyFromSeed", "GenerateKey", "ed25519.GenerateKey"} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s 含签名/私钥相关代码 %q", f, bad)
			}
		}
	}
	if n < 6 {
		t.Fatalf("只扫描到 %d 个源文件，扫描无效", n)
	}
}
