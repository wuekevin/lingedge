package manifest

import (
	"crypto/ed25519"
	"encoding/json"
	"testing"
)

func TestPackageSumsSignatureWithRealKeys(t *testing.T) {
	e := newEnv(t)
	sums := []byte(sha256hex("payload") + "  payload/bin/lingedge-node\n")
	in, err := PackageSigInput("node", "1.2.3", "linux-arm64", sums)
	if err != nil {
		t.Fatal(err)
	}
	sig := sigDoc(e.active.id, ed25519.Sign(e.active.priv, in))
	at := t0
	if id, err := e.v.VerifyPackageSums(sig, sums, "node", "1.2.3", "linux-arm64", at); err != nil || id != e.active.id {
		t.Fatalf("应通过: %v", err)
	}
	// 吊销 / 未知 / 窗口外。
	rev := deriveKey("rel-test-x", "x")
	_, err = e.v.VerifyPackageSums(sigDoc(rev.id, ed25519.Sign(rev.priv, in)), sums, "node", "1.2.3", "linux-arm64", at)
	wantKind(t, err, KindKeyRevoked)
	st := deriveKey("rel-test-unknown", "stranger")
	_, err = e.v.VerifyPackageSums(sigDoc(st.id, ed25519.Sign(st.priv, in)), sums, "node", "1.2.3", "linux-arm64", at)
	wantKind(t, err, KindUnknownKey)
	_, err = e.v.VerifyPackageSums(sig, sums, "node", "1.2.3", "linux-arm64", t0.AddDate(-5, 0, 0))
	wantKind(t, err, KindKeyInvalid)
	// 版本/组件/平台混淆。
	for _, a := range [][3]string{{"server", "1.2.3", "linux-arm64"}, {"node", "1.2.4", "linux-arm64"}, {"node", "1.2.3", "linux-amd64"}} {
		if _, err := e.v.VerifyPackageSums(sig, sums, a[0], a[1], a[2], at); err == nil {
			t.Fatalf("%v 应失败", a)
		}
	}
	// 清单域签名不能充当包签名（反向亦然）。
	mraw := mf(t, nil)
	_, err = e.verify(mraw, sigDoc(e.active.id, ed25519.Sign(e.active.priv, append([]byte(PackageDomain), mraw...))), VerifyOptions{})
	wantKind(t, err, KindSignature)
	_, err = (&Verifier{}).VerifyPackageSums(sig, sums, "node", "1.2.3", "linux-arm64", at)
	wantKind(t, err, KindOptions)
}

func TestPackageMeta(t *testing.T) {
	good := map[string]any{"component": "node", "version": "0.1.0", "variant": "default", "platform": "linux-amd64",
		"built_at": "2026-11-15T00:00:00Z", "config_schema_version": 1, "files_count": 3, "unpacked_size": 1024, "x_future": 1}
	enc := func(m map[string]any) []byte { b, _ := json.Marshal(m); return b }
	meta, err := ParsePackageMeta(enc(good))
	if err != nil {
		t.Fatal(err)
	}
	if err := meta.Matches("node", "0.1.0", "default", "linux-amd64"); err != nil {
		t.Fatal(err)
	}
	for _, a := range [][4]string{{"server", "0.1.0", "default", "linux-amd64"}, {"node", "0.1.1", "default", "linux-amd64"}, {"node", "0.1.0", "ssr", "linux-amd64"}, {"node", "0.1.0", "default", "linux-arm64"}} {
		wantKind(t, meta.Matches(a[0], a[1], a[2], a[3]), KindPackage)
	}
	for _, f := range []string{"component", "version", "variant", "platform", "built_at", "config_schema_version", "files_count", "unpacked_size"} {
		m := map[string]any{}
		for k, v := range good {
			m[k] = v
		}
		delete(m, f)
		_, err := ParsePackageMeta(enc(m))
		if err == nil {
			t.Fatalf("缺 %s 应拒绝", f)
		}
		wantKind(t, err, KindPackage)
	}
	for name, mut := range map[string]func(m map[string]any){
		"version":  func(m map[string]any) { m["version"] = "1.0" },
		"platform": func(m map[string]any) { m["platform"] = "darwin-arm64" },
		"built_at": func(m map[string]any) { m["built_at"] = "x" },
		"files":    func(m map[string]any) { m["files_count"] = 0 },
		"size":     func(m map[string]any) { m["unpacked_size"] = -1 },
		"comp":     func(m map[string]any) { m["component"] = "No De" },
	} {
		m := map[string]any{}
		for k, v := range good {
			m[k] = v
		}
		mut(m)
		if _, err := ParsePackageMeta(enc(m)); err == nil {
			t.Fatalf("%s 非法应拒绝", name)
		}
	}
	_, err = ParsePackageMeta([]byte(`{"component":"node","component":"x"}`))
	wantKind(t, err, KindPackage)
	_, err = ParsePackageMeta(nil)
	wantKind(t, err, KindPackage)
}
