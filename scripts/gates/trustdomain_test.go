package main

import (
	"strings"
	"testing"
)

func TestTrustDomainGateAcceptsRepository(t *testing.T) {
	fs, err := runTrustDomain(repoRoot(t), options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 0 {
		t.Fatalf("三个信任域门禁应通过，实际:\n%v", fs)
	}
}

// 负向样本：向 Server 代码路径加入一个加载发行私钥的函数/字面量/配置，门禁必须失败；合法写法不误伤。
func TestTrustDomainGateNegativeSamples(t *testing.T) {
	ok := map[string]string{"internal/platform/x/x.go": "package x\n\nfunc Sign(priv []byte, manifest []byte) {}\n\nfunc loadConfigSigningKey() {}\n\nconst note = \"release channel stable\"\n"}
	if fs := checkTrustDomain(ok, nil); len(fs) != 0 {
		t.Fatalf("合法代码（含 manifest 形参、config signing key、release channel）不应被误伤: %v", fs)
	}
	cases := []struct {
		name  string
		files map[string]string
		cfg   map[string]string
		rule  string
	}{
		{"加载发行私钥的函数", map[string]string{"internal/platform/x/x.go": "package x\n\nfunc loadReleasePrivateKey(path string) ([]byte, error) { return nil, nil }\n"}, nil, "trust-release-key-name"},
		{"方法名读取 manifest 签名私钥", map[string]string{"internal/cluster/a.go": "package cluster\n\ntype S struct{}\n\nfunc (S) ReadManifestSigningKey() {}\n"}, nil, "trust-release-key-name"},
		{"调用处出现（顺序颠倒的命名）", map[string]string{"cmd/lingedge-server/main.go": "package main\n\nfunc f() { _ = privateKeyForRelease() }\n"}, nil, "trust-release-key-name"},
		{"变量名", map[string]string{"pkg/signing/y.go": "package signing\n\nvar releaseSecretKey []byte\n"}, nil, "trust-release-key-name"},
		{"字面量：发行私钥文件名", map[string]string{"internal/auth/z.go": "package auth\n\nvar p = \"/etc/lingedge/release.key\"\n"}, nil, "trust-release-key-literal"},
		{"字面量：release_signing_key", map[string]string{"internal/auth/z.go": "package auth\n\nvar p = \"release_signing_key\"\n"}, nil, "trust-release-key-literal"},
		{"字面量：发行清单签名域前缀", map[string]string{"internal/node/z.go": "package node\n\nvar d = \"LINGEDGE-MANIFEST-V1\\n\"\n"}, nil, "trust-release-key-literal"},
		{"配置引用发行私钥", nil, map[string]string{"configs/server.example.yaml": "# 说明里可以提发行私钥离线\nsecrets:\n  release_signing_key_file: /x\n"}, "trust-release-key-config"},
		{"无法解析", map[string]string{"internal/x/bad.go": "package x\n\nfunc (\n"}, nil, "trust-parse"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := checkTrustDomain(c.files, c.cfg)
			found := false
			for _, f := range fs {
				if f.Rule == c.rule {
					found = true
				}
			}
			if !found {
				t.Fatalf("应报 [%s]，实际: %v", c.rule, fs)
			}
		})
	}
	// 配置里的注释行不误伤。
	if fs := checkTrustDomain(nil, map[string]string{"configs/server.example.yaml": "# release signing key 离线保管，Server 不持有\nrun_mode: self_hosted\n"}); len(fs) != 0 {
		t.Errorf("注释行不应被误伤: %v", fs)
	}
}

// 扫描范围：Server 代码路径内、Tools 与测试文件之外。
func TestTrustDomainScope(t *testing.T) {
	for rel, want := range map[string]bool{
		"internal/platform/signingkeys/service.go": true,
		"internal/node/api.go":                     true,
		"cmd/lingedge-server/main.go":              true,
		"pkg/signing/revision.go":                  true,
		"internal/tools/manifest/verify.go":        false, // Tools 验签，不持私钥，不在 Server 路径
		"internal/platform/x/x_test.go":            false,
		"cmd/lingedge-node/main.go":                false,
		"scripts/gates/trustdomain.go":             false,
		"internal/platform/x/readme.md":            false,
	} {
		if got := trustDomainScope(rel); got != want {
			t.Errorf("%s: scope=%v want %v", rel, got, want)
		}
	}
	if !strings.Contains(reTrustName.String(), "release") {
		t.Fatal("规则表达式异常")
	}
}
