package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeygenSignVerifyRoundTrip(t *testing.T) {
	keyPath, keysJSON := newKey(t, "rel-e2e-a")
	fi, err := os.Stat(keyPath)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("私钥文件必须 0600: %v %v", fi, err)
	}
	// 不覆盖已有私钥。
	if code, _, _ := runCLI(t, "", "keygen", "-key-id", "rel-e2e-a", "-key-out", keyPath); code == 0 {
		t.Fatal("keygen 覆盖了已有私钥文件")
	}
	for round, mk := range map[int]func() (string, string){
		1: func() (string, string) { return makeRound1(t) },
		2: func() (string, string) {
			return makeRound2(t, fixtureManifest("https://releases.example.invalid/node/0.1.0/x.tar.gz", `"revoked_versions":[],`))
		},
	} {
		dir, list := mk()
		code, _, se := runCLI(t, "", "sign", "-key", keyPath, "-to-sign", list, "-yes")
		if code != 0 {
			t.Fatalf("第 %d 轮签名失败: %s", round, se)
		}
		if code, so, se := runCLI(t, "", "verify", "-keys", keysJSON, "-to-sign", list); code != 0 || !strings.Contains(so, "1 个签名") {
			t.Fatalf("复核失败: %d %s %s", code, so, se)
		}
		// 已存在则拒绝覆盖，-force 才覆盖。
		if code, _, _ := runCLI(t, "", "sign", "-key", keyPath, "-to-sign", list, "-yes"); code == 0 {
			t.Fatal("未加 -force 却覆盖了已有 .sig")
		}
		if code, _, _ := runCLI(t, "", "sign", "-key", keyPath, "-to-sign", list, "-yes", "-force"); code != 0 {
			t.Fatal("-force 应允许覆盖")
		}
		// 签名文件被改一位 → 复核失败。
		var sigPath string
		if round == 1 {
			sigPath = filepath.Join(dir, "signatures/node-0.1.0-linux-amd64.sig")
		} else {
			sigPath = filepath.Join(dir, "manifest.json.sig")
		}
		b, _ := os.ReadFile(sigPath)
		i := strings.Index(string(b), `"sig":"`) + 8
		b[i] ^= 1
		if b[i] == '"' || b[i] == '\\' {
			b[i] = 'A'
		}
		_ = os.WriteFile(sigPath, b, 0o644)
		if code, _, _ := runCLI(t, "", "verify", "-keys", keysJSON, "-to-sign", list); code == 0 {
			t.Fatal("篡改签名后复核仍通过")
		}
	}
}

func TestSignRefusesBadInputs(t *testing.T) {
	keyPath, _ := newKey(t, "rel-e2e-b")
	good := fixtureManifest("https://releases.example.invalid/node/0.1.0/x.tar.gz", `"revoked_versions":[],`)
	cases := map[string]struct {
		manifest []byte
		mutate   func(dir string)
	}{
		"http url":            {manifest: fixtureManifest("http://releases.example.invalid/x.tar.gz", `"revoked_versions":[],`)},
		"url 带凭据":             {manifest: fixtureManifest("https://u:"+"p@releases.example.invalid/x.tar.gz", `"revoked_versions":[],`)},
		"缺 revoked_versions":  {manifest: fixtureManifest("https://releases.example.invalid/x.tar.gz", ``)},
		"文件被改（SHA-256 不符）":    {manifest: good, mutate: func(d string) { writeFile(t, filepath.Join(d, "manifest.json"), append(good, ' ')) }},
		"文件换成符号链接":            {manifest: good, mutate: func(d string) { swapSymlink(t, d, "manifest.json") }},
		"空清单":                 {manifest: []byte(`{"schema_version":1,"channel":"stable","sequence":7,"generated_at":"2026-11-21T00:00:00Z","valid_until":"2027-02-19T00:00:00Z","revoked_versions":[],"components":{}}` + "\n")},
		"非 JSON":              {manifest: []byte("not json")},
		"valid_until 不晚于 gen": {manifest: []byte(strings.Replace(string(good), "2027-02-19", "2026-11-20", 1))},
	}
	for name, c := range cases {
		dir, list := makeRound2(t, c.manifest)
		if c.mutate != nil {
			c.mutate(dir)
		}
		if code, _, se := runCLI(t, "", "sign", "-key", keyPath, "-to-sign", list, "-yes"); code == 0 {
			t.Errorf("%s: 应拒绝签名", name)
		} else if _, err := os.Lstat(filepath.Join(dir, "manifest.json.sig")); err == nil {
			t.Errorf("%s: 拒绝后不应产生 .sig（%s）", name, se)
		}
	}
	// 路径逃逸 / 绝对路径 / 符号链接目录。
	dir, _ := makeRound1(t)
	for _, f := range []string{"../x", "/etc/passwd", "stage/../../x", ""} {
		ts := ToSign{Schema: toSignSchema, Round: 1, Items: []Item{{ID: "a", Kind: kindPackageSums, File: f, SHA256: strings.Repeat("0", 64), Sig: "s.sig", Component: "node", Version: "0.1.0", Platform: "any", FilesCount: 1}}}
		list := writeList(t, dir, "bad.json", ts)
		if code, _, _ := runCLI(t, "", "sign", "-key", keyPath, "-to-sign", list, "-yes"); code == 0 {
			t.Errorf("路径 %q 应被拒绝", f)
		}
	}
}

