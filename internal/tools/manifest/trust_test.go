package manifest

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseKeysFileRejections(t *testing.T) {
	k := deriveKey("rel-test-a", "a")
	good := k.entry(StatusActive, "", "")
	mut := func(f func(*Key)) []byte { kk := good; f(&kk); return keysDoc(t, nil, kk) }
	cases := map[string][]byte{
		"空":            nil,
		"非 JSON":       []byte("x"),
		"未知字段":         []byte(`{"schema_version":1,"keys":[],"revoked_key_ids":[],"extra":1}`),
		"schema 2":     []byte(`{"schema_version":2,"keys":[],"revoked_key_ids":[]}`),
		"key_id 大写":    mut(func(k *Key) { k.KeyID = "Rel" }),
		"key_id 空":     mut(func(k *Key) { k.KeyID = "" }),
		"alg rsa":      mut(func(k *Key) { k.Alg = "rsa" }),
		"公钥太短":         mut(func(k *Key) { k.PublicKey = "AAAA" }),
		"公钥非 base64":   mut(func(k *Key) { k.PublicKey = strings.Repeat("!", 44) }),
		"状态未知":         mut(func(k *Key) { k.Status = "maybe" }),
		"not_before 坏": mut(func(k *Key) { k.NotBefore = "soon" }),
		"窗口倒置":         mut(func(k *Key) { k.NotBefore = "2027-01-01T00:00:00Z"; k.NotAfter = "2026-01-01T00:00:00Z" }),
		"重复 key_id":    keysDoc(t, nil, good, good),
		"吊销 id 非法":     []byte(`{"schema_version":1,"keys":[],"revoked_key_ids":["Bad Id"]}`),
		"吊销 id 重复":     []byte(`{"schema_version":1,"keys":[],"revoked_key_ids":["a","a"]}`),
		"尾随内容":         append(keysDoc(t, nil, good), []byte(`{}`)...),
	}
	for name, doc := range cases {
		if _, err := ParseKeysFile(doc); err == nil {
			t.Fatalf("%s：应拒绝", name)
		}
	}
	if _, err := ParseKeysFile(keysDoc(t, nil, good)); err != nil {
		t.Fatal(err)
	}
}

func TestNewTrustSetRules(t *testing.T) {
	a, b := deriveKey("rel-a", "a"), deriveKey("rel-b", "b")
	f1, _ := ParseKeysFile(keysDoc(t, nil, a.entry(StatusActive, "", "")))
	// 同名同公钥重复：合并无冲突。
	f2, _ := ParseKeysFile(keysDoc(t, nil, a.entry(StatusActive, "", ""), b.entry(StatusActive, "", "")))
	ts, err := NewTrustSet(f1, f2)
	if err != nil {
		t.Fatal(err)
	}
	if ids := ts.IDs(); len(ids) != 2 {
		t.Fatalf("ids=%v", ids)
	}
	// 同名不同公钥：冲突，拒绝加载（keys.d 不得替换内置公钥）。
	evil, _ := ParseKeysFile(keysDoc(t, nil, deriveKey("rel-a", "evil").entry(StatusActive, "", "")))
	_, err = NewTrustSet(f1, evil)
	wantKind(t, err, KindTrustStore)
	// 全部被吊销 → 没有可用公钥，失败关闭。
	allRev, _ := ParseKeysFile(keysDoc(t, []string{"rel-a"}, a.entry(StatusActive, "", "")))
	_, err = NewTrustSet(allRev)
	wantKind(t, err, KindNoBuiltinKeys)
	// keys.d 文件里的 revoked_key_ids 会吊销内置公钥（吊销可经 keys.d 追加）。
	revOnly, _ := ParseKeysFile([]byte(`{"schema_version":1,"keys":[],"revoked_key_ids":["rel-a"]}`))
	ts2, err := NewTrustSet(f2, revOnly)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ts2.Lookup("rel-a")
	wantKind(t, err, KindKeyRevoked)
	if _, err := ts2.Lookup("rel-b"); err != nil {
		t.Fatal(err)
	}
}

func TestLoadTrustFromDisk(t *testing.T) {
	a, b := deriveKey("rel-a", "a"), deriveKey("rel-b", "b")
	builtin := keysDoc(t, nil, a.entry(StatusActive, "", ""))
	_, err := LoadTrust(nil, "")
	wantKind(t, err, KindNoBuiltinKeys)
	// keys.d 不存在：只用内置。
	ts, err := LoadTrust(builtin, filepath.Join(t.TempDir(), "none"))
	if err != nil || len(ts.IDs()) != 1 {
		t.Fatalf("%v %v", err, ts)
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "rel-b.json"), keysDoc(t, nil, b.entry(StatusActive, "", "")), 0o600)
	writeFile(t, filepath.Join(dir, "README"), []byte("ignored"), 0o600) // 非 .json 忽略
	ts, err = LoadTrust(builtin, dir)
	if err != nil || len(ts.IDs()) != 2 {
		t.Fatalf("%v", err)
	}
	// 损坏文件 → 整次加载失败（不静默忽略）。
	writeFile(t, filepath.Join(dir, "rel-c.json"), []byte("{broken"), 0o600)
	_, err = LoadTrust(builtin, dir)
	wantKind(t, err, KindTrustStore)
	_ = os.Remove(filepath.Join(dir, "rel-c.json"))
	// 内置文件本身非法 → schema 错误。
	_, err = LoadTrust([]byte("{}"), dir)
	wantKind(t, err, KindSchema)
	// group/other 可写的信任文件被拒绝。
	if runtime.GOOS != "windows" {
		writeFile(t, filepath.Join(dir, "rel-b.json"), keysDoc(t, nil, b.entry(StatusActive, "", "")), 0o666)
		_, err = LoadTrust(builtin, dir)
		wantKind(t, err, KindTrustStore)
		writeFile(t, filepath.Join(dir, "rel-b.json"), keysDoc(t, nil, b.entry(StatusActive, "", "")), 0o600)
		// 符号链接不被信任。
		target := filepath.Join(t.TempDir(), "elsewhere.json")
		writeFile(t, target, keysDoc(t, nil, deriveKey("rel-z", "z").entry(StatusActive, "", "")), 0o600)
		if err := os.Symlink(target, filepath.Join(dir, "rel-z.json")); err != nil {
			t.Fatal(err)
		}
		_, err = LoadTrust(builtin, dir)
		wantKind(t, err, KindTrustStore)
	}
}