func swapSymlink(t *testing.T, dir, name string) {
	t.Helper()
	real := filepath.Join(dir, name+".real")
	if err := os.Rename(filepath.Join(dir, name), real); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
}

func TestSumsChecks(t *testing.T) {
	keyPath, _ := newKey(t, "rel-e2e-c")
	for name, sums := range map[string]string{
		"行格式非法":      "zz  payload/x\n",
		"缺结尾换行":      strings.TrimSuffix(fixtureSums, "\n"),
		"不在 payload": strings.Replace(fixtureSums, "payload/", "META/", 1),
	} {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "S"), []byte(sums))
		ts := ToSign{Schema: toSignSchema, Round: 1, Items: []Item{{ID: "a", Kind: kindPackageSums, File: "S", SHA256: sha([]byte(sums)), Sig: "S.sig", Component: "node", Version: "0.1.0", Platform: "any", FilesCount: 1}}}
		list := writeList(t, dir, "l.json", ts)
		if code, _, _ := runCLI(t, "", "sign", "-key", keyPath, "-to-sign", list, "-yes"); code == 0 {
			t.Errorf("%s: 应拒绝", name)
		}
	}
	// 注入：component 含换行会使签名输入有歧义。
	if _, err := packageSigInput("node\n0.1.0", "0.1.0", "any", nil); err == nil {
		t.Error("component 含换行必须被拒绝")
	}
}

func TestKeyFileProtections(t *testing.T) {
	keyPath, _ := newKey(t, "rel-e2e-d")
	_, list := makeRound1(t)
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, se := runCLI(t, "", "sign", "-key", keyPath, "-to-sign", list, "-yes"); code == 0 || !strings.Contains(se, "权限") {
		t.Fatalf("group/other 可读的私钥必须被拒绝: %d %s", code, se)
	}
	if err := os.Chmod(keyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link.json")
	if err := os.Symlink(keyPath, link); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runCLI(t, "", "sign", "-key", link, "-to-sign", list, "-yes"); code == 0 {
		t.Fatal("符号链接私钥必须被拒绝")
	}
	for _, bad := range []string{`{"format":"x","key_id":"a","alg":"ed25519","seed":"AAAA"}`, `{"format":"` + keyFileFormat + `","key_id":"A B","alg":"ed25519","seed":"` + strings.Repeat("A", 43) + `="}`, `{"extra":1}`} {
		p := filepath.Join(t.TempDir(), "k.json")
		if err := os.WriteFile(p, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadKey(p); err == nil {
			t.Errorf("非法私钥文件被接受: %s", bad)
		}
	}
}

func TestInteractiveConfirmation(t *testing.T) {
	keyPath, _ := newKey(t, "rel-e2e-e")
	dir, list := makeRound1(t)
	sig := filepath.Join(dir, "signatures/node-0.1.0-linux-amd64.sig")
	if code, _, _ := runCLI(t, "no\n", "sign", "-key", keyPath, "-to-sign", list); code == 0 {
		t.Fatal("未确认不得签名")
	}
	if _, err := os.Lstat(sig); err == nil {
		t.Fatal("未确认却产生了 .sig")
	}
	if code, _, se := runCLI(t, "sign\n", "sign", "-key", keyPath, "-to-sign", list); code != 0 {
		t.Fatalf("输入 sign 应通过: %s", se)
	}
	// 摘要不符（待签清单在带外被调包）→ 拒绝。
	dir2, list2 := makeRound1(t)
	if code, _, se := runCLI(t, "", "sign", "-key", keyPath, "-to-sign", list2, "-yes", "-expect-digest", strings.Repeat("0", 64)); code == 0 || !strings.Contains(se, "摘要不符") {
		t.Fatalf("摘要不符必须拒绝: %d %s", code, se)
	}
	if _, err := os.Lstat(filepath.Join(dir2, "signatures")); err == nil {
		t.Fatal("摘要不符却产生了输出")
	}
	_, _, digest, _ := func() (*ToSign, string, string, error) { return loadToSign(list2) }()
	if code, _, se := runCLI(t, "", "sign", "-key", keyPath, "-to-sign", list2, "-yes", "-expect-digest", strings.ToUpper(digest)); code != 0 {
		t.Fatalf("摘要相符应通过: %s", se)
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{{}, {"nope"}, {"sign"}, {"keygen"}, {"verify"}, {"pubkey"}, {"endorse"}} {
		if code, _, _ := runCLI(t, "", args...); code != 2 {
			t.Errorf("%v 应为用法错误 2，得到 %d", args, code)
		}
	}
	if code, so, _ := runCLI(t, "", "help"); code != 0 || !strings.Contains(so, "离线机") {
		t.Error("help 应输出用法")
	}
}