func TestBuiltinKeysAbsentFailsClosed(t *testing.T) {
	// 仓库不含 builtin/keys.json（发行流水线注入）：BuiltinKeys 失败关闭，LoadTrust 也失败关闭。
	if _, err := os.Stat("builtin/keys.json"); err == nil {
		t.Skip("本地已注入 builtin/keys.json（发行构建环境）；本用例只验证未注入的构建")
	}
	_, err := BuiltinKeys()
	wantKind(t, err, KindNoBuiltinKeys)
}

func TestFileSequenceStore(t *testing.T) {
	dir := t.TempDir()
	s := FileSequenceStore{Path: filepath.Join(dir, "trust", "state")}
	if _, ok, err := s.Max("stable"); err != nil || ok {
		t.Fatalf("文件不存在应为「从未见过」: %v %v", ok, err)
	}
	if err := s.Advance("stable", 5); err != nil {
		t.Fatal(err)
	}
	if err := s.Advance("beta", 9); err != nil {
		t.Fatal(err)
	}
	if err := s.Advance("stable", 3); err != nil { // 只升不降
		t.Fatal(err)
	}
	if n, ok, _ := s.Max("stable"); !ok || n != 5 {
		t.Fatalf("stable=%d", n)
	}
	if n, ok, _ := s.Max("beta"); !ok || n != 9 {
		t.Fatalf("beta=%d", n)
	}
	fi, _ := os.Stat(s.Path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("state 权限 = %o", fi.Mode().Perm())
	}
	if di, _ := os.Stat(filepath.Dir(s.Path)); di.Mode().Perm() != 0o700 {
		t.Fatalf("trust 目录权限 = %o", di.Mode().Perm())
	}
	// 损坏的 state 必须失败关闭，而不是当作「从未见过」（那会让回滚防护被一次删改旁路）。
	for name, content := range map[string]string{
		"垃圾":     "garbage",
		"空":      "",
		"未知字段":   `{"schema_version":1,"max_sequence":{},"x":1}`,
		"schema": `{"schema_version":9,"max_sequence":{}}`,
		"负数":     `{"schema_version":1,"max_sequence":{"stable":-1}}`,
		"未知通道":   `{"schema_version":1,"max_sequence":{"nightly":4}}`,
		"null":   `{"schema_version":1,"max_sequence":null}`,
	} {
		writeFile(t, s.Path, []byte(content), 0o600)
		_, _, err := s.Max("stable")
		if err == nil {
			t.Fatalf("%s：应失败关闭", name)
		}
		wantKind(t, err, KindTrustStore)
		wantKind(t, s.Advance("stable", 100), KindTrustStore)
	}
	if runtime.GOOS != "windows" {
		writeFile(t, s.Path, []byte(`{"schema_version":1,"max_sequence":{"stable":1}}`), 0o666)
		_, _, err := s.Max("stable")
		wantKind(t, err, KindTrustStore)
	}
	// 与 Verifier 联动：state 损坏 → Verify 失败（而不是放行）。
	e := newEnv(t)
	writeFile(t, s.Path, []byte("garbage"), 0o600)
	e.v.Seen = s
	raw := mf(t, nil)
	_, err := e.verify(raw, signManifest(e.active, raw), VerifyOptions{})
	wantKind(t, err, KindTrustStore)
}

func TestFileSequenceStoreEndToEndRollback(t *testing.T) {
	e := newEnv(t)
	e.v.Seen = FileSequenceStore{Path: filepath.Join(t.TempDir(), "trust", "state")}
	keysDir := filepath.Join(t.TempDir(), "keys.d")
	raw10 := mf(t, nil)
	res, err := e.verify(raw10, signManifest(e.active, raw10), VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.v.Commit(res, keysDir); err != nil {
		t.Fatal(err)
	}
	raw9 := mf(t, func(m map[string]any) { m["sequence"] = 9 })
	_, err = e.verify(raw9, signManifest(e.active, raw9), VerifyOptions{})
	wantKind(t, err, KindRollback)
	raw10b := mf(t, func(m map[string]any) { m["generated_at"] = "2026-11-02T00:00:00Z" }) // 同 sequence 的重取
	if _, err := e.verify(raw10b, signManifest(e.active, raw10b), VerifyOptions{}); err != nil {
		t.Fatalf("相等 sequence 应允许: %v", err)
	}
}
